package telemetry

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func sdkCompactionSyntheticBody(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sdkCompactionSyntheticVectors(t *testing.T) (response, wrapper string) {
	t.Helper()
	data, err := os.ReadFile("../prompt/testdata/sdk-compaction-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Response string                  `json:"response_text"`
		Wrappers []struct{ Text string } `json:"wrappers"`
	}
	if json.Unmarshal(data, &vectors) != nil || len(vectors.Wrappers) != 16 {
		t.Fatal("invalid synthetic compaction vectors")
	}
	return vectors.Response, vectors.Wrappers[12].Text
}

func TestSDKCompactionAdoptionDeliversOneOwnedResultAndDatadogMirror(t *testing.T) {
	for _, mode := range []string{"simple", "unknown_hook_history", "missing_tool_result", "rewritten_wire"} {
		t.Run(mode, func(t *testing.T) {
			f := newSDKResultFixture(t, true)
			f.auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
			body := []byte(sdkResultBody)
			main := beginInputTest(t, f, "claude-sonnet-5", body, body, 1, "")
			f.clock.Advance(time.Second)
			main.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), []claudeprompt.ToolObservation{{ID: "tool_compact", Name: "Read"}})
			f.clock.Advance(time.Second)
			main.facts.Prompt.FinishSuccess(f.clock.Now(), "tool_use", []string{"tool_compact"})
			main.ObserveResponse("req_before_compact", "tool_use")
			main.FinishSuccess(t.Context())

			response, wrapper := sdkCompactionSyntheticVectors(t)
			facts := testRequestFacts(f.session)
			facts.ClientRequestID, facts.Role, facts.ParentPromptID = uuid.NewString(), claudeprofile.RoleCompaction, main.facts.PromptID
			facts.StartedAt = f.clock.Now()
			helper := f.manager.BeginRequest(t.Context(), f.auth, facts)
			helperBody := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_compact","content":"PRIVATE_COMPACT_RESULT"},{"type":"text","text":"synthetic compact instruction"}]}]}`)
			if mode == "missing_tool_result" {
				helperBody = []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"synthetic compact instruction"}]}`)
			}
			helper.ObserveRequest(helperBody, nil)
			helper.ObserveHTTPResponse(http.StatusOK, nil)
			helper.ObserveResponsePayload(sdkCompactionSyntheticBody(t, map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": response}}}), false)
			f.clock.Advance(time.Second)
			helper.ObserveResponse("req_compact_summary", "end_turn")
			helper.FinishSuccess(t.Context())
			helper.FinishSuccess(t.Context())
			if got := main.facts.Prompt.Snapshot().SDK; got.PendingCompactions != 1 || got.SawCompact || got.NumTurns != 1 || got.APIDurationMS != 3000 {
				t.Fatalf("helper response alone changed adoption: %+v", got)
			}
			content := []map[string]any{{"type": "text", "text": wrapper}}
			if mode == "unknown_hook_history" {
				content = append([]map[string]any{{"type": "text", "text": "PRIVATE_UNOBSERVED_HOOK"}}, content...)
			}
			continued := sdkCompactionSyntheticBody(t, map[string]any{"model": "claude-sonnet-5", "messages": []map[string]any{{"role": "user", "content": content}}})
			nextFacts := testRequestFacts(f.session)
			nextFacts.ClientRequestID, nextFacts.ParentPromptID, nextFacts.StartedAt = uuid.NewString(), main.facts.PromptID, f.clock.Now()
			nextFacts.Input = claudeprompt.ObserveSubmission(continued, f.clock.Now())
			nextFacts.Prompt = f.tracker.Begin(claudeprompt.Input{AccountID: f.auth.ID, SessionID: f.session, ParentPromptID: main.facts.PromptID,
				ClientRequestID: nextFacts.ClientRequestID, Role: "main", StartedAt: f.clock.Now(), Body: continued})
			nextFacts.PromptID = nextFacts.Prompt.Identity().PromptID
			if nextFacts.PromptID != main.facts.PromptID || nextFacts.Prompt.Identity().StartsPrompt {
				t.Fatal("adopted summary lost explicit parent ownership")
			}
			next := f.manager.BeginRequest(t.Context(), f.auth, nextFacts)
			if nextFacts.Prompt.Snapshot().SDK.SawCompact {
				t.Fatal("selection was mistaken for query adoption")
			}
			wire := continued
			if mode == "rewritten_wire" {
				wire = []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"synthetic rewritten input"}]}`)
			}
			next.ObserveRequest(wire, nil)
			next.ObserveRequest(wire, nil)
			f.clock.Advance(time.Second)
			next.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
			f.clock.Advance(time.Second)
			next.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
			next.ObserveResponse("req_after_compact", "end_turn")
			next.FinishSuccess(t.Context())
			events := f.events(t)
			if len(events["tengu_input_prompt"]) != 1 || len(events["tengu_api_success"]) != 3 {
				t.Fatal("helper/adoption duplicated input or lost API success")
			}
			for i, source := range []string{"sdk", "compact", "sdk"} {
				if events["tengu_api_success"][i]["querySource"] != source {
					t.Fatal("compaction role changed the main query source")
				}
			}
			if mode != "simple" {
				wantReason := map[string]string{"unknown_hook_history": "unobserved-sdk-compaction-user-yields", "missing_tool_result": "unobserved-precompact-tool-result", "rewritten_wire": "unobserved-sdk-compaction-adoption"}[mode]
				if got := next.facts.Prompt.Snapshot().SDK.IncompleteReason; got != wantReason {
					t.Fatalf("compaction gap=%s want=%s", got, wantReason)
				}
				if len(events["tengu_sdk_result"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
					t.Fatal("unknown compaction yields emitted complete result telemetry")
				}
				for _, role := range []string{"sdk-event-logging", datadogLogsRole} {
					if factIssueEndpoint(t, f.manager, role).Status != "awaiting-sdk-prompt-facts" {
						t.Fatalf("unknown compaction yields were hidden for %s", role)
					}
				}
				return
			}
			if len(events["tengu_sdk_result"]) != 1 || len(events["tengu_sdk_ttft"]) != 1 {
				t.Fatal("owned compact result or TTFT was not emitted exactly once")
			}
			want := map[string]any{"num_turns": float64(3), "duration_api_ms": float64(5000), "tool_use_count": float64(0), "saw_compact": true, "cc_prompt_id": main.facts.PromptID}
			for key, value := range want {
				if events["tengu_sdk_result"][0][key] != value {
					t.Fatalf("result %s=%v want=%v", key, events["tengu_sdk_result"][0][key], value)
				}
			}
			if events["tengu_sdk_ttft"][0]["ttft_ms"] != float64(1000) {
				t.Fatal("compaction reset the first-assistant clock")
			}
			mirrors := 0
			for _, request := range f.doer.Requests() {
				if strings.Contains(string(request.Body), "PRIVATE_") || strings.Contains(string(request.Body), "synthetic summary") {
					t.Fatal("compact payload entered telemetry")
				}
				if !strings.Contains(request.URL, "http-intake.logs") {
					continue
				}
				var logs []map[string]any
				if err := json.Unmarshal(request.Body, &logs); err != nil {
					t.Fatal(err)
				}
				for _, entry := range logs {
					if entry["message"] != "tengu_sdk_result" {
						continue
					}
					mirrors++
					for key, value := range want {
						if key == "cc_prompt_id" {
							key = "prompt_id"
						}
						if entry[key] != value {
							t.Fatalf("Datadog %s differs from SDK result", key)
						}
					}
				}
			}
			if mirrors != 1 || next.sdkWorker.factIssueSnapshot() != nil {
				t.Fatal("known compaction result was not delivered cleanly")
			}
		})
	}
}

