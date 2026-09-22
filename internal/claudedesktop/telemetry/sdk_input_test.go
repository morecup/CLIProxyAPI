package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/tidwall/gjson"
)

func beginInputTest(t *testing.T, f *sdkResultFixture, model string, input, wire []byte, attempt int, client string) *RequestSpan {
	t.Helper()
	if client == "" {
		client = uuid.NewString()
	}
	facts := testRequestFacts(f.session)
	facts.Model, facts.Attempt, facts.ClientRequestID, facts.StartedAt = model, attempt, client, f.clock.Now()
	facts.Input = claudeprompt.ObserveSubmission(input, f.clock.Now())
	facts.Prompt = f.tracker.Begin(claudeprompt.Input{AccountID: f.auth.ID, SessionID: f.session, ClientRequestID: client, Role: "main", Body: input, Attempt: attempt, StartedAt: f.clock.Now()})
	facts.PromptID = facts.Prompt.Identity().PromptID
	span := f.manager.BeginRequest(t.Context(), f.auth, facts)
	span.ObserveRequest(wire, http.Header{"Anthropic-Beta": {"synthetic-query-only"}})
	return span
}

func TestSDKInputDeliveryUsesOriginalScalarsAndModelBetas(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			f := newSDKResultFixture(t, true)
			input := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"continue"},{"type":"text","text":"🙂中文"}]}]}`)
			wire := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"PRIVATE_TRANSFORMED_TEXT"}],"output_config":{"effort":"high"}}`)
			if strings.Contains(model, "haiku") {
				wire = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"PRIVATE_TRANSFORMED_TEXT"}]}`)
			}
			span := beginInputTest(t, f, model, input, wire, 1, "")
			span.ObserveRequest(wire, nil)
			events := f.events(t)
			if len(events["tengu_input_prompt"]) != 1 {
				t.Fatalf("input count: %d", len(events["tengu_input_prompt"]))
			}
			got := events["tengu_input_prompt"][0]
			_, hasEffort := got["effort_level"]
			if hasEffort == strings.Contains(model, "haiku") {
				t.Fatal("input effort differs from model's selected configuration")
			}
			if got["prompt_length"] != float64(4) || got["prompt_index"] != float64(1) || got["is_keep_going"] != true || got["prompt_source"] != "sdk" || got["cc_prompt_id"] != span.facts.PromptID {
				t.Fatalf("input fields: %v", got)
			}
			for _, forbidden := range []string{"interrupted_message_id", "model", "betas"} {
				if _, exists := got[forbidden]; exists {
					t.Fatalf("unexpected metadata %s", forbidden)
				}
			}
			for _, request := range f.doer.Requests() {
				for _, event := range gjson.GetBytes(request.Body, "events").Array() {
					if event.Get("event_data.event_name").String() != "tengu_input_prompt" {
						continue
					}
					want, _ := f.manager.sdkProfile.InputBetaHeader(model)
					if event.Get("event_data.betas").String() != want || event.Get("event_data.model").String() != model {
						t.Fatal("input inherited query betas/model")
					}
					if strings.Index(string(request.Body), "tengu_input_prompt") > strings.Index(string(request.Body), "tengu_api_query") {
						t.Fatal("input queued after query")
					}
					decoded, _ := base64.StdEncoding.DecodeString(event.Get("event_data.additional_metadata").String())
					if strings.Contains(string(decoded), "PRIVATE_") || strings.Contains(string(decoded), "continue") {
						t.Fatal("input content leaked")
					}
				}
			}
		})
	}
}

func TestSDKInputJournalSurvivesPromptTTLButDoesNotInventResume(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	f.clock.Advance(2 * time.Hour)
	second := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if record, err := second.sdkWorker.readInputJournal(f.session); err != nil || record.Index != 2 || !record.Known {
		t.Fatalf("TTL reset journal: %+v %v", record, err)
	}
	if first.facts.Prompt.Identity().PromptID == second.facts.Prompt.Identity().PromptID {
		t.Fatal("test did not start another input")
	}
	// A restored worker sees the record, but a new logical app run has no
	// proven native restore rule. Do not persist an ever-increasing guessed index.
	f.manager.appSessionID = uuid.NewString()
	third := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if record, err := third.sdkWorker.readInputJournal(f.session); err != nil || record.Known || record.Index != 2 {
		t.Fatalf("restart guessed index: %+v %v", record, err)
	}
	if third.sdkWorker.factIssueSnapshot() == nil {
		t.Fatal("unknown resume remained healthy")
	}
	if got := len(f.events(t)["tengu_input_prompt"]); got != 2 {
		t.Fatalf("resume emitted input: %d", got)
	}
}

func TestSDKInputRetryAndToolContinuationDoNotIncrement(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "synthetic-call")
	first.facts.Prompt.FinishFailure()
	retry := beginInputTest(t, f, "claude-opus-5", body, body, 2, "synthetic-call")
	retry.facts.Prompt.FinishSuccess(f.clock.Now(), "tool_use", []string{"synthetic-tool"})
	tool := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"synthetic-tool","content":"synthetic"}]}]}`)
	next := beginInputTest(t, f, "claude-opus-5", tool, tool, 1, "")
	if next.facts.Prompt.Identity().StartsPrompt {
		t.Fatal("test tool owner missing")
	}
	if got := len(f.events(t)["tengu_input_prompt"]); got != 1 {
		t.Fatalf("retry/tool input count: %d", got)
	}
	if record, err := next.sdkWorker.readInputJournal(f.session); err != nil || record.Index != 1 || !record.Known {
		t.Fatalf("retry/tool changed journal: %+v %v", record, err)
	}
}

