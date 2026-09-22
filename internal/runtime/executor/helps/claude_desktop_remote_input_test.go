package helps

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/tidwall/gjson"
)

func remoteInputAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("input actor did not complete")
		var zero T
		return zero
	}
}

func TestRemoteInputSerialHistoryInterruptAndOwnedConfiguration(t *testing.T) {
	type request struct {
		ctx   context.Context
		model string
		body  []byte
	}
	requests := make(chan request, 8)
	finish := make(chan []byte, 8)
	outcomes := make(chan error, 8)
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "initial"}, func(ctx context.Context, model string, body []byte) ([]byte, error) {
		requests <- request{ctx, model, body}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case response := <-finish:
			return response, nil
		}
	}, nil, func(model string) (string, error) {
		if model == "bad" {
			return "", errors.New("unavailable model")
		}
		return model, nil
	}, func(err error) { outcomes <- err })
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	input := func(id string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "model": "unowned", "system": "unowned", "message": map[string]string{"role": "user", "content": id}})
		return raw
	}
	if err := a.Enqueue(t.Context(), input("first")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatal("unattached query executed queued input")
	}
	a.Start()
	first := remoteInputAwait(t, requests)
	if first.model != "initial" || gjson.GetBytes(first.body, "system").Exists() {
		t.Fatal("payload supplied request config")
	}
	if err := a.Enqueue(t.Context(), input("second")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatal("concurrent inference within one query")
	}
	if _, err := a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"set_model","model":"next","system_prompt":"owned system"}}`)); err != nil {
		t.Fatal(err)
	}
	response, err := a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"interrupt"}}`))
	if err != nil || len(response.(map[string]any)["still_queued"].([]string)) != 1 {
		t.Fatal("interrupt lost queue", response, err)
	}
	if err := remoteInputAwait(t, outcomes); !errors.Is(err, context.Canceled) {
		t.Fatal("interrupt did not cancel only current inference", err)
	}
	second := remoteInputAwait(t, requests)
	if second.model != "next" || gjson.GetBytes(second.body, "system").String() != "owned system" || len(gjson.GetBytes(second.body, "messages").Array()) != 2 {
		t.Fatal("serial input did not consume updated config/history")
	}
	finish <- []byte(`{"role":"assistant","content":[{"type":"text","text":"actual reply"}]}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"request":{"subtype":"set_model","model":9}}`, `{"request":{"subtype":"set_model","model":"bad"}}`, `{"request":{"subtype":"set_model","model":"wrong","system_prompt":null}}`, `{"request":{"subtype":"unimplemented"}}`} {
		if _, err := a.Control(t.Context(), json.RawMessage(body)); err == nil {
			t.Fatal("invalid control accepted", body)
		}
	}
	if err := a.Enqueue(t.Context(), input("third")); err != nil {
		t.Fatal(err)
	}
	third := remoteInputAwait(t, requests)
	if third.model != "next" || gjson.GetBytes(third.body, "messages.2.content.0.text").String() != "actual reply" {
		t.Fatal("real response/config was lost")
	}
	if err := a.Enqueue(t.Context(), input("discard")); err != nil {
		t.Fatal(err)
	}
	response, err = a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"interrupt","cancel_queued":true}}`))
	if err != nil || len(response.(map[string]any)["cancelled"].([]string)) != 1 {
		t.Fatal("cancel queue disposition", response, err)
	}
	remoteInputAwait(t, outcomes)
	a.Stop()
	remoteInputAwait(t, a.Done())
	if len(requests) != 0 {
		t.Fatal("cancelled queued input executed")
	}
	if err := a.Enqueue(t.Context(), input("late")); !errors.Is(err, context.Canceled) {
		t.Fatal("closed actor resurrected", err)
	}
}

func TestRemoteInputConfigurationRestorationAndFailedCommit(t *testing.T) {
	failure := errors.New("synthetic checkpoint failure")
	var persistedModel, persistedSystem string
	var persistErr error
	requests := make(chan []byte, 4)
	outcomes := make(chan error, 4)
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{
		Model: "restored", DefaultModel: "original", System: "saved system",
		Persist: func(_ context.Context, model, system string) error {
			if persistErr != nil {
				return persistErr
			}
			persistedModel, persistedSystem = model, system
			return nil
		}}, func(_ context.Context, _ string, body []byte) ([]byte, error) {
		requests <- body
		return []byte(`{"role":"assistant","content":"reply"}`), nil
	}, nil, nil, func(err error) { outcomes <- err })
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	a.Start()
	invoke := func(model, system string) {
		t.Helper()
		if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"next"}}`)); err != nil {
			t.Fatal(err)
		}
		body := remoteInputAwait(t, requests)
		if gjson.GetBytes(body, "model").String() != model || gjson.GetBytes(body, "system").String() != system {
			t.Fatal("input used an uncommitted configuration")
		}
		if err := remoteInputAwait(t, outcomes); err != nil {
			t.Fatal(err)
		}
	}
	invoke("restored", "saved system")
	persistErr = failure
	if _, err := a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"set_model","model":"new","system_prompt":"new system"}}`)); !errors.Is(err, failure) {
		t.Fatal("failed persistence returned success", err)
	}
	invoke("restored", "saved system")
	persistErr = nil
	if _, err := a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"set_model","model":null}}`)); err != nil {
		t.Fatal(err)
	}
	if persistedModel != "original" || persistedSystem != "saved system" {
		t.Fatal("default reset lost its creation model or inherited system")
	}
	invoke("original", "saved system")
	if _, err := a.Control(t.Context(), json.RawMessage(`{"request":{"subtype":"set_model","model":"new","system_prompt":"new system"}}`)); err != nil {
		t.Fatal(err)
	}
	invoke("new", "new system")
}

