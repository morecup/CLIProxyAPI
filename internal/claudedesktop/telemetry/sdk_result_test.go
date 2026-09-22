package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type sdkResultFixture struct {
	manager *Manager
	clock   *testClock
	doer    *testDoer
	auth    *cliproxyauth.Auth
	tracker claudeprompt.Tracker
	session string
}

func newSDKResultFixture(t *testing.T, includeInput ...bool) *sdkResultFixture {
	t.Helper()
	f := &sdkResultFixture{clock: &testClock{now: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}, doer: &testDoer{}, session: uuid.NewString()}
	f.manager = newTelemetryTestManager(t, t.TempDir(), f.clock, f.doer, func(bundle *claudeprofile.Bundle) {
		// Result-only fixtures begin below the submission boundary. Full input
		// tests explicitly retain the production input event and beta policy.
		if len(includeInput) == 0 || !includeInput[0] {
			delete(bundle.SDKTelemetry.Events, FactSDKInput)
			bundle.SDKTelemetry.InputBetas = nil
		}
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Batch.MaxEvents = 1000
		bundle.SDKTelemetry.Batch.JitterMinimum, bundle.SDKTelemetry.Batch.JitterMaximum = 1, 1
		bundle.AuxiliaryTelemetry.DatadogLogs.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.AuxiliaryTelemetry.DatadogLogs.Batch.JitterMinimum, bundle.AuxiliaryTelemetry.DatadogLogs.Batch.JitterMaximum = 1, 1
	})
	f.manager.endpointDoerFactory = func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
		return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			response, err := f.doer.Do(request)
			if err == nil && (role == "sdk-event-logging" || role == datadogLogsRole || role == datadogLogsBrowserRole) {
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
			}
			return response, err
		})
	}
	f.auth = newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	return f
}

func (f *sdkResultFixture) begin(t *testing.T, body []byte) *RequestSpan {
	t.Helper()
	facts := testRequestFacts(f.session)
	facts.ClientRequestID, facts.StartedAt = uuid.NewString(), f.clock.Now()
	facts.Prompt = f.tracker.Begin(claudeprompt.Input{AccountID: f.auth.ID, SessionID: f.session,
		ClientRequestID: facts.ClientRequestID, Role: "main", StartedAt: facts.StartedAt, Body: body})
	facts.PromptID = facts.Prompt.Identity().PromptID
	span := f.manager.BeginRequest(t.Context(), f.auth, facts)
	span.ObserveRequest(body, http.Header{})
	return span
}

func (f *sdkResultFixture) events(t *testing.T) map[string][]map[string]any {
	t.Helper()
	if err := f.manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]map[string]any)
	for _, request := range f.doer.Requests() {
		for _, event := range gjson.GetBytes(request.Body, "events").Array() {
			name := event.Get("event_data.event_name").String()
			if name == "" {
				continue
			}
			encoded := event.Get("event_data.additional_metadata")
			body := []byte(encoded.Raw)
			if !encoded.Exists() {
				body = []byte(event.Get("event_data.metadata").String())
			} else if encoded.Type == gjson.String {
				var err error
				body, err = base64.StdEncoding.DecodeString(encoded.String())
				if err != nil {
					t.Fatal(err)
				}
			}
			var metadata map[string]any
			if len(body) != 0 {
				if err := json.Unmarshal(body, &metadata); err != nil {
					t.Fatal(err)
				}
			}
			result[name] = append(result[name], metadata)
		}
	}
	return result
}

const sdkResultBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`

func TestSDKCanonicalToolYieldsReachResultAndDatadog(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "cancelled_after_results"}[cancelled], func(t *testing.T) {
			f := newSDKResultFixture(t)
			f.auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
			first := f.begin(t, []byte(sdkResultBody))
			f.clock.Advance(time.Second)
			first.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), []claudeprompt.ToolObservation{{ID: "a", Name: "Read"}, {ID: "b", Name: "mcp__synthetic__read"}})
			f.clock.Advance(time.Second)
			first.facts.Prompt.FinishSuccess(f.clock.Now(), "tool_use", []string{"a", "b"})
			first.ObserveResponse("req_tool_batch", "tool_use")
			first.FinishSuccess(t.Context())
			if events := f.events(t); len(events["tengu_sdk_result"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
				t.Fatal("tool-use response emitted a premature prompt result")
			}
			f.clock.Advance(time.Second)
			next := f.begin(t, []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"PRIVATE_RESULT_A"},{"type":"tool_result","tool_use_id":"b","content":"PRIVATE_RESULT_B","is_error":true}]}]}`))
			f.clock.Advance(time.Second)
			next.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
			f.clock.Advance(time.Second)
			wantTurns, wantAPI, wantSubtype, wantTTFT := float64(3), float64(4000), "success", 1
			if cancelled {
				next.facts.Prompt.FinishFailure()
				next.FinishFailure(t.Context(), "cancelled", context.Canceled)
				next.facts.Prompt.FinalizeCancellation(f.clock.Now())
				next.FinishPromptFailure(t.Context(), "cancelled", context.Canceled)
				wantTurns, wantAPI, wantSubtype, wantTTFT = 4, 2000, "terminated", 0
			} else {
				next.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
				next.ObserveResponse("req_after_tools", "end_turn")
				next.FinishSuccess(t.Context())
			}
			events := f.events(t)
			if len(events["tengu_sdk_result"]) != 1 || len(events["tengu_sdk_ttft"]) != wantTTFT {
				t.Fatal("canonical tool yields did not reach terminal SDK telemetry")
			}
			result := events["tengu_sdk_result"][0]
			for key, want := range map[string]any{"num_turns": wantTurns, "duration_api_ms": wantAPI, "subtype": wantSubtype, "is_error": cancelled, "tool_use_count": float64(2), "builtin_tool_calls": float64(1), "mcp_tool_calls": float64(1)} {
				if result[key] != want {
					t.Fatalf("SDK result %s=%v want=%v", key, result[key], want)
				}
			}
			if issue := next.sdkWorker.factIssueSnapshot(); issue != nil {
				t.Fatalf("known canonical tool yields were degraded: %+v", issue)
			}
			mirrors := 0
			for _, request := range f.doer.Requests() {
				if strings.Contains(string(request.Body), "PRIVATE_") {
					t.Fatal("tool contents leaked into telemetry")
				}
				if !strings.Contains(request.URL, "http-intake.logs") {
					continue
				}
				var logs []map[string]any
				if err := json.Unmarshal(request.Body, &logs); err != nil {
					t.Fatal(err)
				}
				for _, entry := range logs {
					if entry["message"] == "tengu_sdk_result" {
						mirrors++
						if entry["num_turns"] != wantTurns || entry["duration_api_ms"] != wantAPI || entry["subtype"] != wantSubtype || entry["prompt_id"] != first.facts.PromptID {
							t.Fatalf("Datadog mirror differs: %v", entry)
						}
					}
				}
			}
			if mirrors != 1 {
				t.Fatalf("SDK result Datadog mirror count=%d", mirrors)
			}
		})
	}
}

func TestSDKCancelledResultAndContinuedSuccessUseSeparateTerminalFacts(t *testing.T) {
	f := newSDKResultFixture(t)
	span := f.begin(t, []byte(sdkResultBody))
	f.clock.Advance(time.Second)
	span.ObserveFirstByte(f.clock.Now())
	span.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
	f.clock.Advance(32409 * time.Millisecond)
	span.facts.Prompt.FinishFailure()
	span.FinishFailure(t.Context(), "cancelled", context.Canceled)
	if before := f.events(t); len(before["tengu_sdk_result"]) != 0 || len(before["tengu_turn_end"]) != 0 {
		t.Fatal("provisional failure emitted a prompt terminal event")
	}
	span.facts.Prompt.FinalizeCancellation(f.clock.Now())
	span.FinishPromptFailure(t.Context(), "cancelled", context.Canceled)
	span.FinishPromptFailure(t.Context(), "cancelled", context.Canceled)
	events := f.events(t)
	for name, count := range map[string]int{"tengu_sdk_result": 1, "tengu_turn_end": 1, "tengu_sdk_ttft": 0, "tengu_api_success": 0, "desktop_ccd_message_cycle_outcome": 1} {
		if len(events[name]) != count {
			t.Fatalf("%s count=%d want=%d", name, len(events[name]), count)
		}
	}
	result, turn, renderer := events["tengu_sdk_result"][0], events["tengu_turn_end"][0], events["desktop_ccd_message_cycle_outcome"][0]
	for key, want := range map[string]any{"subtype": "terminated", "is_error": true, "num_turns": float64(2), "duration_api_ms": float64(0), "duration_ms": float64(33409), "tool_use_count": float64(0)} {
		if result[key] != want {
			t.Fatalf("cancelled %s=%v want=%v", key, result[key], want)
		}
	}
	if turn["terminal_reason"] != "aborted_streaming" || turn["is_error"] != false || renderer["cycle_health"] != "healthy" {
		t.Fatalf("terminal meanings were conflated: turn=%v renderer=%v", turn, renderer)
	}
	if _, exists := result["retry_status"]; exists {
		t.Fatal("non-retry result emitted retry status")
	}
	if issue := span.sdkWorker.factIssueSnapshot(); issue != nil {
		t.Fatalf("known zero API duration was degraded: %+v", issue)
	}
	continued := f.begin(t, []byte(sdkResultBody))
	f.clock.Advance(3370 * time.Millisecond)
	continued.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
	f.clock.Advance(1753 * time.Millisecond)
	continued.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
	continued.ObserveResponse("req_continued", "end_turn")
	continued.FinishSuccess(t.Context())
	events = f.events(t)
	if len(events["tengu_sdk_result"]) != 2 || len(events["tengu_sdk_ttft"]) != 1 {
		t.Fatalf("continuation events: %v", events)
	}
	success := events["tengu_sdk_result"][1]
	if success["subtype"] != "success" || success["is_error"] != false || success["num_turns"] != float64(1) || success["cc_prompt_id"] == result["cc_prompt_id"] || events["tengu_sdk_ttft"][0]["ttft_ms"] != float64(3370) {
		t.Fatalf("continuation inherited terminal facts: %v", success)
	}
}

