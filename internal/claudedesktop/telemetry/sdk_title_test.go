package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestSDKTitleGeneratedRequiresOwnedValidatedCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, output, stop                                                            string
		missingParent, otherSession, otherAccount, expired, failed, stream, truncated bool
		want                                                                          int
		wantFailure                                                                   bool
	}{
		{name: "json-title", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", want: 1},
		{name: "stream-title", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", stream: true, want: 1},
		{name: "missing-parent", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", missingParent: true},
		{name: "other-session", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", otherSession: true},
		{name: "other-account", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", otherAccount: true},
		{name: "expired-parent", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", expired: true},
		{name: "plain-text-is-not-title-json", output: `PRIVATE_TITLE`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "empty-title", output: `{"title":" "}`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "wrong-type", output: `{"title":42}`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "missing-title", output: `{}`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "null-title", output: `{"title":null}`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "non-object", output: `["PRIVATE_TITLE"]`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "stream-empty-title", output: `{"title":" "}`, stop: "end_turn", stream: true, want: 1, wantFailure: true},
		{name: "javascript-bom-whitespace", output: `{"title":"\ufeff\u00a0"}`, stop: "end_turn", want: 1, wantFailure: true},
		{name: "javascript-nel-is-not-whitespace", output: `{"title":"\u0085"}`, stop: "end_turn", want: 1},
		{name: "refusal-does-not-gate-title-schema", output: `{"title":"PRIVATE_TITLE"}`, stop: "refusal", want: 1},
		{name: "max-tokens-complete-json", output: `{"title":"PRIVATE_TITLE"}`, stop: "max_tokens", want: 1},
		{name: "max-tokens-invalid-json", output: `{"title":`, stop: "max_tokens", want: 1, wantFailure: true},
		{name: "failure-does-not-emit-success", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", failed: true},
		{name: "truncated-stream", output: `{"title":"PRIVATE_TITLE"}`, stop: "end_turn", stream: true, truncated: true},
		{name: "bounded-text", output: strings.Repeat("x", maxTitleResponseBytes+1), stop: "end_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			main := testRequestFacts(uuid.NewString())
			main.Role, main.Model = claudeprofile.RoleMain, "claude-opus-5"
			owner := manager.BeginRequest(t.Context(), auth, main)
			owner.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[]}`), http.Header{"Anthropic-Beta": {"main-private-beta"}})
			// A concurrent newer prompt must not replace the explicitly named owner.
			newer := main
			newer.PromptID, newer.ClientRequestID = uuid.NewString(), uuid.NewString()
			manager.BeginRequest(t.Context(), auth, newer).ObserveRequest([]byte(`{"model":"claude-sonnet-5","messages":[]}`), http.Header{})
			facts := main
			facts.PromptID, facts.ClientRequestID = uuid.NewString(), uuid.NewString()
			facts.Role, facts.Model, facts.ParentPromptID = claudeprofile.RoleTitle, "claude-haiku-4-5-20251001", main.PromptID
			if tc.missingParent {
				facts.ParentPromptID = ""
			}
			if tc.otherSession {
				facts.SessionID = uuid.NewString()
			}
			if tc.otherAccount {
				auth = newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
			}
			if tc.expired {
				clock.Advance(time.Hour + time.Second)
			}
			span := manager.BeginRequest(t.Context(), auth, facts)
			span.ObserveRequest([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[]}`), http.Header{"Anthropic-Beta": {"title-beta"}})
			span.ObserveHTTPResponse(200, http.Header{"Request-Id": {"req_synthetic_title"}})
			textJSON, _ := json.Marshal(tc.output)
			if tc.stream {
				lines := []string{`{"type":"message_start","message":{"type":"message","role":"assistant"}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(textJSON) + `}}`, `{"type":"content_block_stop","index":0}`, `{"type":"message_delta","delta":{"stop_reason":"` + tc.stop + `"}}`}
				if !tc.truncated {
					lines = append(lines, `{"type":"message_stop"}`)
				}
				for _, line := range lines {
					span.ObserveStreamLine([]byte("data: " + line))
				}
			} else {
				span.ObserveResponsePayload([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":`+string(textJSON)+`}],"stop_reason":"`+tc.stop+`"}`), false)
			}
			if tc.failed {
				span.FinishFailure(t.Context(), "cancelled", errors.New("PRIVATE_ERROR"))
			} else {
				span.FinishSuccess(t.Context())
			}
			span.FinishSuccess(t.Context())
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, request := range doer.Requests() {
				if strings.Contains(string(request.Body), "PRIVATE_TITLE") {
					t.Fatal("title text persisted in telemetry")
				}
				var batch struct {
					Events []sdkEventWrapper `json:"events"`
				}
				if json.Unmarshal(request.Body, &batch) != nil {
					continue
				}
				for _, event := range batch.Events {
					if event.EventData.EventName != "tengu_session_title_generated" {
						continue
					}
					count++
					if event.EventData.Model != main.Model || event.EventData.Betas != sdkTitleLifecycleBetas || event.EventData.SessionID != main.SessionID {
						t.Fatalf("wrong title dimensions: %+v", event.EventData)
					}
					metadata := sdkMetadataForEvent(t, request.Body, "tengu_session_title_generated")
					if len(metadata) != 3 || metadata["cc_prompt_id"] != main.PromptID || metadata["success"] != !tc.wantFailure {
						t.Fatalf("wrong title metadata: %v", metadata)
					}
				}
			}
			if count != tc.want {
				t.Fatalf("titles=%d, want=%d", count, tc.want)
			}
			if span.titleResponse.text != nil {
				t.Fatal("response text retained after completion")
			}
			if tc.want == 1 {
				span.sdkWorker.factIssueMu.Lock()
				hasTitleIssue := false
				for _, kind := range span.sdkWorker.factIssues {
					hasTitleIssue = hasTitleIssue || kind == factIssueTitle
				}
				span.sdkWorker.factIssueMu.Unlock()
				if hasTitleIssue {
					t.Fatal("known title outcome was reported as missing response facts")
				}
			}
			if tc.want == 0 && !tc.failed {
				found := false
				for _, status := range manager.Status().DeliveryEndpoints {
					if status.Role == "sdk-event-logging" && status.Status == "awaiting-response-facts" {
						found = true
					}
				}
				if !found {
					t.Fatal("missing title facts not exposed")
				}
			}
		})
	}
}