// A subagent's SendMessage to main enters the owned queue as a native meta
// input: the fresh-turn projection is the wire content of its own turn, the
// context carries the wrapped text and origin, and the later completion
// notification stays a separate task-notification turn.
func TestRemoteInputProjectsSubagentMessageAsMetaInput(t *testing.T) {
	type request struct {
		meta         *claudeprompt.SDKMetaInput
		notification *string
		body         []byte
	}
	requests := make(chan request, 8)
	responses := make(chan []byte, 8)
	outcomes := make(chan error, 8)
	childInvocations := make(chan claudetasks.Invocation, 4)
	childResponses := make(chan []byte, 4)
	agents := &claudetasks.Options{Scope: strings.Repeat("b", 64), DeliveryScope: "query-one", Store: NewClaudeDesktopAgentTaskStore(t.TempDir(), "owned-egress"),
		ResolveModel: func(parent, _, _ string) (string, error) { return parent, nil },
		Execute: func(ctx context.Context, invocation claudetasks.Invocation) ([]byte, error) {
			childInvocations <- invocation
			select {
			case response := <-childResponses:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "claude-sonnet-5", Agents: agents}, func(ctx context.Context, _ string, body []byte) ([]byte, error) {
		requests <- request{claudeprompt.SDKMetaInputFromContext(ctx), claudeprompt.SDKTaskNotificationFromContext(ctx), body}
		select {
		case response := <-responses:
			return response, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil, nil, func(err error) { outcomes <- err })
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	a.Start()
	if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"delegate"}}`)); err != nil {
		t.Fatal(err)
	}
	first := remoteInputAwait(t, requests)
	if first.meta != nil || first.notification != nil {
		t.Fatal("user input carried provenance")
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"launch","name":"Agent","input":{"description":"delegate","prompt":"work","name":"worker"}}],"stop_reason":"tool_use"}`)
	second := remoteInputAwait(t, requests)
	if second.meta != nil || second.notification != nil || !strings.Contains(gjson.GetBytes(second.body, "messages.2.content.0.content").String(), "agentId") {
		t.Fatalf("launch continuation: %s", second.body)
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"launched"}],"stop_reason":"end_turn"}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
	// The worker addresses main from its first tool round, then completes.
	childRun := remoteInputAwait(t, childInvocations)
	childResponses <- []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"send","name":"SendMessage","input":{"to":"main","message":"peer <report>"}}],"stop_reason":"tool_use"}`)
	childSecond := remoteInputAwait(t, childInvocations)
	childWire, _ := json.Marshal(childSecond.Messages)
	if !strings.Contains(string(childWire), `Message queued for the main conversation's next turn.`) {
		t.Fatalf("child did not receive the queued-main result: %s", childWire)
	}
	third := remoteInputAwait(t, requests)
	wrapped := "<agent-message from=\"worker\">\npeer <report>\n</agent-message>"
	origin := `{"kind":"peer","from":"worker","senderTaskId":"` + childRun.AgentID + `","name":"worker","body":"peer <report>"}`
	if third.meta == nil || third.notification != nil || third.meta.Text != wrapped || string(third.meta.Origin) != origin {
		t.Fatalf("meta provenance: %+v", third.meta)
	}
	wire := claudetasks.FreshTurnWire(claudetasks.MainDelivery{Text: wrapped, Origin: json.RawMessage(origin)})
	messages := gjson.GetBytes(third.body, "messages").Array()
	if len(messages) != 5 || messages[4].Get("role").String() != "user" || messages[4].Get("content").String() != wire || !strings.HasPrefix(wire, "Another Claude session sent a message:\n"+wrapped+"\n\n") {
		t.Fatalf("meta turn wire: %s", third.body)
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"acknowledged"}],"stop_reason":"end_turn"}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
	childResponses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"worker done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	fourth := remoteInputAwait(t, requests)
	if fourth.notification == nil || fourth.meta != nil || len(gjson.GetBytes(fourth.body, "messages").Array()) != 7 || !strings.Contains(gjson.GetBytes(fourth.body, "messages.6.content").String(), "<task-notification>") {
		t.Fatalf("completion notification turn: %s", fourth.body)
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"noted"}],"stop_reason":"end_turn"}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
}

