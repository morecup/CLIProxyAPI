package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestSDKCacheDiagnosisUsesRequestScopedWireFacts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role = claudeprofile.RoleMain
			depth := 3
			facts.QueryDepth, facts.QueryChainID = &depth, "88888888-8888-4888-8888-888888888888"
			span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"PRIVATE_SYSTEM","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"PRIVATE_PROMPT"}],"diagnostics":{"previous_message_id":"msg_previous"}}`), nil)
			span.ObserveHTTPResponse(200, http.Header{"request-id": {"req_actual"}, "Cookie": {"PRIVATE_COOKIE"}})
			message := `{"type":"message","content":[{"type":"text","text":"PRIVATE_RESPONSE"}],"diagnostics":{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":123}}}`
			payload := []byte(message)
			if stream {
				payload = []byte("event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":" + message + "}\r\n\r\n")
			}
			span.ObserveResponsePayload(payload, stream)
			span.ObserveResponsePayload(payload, stream)
			// The first chunk provides diagnostics but is not the captured
			// emission boundary: wait for the complete successful response.
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			for _, request := range doer.Requests() {
				if strings.HasPrefix(request.URL, "https://api.anthropic.com/") && capturedSDKEventNames(t, []recordedRequest{request})["tengu_prompt_cache_diagnosis_received"] != 0 {
					t.Fatal("diagnosis emitted at message_start before body completion")
				}
			}
			span.ObserveResponse("req_actual", "end_turn")
			span.FinishSuccess(context.Background())
			span.FinishSuccess(context.Background())
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			seen := 0
			for _, request := range doer.Requests() {
				if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
					continue
				}
				names := capturedSDKEventNames(t, []recordedRequest{request})
				seen += names["tengu_prompt_cache_diagnosis_received"]
				if names["tengu_prompt_cache_diagnosis_received"] == 0 {
					continue
				}
				if names["tengu_api_success"] != 1 || strings.Index(string(request.Body), "tengu_prompt_cache_diagnosis_received") > strings.Index(string(request.Body), "tengu_api_success") {
					t.Fatal("diagnosis must precede its API success after completion")
				}
				metadata := sdkMetadataForEvent(t, request.Body, "tengu_prompt_cache_diagnosis_received")
				for key, want := range map[string]any{"cc_prompt_id": facts.PromptID, "diagnosisType": "messages_changed", "is1hCacheTTL": true,
					"isCowork": false, "model": "claude-opus-5", "previousMessageId": "msg_previous", "queryDepth": float64(3), "querySource": "sdk", "requestId": "req_actual", "tokensMissed": float64(123)} {
					if metadata[key] != want {
						t.Errorf("%s = %v, want %v", key, metadata[key], want)
					}
				}
				encoded, _ := json.Marshal(metadata)
				if strings.Contains(string(encoded), "PRIVATE_") {
					t.Fatal("diagnosis metadata contains source content")
				}
			}
			if seen != 1 {
				t.Fatalf("diagnosis count = %d, want one", seen)
			}
		})
	}
}

func TestSDKCacheDiagnosisMissingFactsAndUnknownVariantsStayVisible(t *testing.T) {
	for _, tc := range []struct{ name, previous, requestID, diagnosis string }{
		{"missing-previous", "", "req_current", `{"type":"messages_changed","cache_missed_input_tokens":1}`},
		{"missing-request-id", "msg_previous", "", `{"type":"messages_changed","cache_missed_input_tokens":1}`},
		{"invalid-previous", "PRIVATE_PREVIOUS", "req_current", `{"type":"messages_changed","cache_missed_input_tokens":1}`},
		{"unknown-diagnosis", "msg_previous", "req_current", `{"type":"PRIVATE_REASON","cache_missed_input_tokens":1}`},
		{"missing-token-count", "msg_previous", "req_current", `{"type":"messages_changed"}`},
		{"negative-token-count", "msg_previous", "req_current", `{"type":"messages_changed","cache_missed_input_tokens":-1}`},
		{"fractional-token-count", "msg_previous", "req_current", `{"type":"messages_changed","cache_missed_input_tokens":1.5}`},
		{"non-numeric-token-count", "msg_previous", "req_current", `{"type":"messages_changed","cache_missed_input_tokens":"PRIVATE_COUNT"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role = claudeprofile.RoleMain
			span := manager.BeginRequest(context.Background(), auth, facts)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[],"diagnostics":{"previous_message_id":"`+tc.previous+`"}}`), nil)
			span.ObserveHTTPResponse(200, http.Header{"Request-Id": {tc.requestID}})
			span.ObserveResponsePayload([]byte(`{"type":"message","diagnostics":{"cache_miss_reason":`+tc.diagnosis+`}}`), false)
			span.FinishSuccess(context.Background())
			manager.BeginRequest(context.Background(), auth, facts)
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			for _, request := range doer.Requests() {
				if strings.HasPrefix(request.URL, "https://api.anthropic.com/") && capturedSDKEventNames(t, []recordedRequest{request})["tengu_prompt_cache_diagnosis_received"] != 0 {
					t.Fatal("missing response facts were invented")
				}
			}
			found := false
			for _, endpoint := range manager.Status().DeliveryEndpoints {
				if endpoint.Role == "sdk-event-logging" {
					found = endpoint.Status == "awaiting-response-facts" && endpoint.Reason != "" && !strings.Contains(endpoint.Reason, "PRIVATE_")
				}
			}
			if !found {
				t.Fatal("discarded response facts became silently healthy")
			}
		})
	}
}

func TestSDKCacheDiagnosisDoesNotReadToolOrUserContent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"system", `{"system":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, true},
		{"tool-block", `{"tools":[{"name":"Read","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, true},
		{"message-block", `{"messages":[{"content":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, true},
		{"tool-argument", `{"messages":[{"content":[{"type":"tool_use","input":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}]}]}`, false},
		{"short-ttl", `{"system":[{"cache_control":{"type":"ephemeral","ttl":"5m"}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sdkCacheDiagnosisRequestFromBody([]byte(tc.body)).oneHourTTL; got != tc.want {
				t.Fatalf("one-hour TTL = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestSDKCacheDiagnosisRequiresSuccessfulMainCompletion(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleSubagent, claudeprofile.RoleSecurityMonitor, claudeprofile.RoleCountTokens} {
		t.Run(string(role), func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role = role
			span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[],"diagnostics":{"previous_message_id":"msg_previous"}}`), nil)
			span.ObserveHTTPResponse(200, http.Header{"Request-Id": {"req_current"}})
			span.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"type":"message","diagnostics":{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":1}}}}`))
			if role == claudeprofile.RoleMain {
				span.FinishFailure(context.Background(), "cancelled", errors.New("synthetic cancellation"))
			} else {
				span.FinishSuccess(context.Background())
			}
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			for _, request := range doer.Requests() {
				if strings.HasPrefix(request.URL, "https://api.anthropic.com/") && capturedSDKEventNames(t, []recordedRequest{request})["tengu_prompt_cache_diagnosis_received"] != 0 {
					t.Fatal("cancelled or auxiliary response emitted a completed main diagnosis")
				}
			}
		})
	}
}