func TestSDKNonCancellationAndMissingToolYieldsStayVisiblyDegraded(t *testing.T) {
	for _, kind := range []string{"timeout", "incomplete_stream", "cancelled_tool_drain"} {
		t.Run(kind, func(t *testing.T) {
			f := newSDKResultFixture(t)
			span := f.begin(t, []byte(sdkResultBody))
			if kind == "cancelled_tool_drain" {
				span.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), []claudeprompt.ToolObservation{{Name: "Read", ID: "tool"}})
			}
			span.facts.Prompt.FinishFailure()
			span.FinishFailure(t.Context(), kind, context.DeadlineExceeded)
			if kind == "cancelled_tool_drain" {
				span.facts.Prompt.FinalizeCancellation(f.clock.Now())
			} else {
				span.facts.Prompt.FinalizeFailure(f.clock.Now())
			}
			span.FinishPromptFailure(t.Context(), kind, context.DeadlineExceeded)
			events := f.events(t)
			if len(events["tengu_sdk_result"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
				t.Fatal("missing facts became a fabricated SDK result")
			}
			if issue := span.sdkWorker.factIssueSnapshot(); issue == nil || issue.Unresolved != 1 {
				t.Fatal("missing SDK facts were silently dropped")
			}
			if factIssueEndpoint(t, f.manager, "sdk-event-logging").Status != "awaiting-sdk-prompt-facts" {
				t.Fatal("management did not expose degraded facts")
			}
		})
	}
}

func TestSDKResultLedgerSelectsHelperPathAndDatadogMirrorsMetadata(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleSecurityMonitor} {
		t.Run(string(role), func(t *testing.T) {
			f := newSDKResultFixture(t)
			f.auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
			main := f.begin(t, []byte(sdkResultBody))
			facts := testRequestFacts(f.session)
			facts.ClientRequestID, facts.Role, facts.ParentPromptID = uuid.NewString(), role, main.facts.PromptID
			facts.StartedAt = f.clock.Now()
			helper := f.manager.BeginRequest(t.Context(), f.auth, facts)
			helper.ObserveRequest([]byte(sdkResultBody), http.Header{})
			f.clock.Advance(100600 * time.Microsecond)
			helper.ObserveResponse("req_helper", "end_turn")
			helper.FinishSuccess(t.Context())
			main.facts.Prompt.ObserveSDKAssistantMessage(f.clock.Now(), nil)
			f.clock.Advance(999400 * time.Microsecond)
			main.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
			main.ObserveResponse("req_main", "end_turn")
			main.FinishSuccess(t.Context())
			events := f.events(t)
			if len(events["tengu_sdk_result"]) != 1 {
				t.Fatalf("helper completed main or result was lost: %v", events)
			}
			result := events["tengu_sdk_result"][0]
			want := float64(1201)
			if role == claudeprofile.RoleSecurityMonitor {
				want = 1100
			}
			if result["duration_api_ms"] != want || result["num_turns"] != float64(1) {
				t.Fatalf("helper ledger path=%s result=%v", role, result)
			}
			found, foundTTFT := false, false
			for _, request := range f.doer.Requests() {
				if !strings.Contains(request.URL, "http-intake.logs") {
					continue
				}
				var logs []map[string]any
				if err := json.Unmarshal(request.Body, &logs); err != nil {
					t.Fatal(err)
				}
				for _, entry := range logs {
					if entry["message"] == "tengu_sdk_ttft" {
						foundTTFT = true
						if entry["ttft_ms"] != float64(101) || entry["prompt_id"] != main.facts.PromptID {
							t.Fatalf("Datadog TTFT diverged: %v", entry)
						}
					}
					if entry["message"] != "tengu_sdk_result" {
						continue
					}
					found = true
					if entry["duration_api_ms"] != want || entry["prompt_id"] != main.facts.PromptID || entry["subtype"] != "success" {
						t.Fatalf("Datadog result diverged: %v", entry)
					}
				}
			}
			if !found || !foundTTFT {
				t.Fatal("Datadog result or TTFT was not delivered")
			}
		})
	}
}