const remoteInputReminderText = "<system-reminder>\nThe following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>"

// The owned request build runs filter, reminder and tools in the native order
// against the rows it produced, and refuses to send a request whose reminder
// has no user tail to merge into.
func TestRemoteInputOwnedRequestToolsOrderAndFailClosed(t *testing.T) {
	var calls []string
	stub := ClaudeDesktopOwnedRequestTools{
		FilterToolReferences: func(rows []json.RawMessage) []json.RawMessage {
			calls = append(calls, "filter:"+strconv.Itoa(len(rows)))
			filtered := make([]json.RawMessage, len(rows))
			for i, row := range rows {
				filtered[i] = json.RawMessage(strings.ReplaceAll(string(row), "tool_reference_dropped", "filtered"))
			}
			return filtered
		},
		DeferredToolsReminder: func(rows []json.RawMessage) string {
			calls = append(calls, "reminder:"+strconv.Itoa(len(rows)))
			for _, row := range rows {
				// Only the filter output carries "filtered": returning "" for it
				// proves the reminder is computed against the filtered rows.
				if strings.Contains(string(row), "deferred tools are now available") || strings.Contains(string(row), `"filtered"`) {
					return ""
				}
			}
			return remoteInputReminderText
		},
		RequestTools: func(rows []json.RawMessage) []json.RawMessage {
			calls = append(calls, "tools:"+strconv.Itoa(len(rows)))
			seen := "none"
			if strings.Contains(string(rows[len(rows)-1]), "deferred tools are now available") {
				seen = "announced"
			}
			raw, _ := json.Marshal(map[string]string{"name": seen})
			return []json.RawMessage{raw}
		},
	}
	prompt := json.RawMessage(`{"role":"user","content":"hi"}`)
	prepared, err := stub.Prepare([]json.RawMessage{prompt})
	if err != nil {
		t.Fatal(err)
	}
	// The row keeps its role/content shape; the merged content is the prompt
	// text block followed by the reminder text block (encoding/json escapes
	// the angle brackets when the row is re-encoded, the values are exact).
	merged := gjson.ParseBytes(prepared.Messages[0])
	announcedContent := func(content gjson.Result) bool {
		return len(content.Array()) == 2 && content.Get("0.type").String() == "text" && content.Get("0.text").String() == "hi" &&
			content.Get("1.type").String() == "text" && content.Get("1.text").String() == remoteInputReminderText
	}
	if prepared.Announced != 0 || merged.Get("role").String() != "user" || len(merged.Map()) != 2 || !announcedContent(merged.Get("content")) ||
		!announcedContent(gjson.ParseBytes(prepared.AnnouncedContent)) || string(prepared.Tools[0]) != `{"name":"announced"}` {
		t.Fatalf("merged request: %s tools=%s announced=%s", prepared.Messages[0], prepared.Tools, prepared.AnnouncedContent)
	}
	if strings.Join(calls, ",") != "filter:1,reminder:1,tools:1" {
		t.Fatal("request build order", calls)
	}
	// The filter output is what the reminder and the tools see; an already
	// announced history is left alone and the input slice is never mutated.
	calls = nil
	rows := []json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"tool_reference_dropped","tool_name":"X"}]}]}`)}
	prepared, err = stub.Prepare(rows)
	if err != nil || prepared.Announced != -1 || !strings.Contains(string(prepared.Messages[0]), `"filtered"`) || strings.Contains(string(rows[0]), `"filtered"`) || string(prepared.Tools[0]) != `{"name":"none"}` {
		t.Fatalf("filtered request: %+v err=%v", prepared, err)
	}
	if _, err := stub.Prepare([]json.RawMessage{prompt, json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"reply"}]}`)}); err == nil || !strings.Contains(err.Error(), `role "assistant"`) {
		t.Fatal("assistant tail must fail closed", err)
	}
	if _, err := stub.Prepare(nil); err == nil {
		t.Fatal("empty history must fail closed")
	}
	orphan := json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"x"}]}]}`)
	if _, err := stub.Prepare([]json.RawMessage{orphan}); !errors.Is(err, claudeprompt.ErrSDKCompactionContentUnknown) {
		t.Fatal("undefined wire merge must fail closed", err)
	}

	// A child's announced row is restored for later requests of the same
	// agent only while the row still matches; other agents announce themselves.
	var memo ClaudeDesktopDeferredAnnouncements
	first, err := memo.Prepare("agent-a", stub, []json.RawMessage{prompt})
	if err != nil || first.Announced != 0 {
		t.Fatal("child first request", err, first.Announced)
	}
	later := []json.RawMessage{prompt, json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"ToolSearch","input":{"query":"select:TaskStop"}}]}`), orphan}
	second, err := memo.Prepare("agent-a", stub, later)
	if err != nil || second.Announced != -1 || string(second.Messages[0]) != string(first.Messages[0]) || string(second.Messages[2]) != string(orphan) || string(second.Tools[0]) != `{"name":"none"}` {
		t.Fatalf("child continuation: %+v err=%v", second, err)
	}
	if string(later[0]) != string(prompt) {
		t.Fatal("restoration mutated the runtime's rows")
	}
	rewritten := []json.RawMessage{json.RawMessage(`{"role":"user","content":"other prompt"}`), later[1], orphan}
	if _, err := memo.Prepare("agent-a", stub, rewritten); !errors.Is(err, claudeprompt.ErrSDKCompactionContentUnknown) {
		t.Fatal("a rebuilt history must not receive a stale announcement", err)
	}
	if other, err := memo.Prepare("agent-b", stub, []json.RawMessage{prompt}); err != nil || other.Announced != 0 {
		t.Fatal("another agent announces its own pool", err)
	}
}