func TestSDKCompactionResponseCannotBypassSuccessAndVersionGates(t *testing.T) {
	for _, mode := range []string{"unobserved", "non-2xx", "malformed", "truncated", "version", "failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newSDKResultFixture(t)
			main := f.begin(t, []byte(sdkResultBody))
			facts := testRequestFacts(f.session)
			facts.ClientRequestID, facts.Role, facts.ParentPromptID = uuid.NewString(), claudeprofile.RoleCompaction, main.facts.PromptID
			helper := f.manager.BeginRequest(t.Context(), f.auth, facts)
			helper.ObserveRequest([]byte(sdkResultBody), nil)
			if mode != "unobserved" {
				status := http.StatusOK
				if mode == "non-2xx" {
					status = http.StatusBadGateway
				}
				helper.ObserveHTTPResponse(status, nil)
			}
			response, wrapper := sdkCompactionSyntheticVectors(t)
			payload := sdkCompactionSyntheticBody(t, map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": response}}})
			if mode == "malformed" {
				helper.ObserveResponsePayload([]byte("{"), false)
			}
			if mode == "truncated" {
				payload = []byte(`{"type":"message_start","message":{"role":"assistant"}}`)
			}
			helper.ObserveResponsePayload(payload, false)
			if mode == "version" {
				f.manager.bundle.CodeVersion = "unreviewed"
			}
			if mode == "failure" {
				helper.FinishFailure(t.Context(), "incomplete_stream", nil)
			} else {
				helper.FinishSuccess(t.Context())
			}
			// SDKCompactionSummary is opaque; check the public adoption boundary
			// rather than reflecting into the private fingerprint fields.
			var tracker claudeprompt.Tracker
			owner := tracker.Begin(claudeprompt.Input{AccountID: "a", SessionID: "s", ClientRequestID: "first", Role: "main", Body: []byte(sdkResultBody), StartedAt: f.clock.Now()})
			owner.FinishSuccess(f.clock.Now(), "tool_use", []string{"tool"})
			owner.RecordSDKCompactionSuccess("compact", "helper", 1, claudeprompt.CompletedSDKCompaction(claudeprompt.ObserveSDKCompactionInput([]byte(sdkResultBody)), helper.compactionSummary))
			next := tracker.Begin(claudeprompt.Input{AccountID: "a", SessionID: "s", ClientRequestID: "next", Role: "main", ParentPromptID: owner.Identity().PromptID,
				Body: sdkCompactionSyntheticBody(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": wrapper}}}), StartedAt: f.clock.Now()})
			if !next.Identity().StartsPrompt {
				t.Fatal("unsupported helper supplied a usable adoption fingerprint")
			}
		})
	}
}

func TestSDKCompactionSuccessKeepsUnresolvedDispositionLocal(t *testing.T) {
	f := newSDKResultFixture(t)
	f.auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	main := f.begin(t, []byte(sdkResultBody))
	facts := testRequestFacts(f.session)
	facts.ClientRequestID, facts.Role, facts.ParentPromptID = uuid.NewString(), claudeprofile.RoleCompaction, main.facts.PromptID
	facts.StartedAt = f.clock.Now()
	helper := f.manager.BeginRequest(t.Context(), f.auth, facts)
	helper.ObserveRequest([]byte(sdkResultBody), http.Header{})
	f.clock.Advance(100600 * time.Microsecond)
	helper.ObserveResponse("req_compact", "end_turn")
	helper.FinishSuccess(t.Context())
	helper.FinishSuccess(t.Context())
	before := main.facts.Prompt.Snapshot().SDK
	if before.APIDurationMS != 101 || before.PendingCompactions != 1 || before.SawCompact || before.NumTurns != 1 {
		t.Fatalf("helper completion fabricated adoption or duplicated duration: %+v", before)
	}
	if events := f.events(t); len(events["tengu_sdk_result"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
		t.Fatal("helper completed its parent prompt")
	}
	main.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
	f.clock.Advance(999400 * time.Microsecond)
	main.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	main.ObserveResponse("req_main", "end_turn")
	main.FinishSuccess(t.Context())
	sealed := main.facts.Prompt.SDKResultSnapshot()
	if sealed.CompleteFacts || sealed.IncompleteReason != "unobserved-sdk-compaction-disposition" || sealed.APIDurationMS != 1201 {
		t.Fatalf("unknown adoption corrupted known API duration: %+v", sealed)
	}
	if events := f.events(t); len(events["tengu_sdk_result"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
		t.Fatal("unresolved compact disposition emitted a complete SDK result")
	}
	for _, role := range []string{"sdk-event-logging", datadogLogsRole} {
		if status := factIssueEndpoint(t, f.manager, role); status.Status != "awaiting-sdk-prompt-facts" {
			t.Fatalf("unresolved compact disposition was hidden for %s: %+v", role, status)
		}
	}
	next := f.begin(t, []byte(sdkResultBody))
	f.clock.Advance(time.Second)
	next.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
	f.clock.Advance(time.Second)
	next.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	next.ObserveResponse("req_next", "end_turn")
	next.FinishSuccess(t.Context())
	events := f.events(t)
	if len(events["tengu_sdk_result"]) != 1 || len(events["tengu_sdk_ttft"]) != 1 {
		t.Fatal("an older unresolved disposition suppressed an unrelated prompt")
	}
	result := events["tengu_sdk_result"][0]
	if result["duration_api_ms"] != float64(2000) || result["num_turns"] != float64(1) || result["saw_compact"] != false {
		t.Fatalf("unrelated result inherited compact facts: %v", result)
	}
	for _, worker := range []*accountWorker{next.sdkWorker, next.auxiliaryWorkers[datadogLogsRole]} {
		if issue := worker.factIssueSnapshot(); issue == nil || issue.Unresolved != 1 {
			t.Fatalf("later successful delivery cleared the older unresolved prompt: %+v", issue)
		}
	}
	if main.facts.Prompt.SDKResultSnapshot() != sealed {
		t.Fatal("later activity rewrote the original result snapshot")
	}
}

func TestSDKCompactionCannotAcquireAnUnownedParent(t *testing.T) {
	for _, scope := range []string{"missing", "unknown", "session", "account"} {
		t.Run(scope, func(t *testing.T) {
			f := newSDKResultFixture(t)
			main := f.begin(t, []byte(sdkResultBody))
			facts := testRequestFacts(f.session)
			facts.ClientRequestID, facts.Role, facts.ParentPromptID = uuid.NewString(), claudeprofile.RoleCompaction, main.facts.PromptID
			facts.StartedAt = f.clock.Now()
			auth := f.auth
			switch scope {
			case "missing":
				facts.ParentPromptID = ""
			case "unknown":
				facts.ParentPromptID = uuid.NewString()
			case "session":
				facts.SessionID = uuid.NewString()
			case "account":
				auth = newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
			}
			helper := f.manager.BeginRequest(t.Context(), auth, facts)
			helper.ObserveRequest([]byte(sdkResultBody), http.Header{})
			f.clock.Advance(time.Second)
			helper.ObserveResponse("req_foreign_compact", "end_turn")
			helper.FinishSuccess(t.Context())
			if helper.sdkTitleParent() != nil {
				t.Fatal("compact helper acquired an unowned parent")
			}
			if got := main.facts.Prompt.Snapshot().SDK; got.APIDurationMS != 0 || got.PendingCompactions != 0 || got.SawCompact {
				t.Fatalf("unowned helper modified a main prompt: %+v", got)
			}
		})
	}
}
