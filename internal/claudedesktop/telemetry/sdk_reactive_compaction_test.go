package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/tidwall/gjson"
)

func reactiveTelemetryFixture(t *testing.T, model string, rounds int) (*sdkResultFixture, *RequestSpan, *claudeprompt.SDKCompactionView) {
	t.Helper()
	f := newSDKResultFixture(t, true)
	var messages []map[string]any
	for index := range rounds {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("PRIVATE_INPUT_%d", index)})
		payload := map[string]any{"model": model, "messages": messages}
		if !strings.Contains(model, "haiku") {
			payload["output_config"] = map[string]any{"effort": "high"}
		}
		body, _ := json.Marshal(payload)
		span := f.begin(t, body)
		if index == rounds-1 {
			span.ObserveHTTPResponse(400, http.Header{})
			view, err := span.facts.Prompt.CompactionView(body)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(view.Discard)
			return f, span, view
		}
		f.clock.Advance(time.Second)
		var response claudeprompt.Response
		response.ObservePayloadAt([]byte(fmt.Sprintf(`{"type":"message","role":"assistant","id":"msg_%d","content":[{"type":"text","text":"PRIVATE_REPLY"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, index)), false, f.clock.Now())
		span.facts.Prompt.ObserveSDKHistory(response.SDKHistoryMessages())
		span.facts.Prompt.ObserveSDKWireResponse(response.SDKWireFingerprint())
		span.facts.Prompt.FinishSuccess(f.clock.Now(), "end_turn", nil)
		span.ObserveHTTPResponse(200, http.Header{})
		span.ObserveResponse(fmt.Sprintf("req_%d", index), "end_turn")
		span.FinishSuccess(t.Context())
		messages = append(messages, map[string]any{"role": "assistant", "content": "PRIVATE_REPLY"})
	}
	t.Fatal("empty fixture")
	return nil, nil, nil
}

func TestSDKReactiveCompactionEventsUseOwnedParentAndIteration(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			f, span, view := reactiveTelemetryFixture(t, model, 4)
			if !view.ClaimReactiveFailure() {
				t.Fatal("fixture did not claim main recovery")
			}
			operation := span.BeginSDKReactiveCompaction(view)
			defer operation.Close()
			if operation == nil || span.BeginSDKReactiveCompaction(view) != operation {
				t.Fatal("duplicate observer acquired a different recovery")
			}
			// Closing the physical API attempt must not close recovery telemetry.
			span.FinishFailure(t.Context(), "invalid_request", errors.New("PRIVATE_FAILURE"))
			iterations := 0
			_, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil, func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				f.clock.Advance(time.Millisecond)
				var group sync.WaitGroup
				for range 8 {
					group.Go(func() { operation.ObserveAttempt(attempt) })
				}
				group.Wait()
				iterations++
				if iterations == 1 {
					return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "media_too_large"}, nil
				}
				if iterations == 2 {
					return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "prompt_too_long"}, nil
				}
				return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			events := f.events(t)
			if len(events["tengu_reactive_compact_triggered"]) != 1 || len(events["tengu_reactive_compact_attempt"]) != 3 || len(events["tengu_reactive_compact_succeeded"]) != 0 {
				t.Fatal("incorrect recovery lifecycle events")
			}
			trigger := events["tengu_reactive_compact_triggered"][0]
			want := map[string]any{"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID, "querySource": "sdk", "precomputed": false, "precomputedKind": "none"}
			if !strings.Contains(model, "haiku") {
				want["effort_level"] = "high"
			}
			if !reflect.DeepEqual(trigger, want) {
				t.Fatalf("trigger metadata=%v want=%v", trigger, want)
			}
			for index, metadata := range events["tengu_reactive_compact_attempt"] {
				attempt, summarize, preserve := 1, 3, 1
				if index == 2 {
					attempt, summarize, preserve = 2, 2, 2
				}
				want := map[string]any{"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID,
					"attempt": float64(attempt), "groupsToSummarize": float64(summarize), "groupsToPreserve": float64(preserve),
					"messagesToSummarize": float64(2*summarize - 1), "strippedMedia": index > 0}
				if index == 2 {
					want["stepMode"], want["stepSize"] = "gap_unparseable", float64(1)
				}
				if !reflect.DeepEqual(metadata, want) {
					t.Fatalf("iteration %d metadata=%v want=%v", index, metadata, want)
				}
			}
			var names []string
			var previous time.Time
			for _, request := range f.doer.Requests() {
				for _, event := range gjson.GetBytes(request.Body, "events").Array() {
					data := event.Get("event_data")
					if !strings.HasPrefix(data.Get("event_name").String(), "tengu_reactive_compact_") {
						continue
					}
					betas, _ := f.manager.sdkProfile.InputBetaHeader(model)
					if data.Get("model").String() != model || data.Get("betas").String() != betas || data.Get("session_id").String() != f.session {
						t.Fatal("recovery inherited foreign helper or main-header dimensions")
					}
					at, err := time.Parse(time.RFC3339Nano, data.Get("client_timestamp").String())
					if err != nil || (!previous.IsZero() && !at.After(previous)) {
						t.Fatal("events lost their actual occurrence order")
					}
					previous = at
					names = append(names, data.Get("event_name").String())
					metadata, _ := base64.StdEncoding.DecodeString(data.Get("additional_metadata").String())
					if strings.Contains(string(metadata), "PRIVATE_") {
						t.Fatal("content leaked to recovery telemetry")
					}
				}
			}
			if len(names) != 4 || names[0] != "tengu_reactive_compact_triggered" {
				t.Fatal("trigger was not delivered before attempts")
			}
		})
	}
}

func TestSDKReactiveCompactionRejectsForeignUnclaimedAndLateObservers(t *testing.T) {
	f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 3)
	_, foreign, foreignView := reactiveTelemetryFixture(t, "claude-opus-5", 3)
	if span.BeginSDKReactiveCompaction(view) != nil {
		t.Fatal("an unclaimed error became a recovery trigger")
	}
	foreignView.ClaimReactiveFailure()
	if span.BeginSDKReactiveCompaction(foreignView) != nil || foreign.BeginSDKReactiveCompaction(view) != nil {
		t.Fatal("observer accepted foreign account/session ownership")
	}
	view.ClaimReactiveFailure()
	operation := span.BeginSDKReactiveCompaction(view)
	if operation == nil {
		t.Fatal("claimed operation was not observed")
	}
	operation.Close()
	_, _ = claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil, func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
		operation.ObserveAttempt(attempt)
		return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
	})
	if events := f.events(t); len(events["tengu_reactive_compact_triggered"]) != 1 || len(events["tengu_reactive_compact_attempt"]) != 0 {
		t.Fatal("late callback emitted an attempt")
	}
}

func TestSDKReactiveCompactionQueueFailureRemainsVisibleAfterSuccess(t *testing.T) {
	f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 3)
	view.ClaimReactiveFailure()
	delete(f.manager.sdkProfile.Events, FactSDKReactiveCompactTriggered)
	operation := span.BeginSDKReactiveCompaction(view)
	defer operation.Close()
	_, _ = claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil, func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
		operation.ObserveAttempt(attempt)
		return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
	})
	_ = f.events(t)
	if issues := span.sdkWorker.factIssueSnapshot(); issues == nil || issues.Unresolved == 0 || issues.Status != "awaiting-sdk-prompt-facts" {
		t.Fatal("lost trigger was cleared by a later successful enqueue/flush")
	}
	record, err := readFactIssueRecord(span.sdkWorker.directory)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kind := range record.Issues {
		found = found || kind == factIssueSDKCompaction
	}
	if !found {
		t.Fatal("compaction fact issue was not persisted")
	}
	visible := false
	for _, endpoint := range f.manager.Status().DeliveryEndpoints {
		visible = visible || (endpoint.Role == "sdk-event-logging" && strings.Contains(endpoint.Reason, "reactive compaction facts"))
	}
	if !visible {
		t.Fatal("management status hid the retained compaction failure")
	}
	encoded, err := os.ReadFile(filepath.Join(span.sdkWorker.directory, factIssuesFile))
	if err != nil || strings.Contains(string(encoded), f.session) || strings.Contains(string(encoded), span.facts.PromptID) {
		t.Fatal("diagnostic state was not protected")
	}
}

func TestSDKReactiveCompactionSeededAndGuidedFieldsReachDelivery(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprint(seeded), func(t *testing.T) {
			f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 5)
			view.ClaimReactiveFailure()
			operation := span.BeginSDKReactiveCompaction(view)
			defer operation.Close()
			gap := int64(100000)
			var initial *int64
			if seeded {
				initial = &gap
			}
			iterations := 0
			_, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), initial, func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				operation.ObserveAttempt(attempt)
				iterations++
				if !seeded && iterations == 1 {
					return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "prompt_too_long", TokenGap: &gap}, nil
				}
				return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			events := f.events(t)["tengu_reactive_compact_attempt"]
			wantMode := "gap_guided"
			if seeded {
				wantMode = "seeded"
			}
			if len(events) != iterations || len(events) == 0 {
				t.Fatal("missing actual selection events")
			}
			last := events[len(events)-1]
			if last["tokenGap"] != float64(gap) || last["stepMode"] != wantMode || last["stepSize"] != float64(2) || last["groupsToPreserve"] != float64(3) {
				t.Fatalf("native gap selection metadata changed: %v", last)
			}
		})
	}
}

func TestSDKReactiveCompactionMissingDimensionsNeverBorrowRequestBetas(t *testing.T) {
	f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 3)
	view.ClaimReactiveFailure()
	delete(f.manager.sdkProfile.InputBetas, "claude-opus-5")
	span.facts.Betas = "PRIVATE_REQUEST_ONLY"
	operation := span.BeginSDKReactiveCompaction(view)
	defer operation.Close()
	_, _ = claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil, func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
		operation.ObserveAttempt(attempt)
		return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
	})
	events := f.events(t)
	if len(events["tengu_reactive_compact_triggered"]) != 0 || len(events["tengu_reactive_compact_attempt"]) != 0 || span.sdkWorker.factIssueSnapshot() == nil {
		t.Fatal("missing session dimensions became a guessed or silently healthy event")
	}
}