// A live actor announces the deferrable names once, after the prompt, keeps
// the merged row in its history, executes ToolSearch through the task runtime
// and advertises discovered tools deferred on every later request.
func TestRemoteInputAnnouncesDeferredToolsOnceAndDiscoversThroughToolSearch(t *testing.T) {
	requests := make(chan []byte, 8)
	responses := make(chan []byte, 8)
	outcomes := make(chan error, 8)
	agents := &claudetasks.Options{Scope: strings.Repeat("c", 64), DeliveryScope: "query-search", Store: NewClaudeDesktopAgentTaskStore(t.TempDir(), "owned-egress"),
		ResolveModel: func(parent, _, _ string) (string, error) { return parent, nil },
		Execute: func(context.Context, claudetasks.Invocation) ([]byte, error) {
			return nil, errors.New("this query launches no child")
		}}
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "claude-sonnet-5", Agents: agents}, func(ctx context.Context, _ string, body []byte) ([]byte, error) {
		requests <- body
		select {
		case response := <-responses:
			return response, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil, nil, func(err error) { outcomes <- err })
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	a.Start()
	if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"delegate"}}`)); err != nil {
		t.Fatal(err)
	}
	names := func(body []byte) string {
		var list []string
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			list = append(list, tool.Get("name").String())
		}
		return strings.Join(list, ",")
	}
	announcements := func(body []byte) int {
		count := 0
		for _, row := range gjson.GetBytes(body, "messages").Array() {
			for _, block := range row.Get("content").Array() {
				if block.Get("text").String() == remoteInputReminderText {
					count++
				}
			}
		}
		return count
	}
	ctx := claudetasks.DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}
	first := remoteInputAwait(t, requests)
	blocks := gjson.GetBytes(first, "messages.0.content").Array()
	if len(gjson.GetBytes(first, "messages").Array()) != 1 || len(blocks) != 2 || blocks[0].Get("text").String() != "delegate" || blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != remoteInputReminderText {
		t.Fatalf("first turn did not merge the reminder after the prompt: %s", first)
	}
	if names(first) != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("first turn tools: %s", names(first))
	}
	for i, want := range []json.RawMessage{claudetasks.PlaceholderDefinition(), claudetasks.ToolSearchDefinition(ctx)} {
		encoded, _ := json.Marshal(want)
		if got := gjson.GetBytes(first, "tools").Array()[i+1].Raw; got != string(encoded) {
			t.Fatalf("tool %d is not the native definition: %s", i+1, got)
		}
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"search_select","name":"ToolSearch","input":{"query":"select:SendMessage"}}],"stop_reason":"tool_use"}`)
	second := remoteInputAwait(t, requests)
	result := gjson.GetBytes(second, "messages.2.content.0")
	if result.Get("type").String() != "tool_result" || result.Get("tool_use_id").String() != "search_select" || result.Get("is_error").Exists() ||
		len(result.Get("content").Array()) != 1 || result.Get("content.0.type").String() != "tool_reference" || result.Get("content.0.tool_name").String() != "SendMessage" {
		t.Fatalf("select result: %s", second)
	}
	if names(second) != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" || !strings.HasSuffix(gjson.GetBytes(second, "tools.1").Raw, `,"defer_loading":true}`) || announcements(second) != 1 {
		t.Fatalf("discovered turn: tools=%s announcements=%d", names(second), announcements(second))
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"search_keyword","name":"ToolSearch","input":{"query":"nothing here"}}],"stop_reason":"tool_use"}`)
	third := remoteInputAwait(t, requests)
	if result := gjson.GetBytes(third, "messages.4.content.0"); result.Get("tool_use_id").String() != "search_keyword" || result.Get("content").Type != gjson.String || result.Get("content").String() != "No matching deferred tools found" || result.Get("is_error").Exists() {
		t.Fatalf("keyword miss: %s", third)
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
	// The persisted history carries the merged first row: the next turn
	// announces nothing new and still advertises the discovered tool.
	if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"again"}}`)); err != nil {
		t.Fatal(err)
	}
	fourth := remoteInputAwait(t, requests)
	if len(gjson.GetBytes(fourth, "messages").Array()) != 7 || gjson.GetBytes(fourth, "messages.0.content").Raw != gjson.GetBytes(first, "messages.0.content").Raw ||
		gjson.GetBytes(fourth, "messages.6.content").String() != "again" || announcements(fourth) != 1 || names(fourth) != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("later turn: %s", fourth)
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	if err := remoteInputAwait(t, outcomes); err != nil {
		t.Fatal(err)
	}
}