func TestSDKInputUnknownFactsAreVisibleAndDoNotBlockQuery(t *testing.T) {
	for _, kind := range []string{"missing", "slash", "history", "model", "retry"} {
		t.Run(kind, func(t *testing.T) {
			f := newSDKResultFixture(t, true)
			model, attempt := "claude-opus-5", 1
			input := []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`)
			switch kind {
			case "missing":
				input = nil
			case "slash":
				input = []byte(`{"messages":[{"role":"user","content":"/synthetic-unknown"}]}`)
			case "history":
				input = []byte(`{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"old"},{"role":"user","content":"new"}]}`)
			case "model":
				model = "unreviewed-model"
			case "retry":
				attempt = 2
			}
			wire := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"synthetic"}]}`)
			span := beginInputTest(t, f, model, input, wire, attempt, "")
			if issue := span.sdkWorker.factIssueSnapshot(); issue == nil || issue.Unresolved != 1 {
				t.Fatalf("missing input was healthy: %+v", issue)
			}
			events := f.events(t)
			if len(events["tengu_input_prompt"]) != 0 {
				t.Fatal("unknown facts emitted an input")
			}
			if attempt == 1 && len(events["tengu_api_query"]) != 1 {
				t.Fatal("missing input stopped query")
			}
		})
	}
}