func TestSDKTitleParentRetentionIsBounded(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	manager := newSDKPreparationTestManager(t, clock, &testDoer{})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(t.Context(), auth, testRequestFacts(uuid.NewString()))
	for range maxPromptIssues + 2 {
		facts := span.facts
		facts.Role, facts.PromptID = claudeprofile.RoleMain, uuid.NewString()
		span.observeSDKParent(facts)
		clock.Advance(time.Millisecond)
	}
	if len(manager.sdkParents) != maxPromptIssues {
		t.Fatalf("parent count=%d", len(manager.sdkParents))
	}
}

func TestSDKTitleTextMatchesNativeOutcomeVectors(t *testing.T) {
	data, errRead := os.ReadFile("testdata/sdk-title-outcome-native.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var fixture struct {
		Cases []struct {
			Name    string `json:"name"`
			JSON    string `json:"json"`
			Success bool   `json:"success"`
			Known   bool   `json:"known"`
		} `json:"cases"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Cases) != 29 {
		t.Fatal("invalid synthetic native title vectors")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			response := sdkTitleResponse{text: []byte(tc.JSON), blockDone: true, stopReason: "end_turn"}
			response.finish()
			if response.outcomeKnown != tc.Known || response.valid != tc.Success || !response.complete || response.text != nil {
				t.Fatalf("title outcome known=%v success=%v, want %v/%v", response.outcomeKnown, response.valid, tc.Known, tc.Success)
			}
		})
	}
}

func TestSDKTitleReturnedAssistantMatchesNativeContentVectors(t *testing.T) {
	data, errRead := os.ReadFile("testdata/sdk-title-outcome-native.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var fixture struct {
		Cases []struct {
			Name          string            `json:"name"`
			Content       []json.RawMessage `json:"content"`
			Success       bool              `json:"success"`
			StreamSuccess bool              `json:"stream_success"`
		} `json:"content_cases"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Cases) != 5 {
		t.Fatal("invalid native content fixture")
	}
	for _, tc := range fixture.Cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.Name, stream), func(t *testing.T) {
				clock := &testClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
				manager := newSDKPreparationTestManager(t, clock, &testDoer{})
				facts := testRequestFacts(uuid.NewString())
				facts.Role = claudeprofile.RoleTitle
				span := manager.BeginRequest(t.Context(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
				span.ObserveRequest([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[]}`), http.Header{})
				span.ObserveHTTPResponse(200, http.Header{})
				if !stream {
					content, _ := json.Marshal(tc.Content)
					span.ObserveResponsePayload([]byte(`{"type":"message","role":"assistant","stop_reason":"end_turn","content":`+string(content)+`}`), false)
				} else {
					span.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"role":"assistant","content":[]}}`))
					for index, block := range tc.Content {
						var decoded map[string]any
						if json.Unmarshal(block, &decoded) != nil {
							t.Fatal("invalid content")
						}
						text, isText := decoded["text"].(string)
						if isText {
							decoded["text"] = ""
						}
						encoded, _ := json.Marshal(decoded)
						span.ObserveStreamLine([]byte(fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}", index, encoded)))
						if isText {
							span.ObserveStreamLine([]byte(fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}", index, text)))
						}
						span.ObserveStreamLine([]byte(fmt.Sprintf("data: {\"type\":\"content_block_stop\",\"index\":%d}", index)))
					}
					span.ObserveStreamLine([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
					span.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
				}
				want := tc.Success
				if stream {
					want = tc.StreamSuccess
				}
				if !span.titleResponse.complete || !span.titleResponse.outcomeKnown || span.titleResponse.valid != want || span.titleResponse.text != nil {
					t.Fatalf("native title result known=%t valid=%t want=%t", span.titleResponse.outcomeKnown, span.titleResponse.valid, want)
				}
			})
		}
	}
}