// A reminder that cannot merge into the trailing user row fails the turn
// before any model request instead of silently offering ToolSearch without
// the names it can fetch.
func TestRemoteInputFailsClosedWhenReminderCannotMerge(t *testing.T) {
	requests := make(chan []byte, 2)
	outcomes := make(chan error, 2)
	agents := &claudetasks.Options{Scope: strings.Repeat("d", 64), DeliveryScope: "query-closed", Store: NewClaudeDesktopAgentTaskStore(t.TempDir(), "owned-egress"),
		ResolveModel: func(parent, _, _ string) (string, error) { return parent, nil },
		Execute: func(context.Context, claudetasks.Invocation) ([]byte, error) {
			return nil, errors.New("this query launches no child")
		}}
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "claude-sonnet-5", Agents: agents}, func(_ context.Context, _ string, body []byte) ([]byte, error) {
		requests <- body
		return []byte(`{"role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn"}`), nil
	}, nil, nil, func(err error) { outcomes <- err })
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	a.Start()
	if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_orphan","content":[{"type":"text","text":"orphan"}]}]}}`)); err != nil {
		t.Fatal(err)
	}
	err := remoteInputAwait(t, outcomes)
	if !errors.Is(err, claudeprompt.ErrSDKCompactionContentUnknown) || !strings.Contains(err.Error(), "deferred-tools reminder") {
		t.Fatal("turn did not fail closed", err)
	}
	if len(requests) != 0 {
		t.Fatal("a request was sent without the announcement")
	}
}