func TestSDKInputJournalIsolationAndCorruption(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	originalSession := f.session
	f.session = uuid.NewString()
	other := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if record, err := other.sdkWorker.readInputJournal(f.session); err != nil || record.Index != 1 {
		t.Fatal("session inherited input index")
	}
	f.session = originalSession
	file := first.sdkWorker.inputJournalPath(f.session)
	// Test-only corruption must not be replaced with a fresh healthy counter.
	if err := os.WriteFile(file, []byte("synthetic-corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	retained, _ := os.ReadFile(file)
	if string(retained) != "synthetic-corruption" || corrupt.sdkWorker.factIssueSnapshot() == nil {
		t.Fatal("corruption overwritten or invisible")
	}
	f.auth = newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	accountB := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if record, err := accountB.sdkWorker.readInputJournal(f.session); err != nil || record.Index != 1 || !record.Known {
		t.Fatalf("account inherited journal: %+v %v", record, err)
	}
}

func TestSDKInputHelperAndCancellationBoundaries(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleSubagent, claudeprofile.RoleCompaction, claudeprofile.RoleCountTokens} {
		facts := first.facts
		facts.Role = role
		span := f.manager.BeginRequest(t.Context(), f.auth, facts)
		span.ObserveRequest(body, nil)
	}
	first.facts.Prompt.FinishFailure()
	first.facts.Prompt.FinalizeCancellation(f.clock.Now())
	first.FinishPromptFailure(context.Background(), "cancelled", context.Canceled)
	next := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if record, err := next.sdkWorker.readInputJournal(f.session); err != nil || !record.Known || record.Index != 2 {
		t.Fatal("headless cancellation changed the input journal")
	}
	events := f.events(t)["tengu_input_prompt"]
	if len(events) != 2 || next.sdkWorker.factIssueSnapshot() != nil {
		t.Fatal("helper/cancellation input boundary")
	}
	for _, event := range events {
		if _, present := event["interrupted_message_id"]; present {
			t.Fatal("headless input inherited interactive CLI interrupted identity")
		}
	}
	encoded, _ := json.Marshal(next.sdkWorker.factIssueSnapshot())
	if strings.Contains(string(encoded), f.session) {
		t.Fatal("diagnostic leaked scope")
	}
}

func TestSDKInputRealManagerRestartAndMissingFactStatus(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if err := f.manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	root, bundle := f.manager.root, f.manager.bundle
	f.manager.Close()
	f.manager = NewManager(Options{StatePath: root, Bundle: bundle, DoerFactory: func(string) HTTPDoer { return f.doer }, Now: f.clock.Now})
	t.Cleanup(f.manager.Close)
	f.tracker = claudeprompt.Tracker{}
	second := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if first.sdkWorker == second.sdkWorker {
		t.Fatal("worker was not restored")
	}
	record, err := second.sdkWorker.readInputJournal(f.session)
	if err != nil || record.Known || record.Index != 1 {
		t.Fatalf("restart fabricated restore/reset: %+v %v", record, err)
	}
	status := factIssueEndpoint(t, f.manager, "sdk-event-logging")
	if status.Status != "awaiting-sdk-prompt-facts" || !strings.Contains(status.Reason, "input submission") {
		t.Fatalf("input gap not visible: %+v", status)
	}
}

func TestSDKInputCancellationDoesNotHealUnknownJournal(t *testing.T) {
	f := newSDKResultFixture(t, true)
	wire := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	slash := []byte(`{"messages":[{"role":"user","content":"/synthetic-unknown"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", slash, wire, 1, "")
	first.facts.Prompt.FinishFailure()
	first.facts.Prompt.FinalizeCancellation(f.clock.Now())
	first.FinishPromptFailure(context.Background(), "cancelled", context.Canceled)
	next := beginInputTest(t, f, "claude-opus-5", wire, wire, 1, "")
	if record, err := next.sdkWorker.readInputJournal(f.session); err != nil || record.Known || record.Index != 0 {
		t.Fatal("cancellation healed an unrelated unknown input history")
	}
	if next.sdkWorker.factIssueSnapshot() == nil || len(f.events(t)["tengu_input_prompt"]) != 0 {
		t.Fatal("unknown input history became healthy after cancellation")
	}
}

func TestSDKInputJournalRejectsForeignSessionRecord(t *testing.T) {
	f := newSDKResultFixture(t, true)
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	record, err := first.sdkWorker.readInputJournal(f.session)
	if err != nil {
		t.Fatal(err)
	}
	f.session = uuid.NewString()
	if err := first.sdkWorker.writeInputJournal(f.session, record); err != nil {
		t.Fatal(err)
	}
	second := beginInputTest(t, f, "claude-opus-5", body, body, 1, "")
	if _, err := second.sdkWorker.readInputJournal(f.session); err == nil {
		t.Fatal("foreign session journal accepted")
	}
	if second.sdkWorker.factIssueSnapshot() == nil {
		t.Fatal("foreign journal remained healthy")
	}
}

func TestSDKInputOwnedNotificationUsesPreWireTextWithoutSuppressingNativeIndex(t *testing.T) {
	f := newSDKResultFixture(t, true)
	firstBody := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic human"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", firstBody, firstBody, 1, "")
	first.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	text := "<task-notification>synthetic completed 🙂</task-notification>"
	wire, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": []map[string]string{
		{"role": "user", "content": "synthetic human"}, {"role": "assistant", "content": "delegated"},
		{"role": "user", "content": claudeprompt.SDKTaskNotificationWire(text)},
	}})
	ctx := claudeprompt.WithSDKTaskNotification(t.Context(), text)
	facts := testRequestFacts(f.session)
	facts.Model, facts.Attempt, facts.ClientRequestID, facts.StartedAt = "claude-opus-5", 1, uuid.NewString(), f.clock.Now()
	facts.Input = claudeprompt.ObserveSubmissionContext(ctx, wire, f.clock.Now())
	facts.Prompt = f.tracker.Begin(claudeprompt.Input{AccountID: f.auth.ID, SessionID: f.session, ClientRequestID: facts.ClientRequestID,
		Role: "main", Body: wire, Attempt: 1, StartedAt: f.clock.Now(), TaskNotification: &text})
	facts.PromptID = facts.Prompt.Identity().PromptID
	notification := f.manager.BeginRequest(ctx, f.auth, facts)
	notification.ObserveRequest(wire, nil)
	notification.ObserveRequest(wire, nil)
	notification.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	next := beginInputTest(t, f, "claude-opus-5", firstBody, firstBody, 1, "")
	events := f.events(t)["tengu_input_prompt"]
	if len(events) != 3 || events[1]["prompt_index"] != float64(2) || events[2]["prompt_index"] != float64(3) ||
		events[1]["prompt_length"] != float64(facts.Input.Length) || events[1]["prompt_source"] != "sdk" {
		t.Fatal("owned notification changed native SDK input semantics", events)
	}
	if record, err := next.sdkWorker.readInputJournal(f.session); err != nil || !record.Known || record.Index != 3 {
		t.Fatal("notification tainted the native journal", record, err)
	}
}

// A subagent's SendMessage delivered to main is a native isMeta input: its
// input event measures the queued wrapped value, carries no prompt_index and
// does not advance the session's prompt journal.
func TestSDKInputOwnedMetaInputHasNoPromptIndexAndKeepsJournal(t *testing.T) {
	f := newSDKResultFixture(t, true)
	firstBody := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic human"}]}`)
	first := beginInputTest(t, f, "claude-opus-5", firstBody, firstBody, 1, "")
	first.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	wrapped := "<agent-message from=\"worker\">\nsynthetic peer 🙂\n</agent-message>"
	meta := claudeprompt.SDKMetaInput{Text: wrapped, Origin: json.RawMessage(`{"kind":"peer","from":"worker","senderTaskId":"a0123456789abcdef","name":"worker","body":"synthetic peer 🙂"}`)}
	wire, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": []map[string]string{
		{"role": "user", "content": "synthetic human"}, {"role": "assistant", "content": "delegated"},
		{"role": "user", "content": claudetasks.FreshTurnWire(claudetasks.MainDelivery{Text: meta.Text, Origin: meta.Origin})},
	}})
	ctx := claudeprompt.WithSDKMetaInput(t.Context(), meta)
	facts := testRequestFacts(f.session)
	facts.Model, facts.Attempt, facts.ClientRequestID, facts.StartedAt = "claude-opus-5", 1, uuid.NewString(), f.clock.Now()
	facts.Input = claudeprompt.ObserveSubmissionContext(ctx, wire, f.clock.Now())
	if !facts.Input.IsMeta || facts.Input.Length != len(utf16.Encode([]rune(wrapped))) {
		t.Fatal("meta input measured the projection instead of the queued value", facts.Input)
	}
	facts.Prompt = f.tracker.Begin(claudeprompt.Input{AccountID: f.auth.ID, SessionID: f.session, ClientRequestID: facts.ClientRequestID,
		Role: "main", Body: wire, Attempt: 1, StartedAt: f.clock.Now(), MetaInput: &meta})
	facts.PromptID = facts.Prompt.Identity().PromptID
	request := f.manager.BeginRequest(ctx, f.auth, facts)
	request.ObserveRequest(wire, nil)
	request.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	next := beginInputTest(t, f, "claude-opus-5", firstBody, firstBody, 1, "")
	events := f.events(t)["tengu_input_prompt"]
	if len(events) != 3 || events[0]["prompt_index"] != float64(1) || events[2]["prompt_index"] != float64(2) {
		t.Fatal("meta input advanced the native prompt index", events)
	}
	if _, indexed := events[1]["prompt_index"]; indexed || events[1]["prompt_length"] != float64(facts.Input.Length) || events[1]["prompt_source"] != "sdk" {
		t.Fatal("meta input event differs from native", events[1])
	}
	if record, err := next.sdkWorker.readInputJournal(f.session); err != nil || !record.Known || record.Index != 2 {
		t.Fatal("meta input tainted the native journal", record, err)
	}
}