func TestSDKTitleTerminalFailureIsIdempotentAndClockedAtDecision(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newSDKPreparationTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	main := testRequestFacts(uuid.NewString())
	main.Role = claudeprofile.RoleMain
	manager.BeginRequest(t.Context(), auth, main).ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[]}`), http.Header{})
	facts := main
	facts.Role, facts.ParentPromptID, facts.PromptID = claudeprofile.RoleTitle, main.PromptID, uuid.NewString()
	span := manager.BeginRequest(t.Context(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[]}`), http.Header{})
	span.FinishTitleFailure()
	if span.titleOutcomeFinished {
		t.Fatal("unfinished request became a terminal title")
	}
	clock.Advance(time.Second)
	span.FinishFailure(t.Context(), "network_error", errors.New("PRIVATE_ERROR"))
	clock.Advance(2 * time.Second)
	decisionAt := clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	span.FinishTitleFailure()
	span.FinishTitleFailure()
	span.FinishSuccess(t.Context())
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			continue
		}
		for _, event := range batch.Events {
			if event.EventData.EventName == "tengu_session_title_generated" {
				count++
				if event.EventData.ClientTimestamp != decisionAt {
					t.Fatal("title timestamp used provisional failure time")
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("terminal title count=%d", count)
	}
	if span.sdkWorker.factIssueSnapshot() != nil {
		t.Fatal("terminal decision retained pending title issue")
	}
}

func TestSDKTitleFinalizerIsolation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
	manager := newSDKPreparationTestManager(t, clock, &testDoer{})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts(uuid.NewString())
	facts.Role, facts.ParentPromptID, facts.ChainStartedAt = claudeprofile.RoleTitle, uuid.NewString(), clock.Now()
	makeSpan := func(input RequestFacts) *RequestSpan {
		span := manager.BeginRequest(t.Context(), auth, input)
		span.ObserveRequest([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[]}`), http.Header{})
		return span
	}
	first := makeSpan(facts)
	if first.TitleFinalizerKey() == "" {
		t.Fatal("missing title key")
	}
	clock.Advance(time.Second)
	retryFacts := facts
	retryFacts.PromptID, retryFacts.ClientRequestID, retryFacts.Attempt, retryFacts.StartedAt = uuid.NewString(), uuid.NewString(), 2, clock.Now()
	if makeSpan(retryFacts).TitleFinalizerKey() != first.TitleFinalizerKey() {
		t.Fatal("attempt IDs split one title generation")
	}
	for _, mutate := range []func(*RequestFacts){
		func(f *RequestFacts) { f.ParentPromptID = uuid.NewString() },
		func(f *RequestFacts) { f.SessionID = uuid.NewString() },
		func(f *RequestFacts) { f.ChainStartedAt = clock.Now() },
	} {
		other := retryFacts
		mutate(&other)
		if makeSpan(other).TitleFinalizerKey() == first.TitleFinalizerKey() {
			t.Fatal("unrelated title generation shared finalizer")
		}
	}
	auth = newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	if makeSpan(retryFacts).TitleFinalizerKey() == first.TitleFinalizerKey() {
		t.Fatal("accounts shared title finalizer")
	}
}
