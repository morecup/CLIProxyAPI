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
			want := map[string]any{"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID, "desktop_app_version": "1.40609.0.0", "querySource": "sdk", "precomputed": false, "precomputedKind": "none"}
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
				want := map[string]any{"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID, "desktop_app_version": "1.40609.0.0",
					"attempt": float64(attempt), "groupsToSummarize": float64(summarize), "groupsToPreserve": float64(preserve),
					"messagesToSummarize": float64(2*summarize - 1), "strippedMedia": index > 0,
					"splitKind": "round", "headTruncations": float64(0)}
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

func TestSDKReactiveCompactionTerminalEventsMatchAutomaticLifecycle(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 4)
		if !view.ClaimReactiveFailure() {
			t.Fatal("fixture did not claim main recovery")
		}
		operation := span.BeginSDKReactiveCompaction(view)
		defer operation.Close()
		span.FinishFailure(t.Context(), "invalid_request", errors.New("PRIVATE_FAILURE"))
		result, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil,
			func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				f.clock.Advance(time.Millisecond)
				operation.ObserveAttempt(attempt)
				return claudeprompt.SDKReactiveQueryResult[bool]{Success: true, Payload: true}, nil
			})
		if err != nil || !result.ReadyToApply {
			t.Fatalf("summary result=%+v err=%v", result, err)
		}
		f.clock.Advance(27 * time.Millisecond)
		pre, post := view.History().TokenEstimate.Tokens, int64(42)
		preservedUUIDs := 0
		for _, message := range result.Preserve {
			if message.UUID != "" {
				preservedUUIDs++
			}
		}
		operation.RecordSuccess(SDKReactiveCompactionSuccess{
			Attempts: result.Attempts, GroupsPreserved: result.GroupsPreserved, TotalGroups: result.TotalGroups,
			SplitKind: result.SplitKind, HeadTruncations: result.HeadTruncations,
			PreservedUUIDCount: preservedUUIDs, PreservedMessageCount: len(result.Preserve), ForkAssistantMessageCount: 1,
			RestoredItemCount: 3, PreCompactTokens: &pre, PostCompactTokens: &post, UsageKnown: true,
			Usage: claudeprompt.SDKTokenUsage{InputTokens: 10, OutputTokens: 7, CacheReadInputTokens: 30, CacheCreationInputTokens: 5},
		})
		operation.RecordFailure(SDKReactiveCompactionFailure{Reason: "error", Attempts: 1, TotalGroups: result.TotalGroups})
		events := f.events(t)
		if len(events["tengu_reactive_compact_succeeded"]) != 1 || len(events["tengu_reactive_compact_failed"]) != 0 {
			t.Fatalf("terminal events=%v", events)
		}
		metadata := events["tengu_reactive_compact_succeeded"][0]
		want := map[string]any{
			"attempts": float64(result.Attempts), "groupsPreserved": float64(result.GroupsPreserved), "totalGroups": float64(result.TotalGroups),
			"splitKind": "round", "headTruncations": float64(0), "preservedUuidCount": float64(preservedUUIDs),
			"preservedMessageCount": float64(len(result.Preserve)), "forkAssistantMessageCount": float64(1), "trigger": "auto",
			"restoredAttachmentCount": float64(3), "durationMs": float64(28), "userWaitMs": float64(28), "precomputed": false,
			"querySource": "sdk", "effort_level": "high", "preCompactTokens": float64(pre), "postCompactTokens": float64(post),
			"compactionInputTokens": float64(10), "compactionOutputTokens": float64(7), "compactionCacheReadTokens": float64(30),
			"compactionCacheCreationTokens": float64(5), "compactionTotalTokens": float64(52), "cacheHitRate": float64(2) / 3,
			"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID, "desktop_app_version": "1.40609.0.0",
		}
		if !reflect.DeepEqual(metadata, want) {
			t.Fatalf("success metadata=%v want=%v", metadata, want)
		}
		for name, items := range events {
			if strings.HasPrefix(name, "tengu_auto_compact_") && len(items) > 0 {
				t.Fatalf("post-PTL recovery fabricated a threshold preflight lifecycle: %s", name)
			}
		}
	})

	t.Run("failure", func(t *testing.T) {
		f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 3)
		if !view.ClaimReactiveFailure() {
			t.Fatal("fixture did not claim main recovery")
		}
		operation := span.BeginSDKReactiveCompaction(view)
		defer operation.Close()
		span.FinishFailure(t.Context(), "invalid_request", errors.New("PRIVATE_FAILURE"))
		result, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil,
			func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				f.clock.Advance(time.Millisecond)
				operation.ObserveAttempt(attempt)
				return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "error"}, nil
			})
		if err != nil || result.ReadyToApply || result.Reason != "error" {
			t.Fatalf("summary result=%+v err=%v", result, err)
		}
		f.clock.Advance(12 * time.Millisecond)
		pre := view.History().TokenEstimate.Tokens
		operation.RecordFailure(SDKReactiveCompactionFailure{
			Reason: result.Reason, Attempts: result.Attempts, TotalGroups: result.TotalGroups, PreCompactTokens: &pre,
		})
		operation.RecordSuccess(SDKReactiveCompactionSuccess{Attempts: 1, GroupsPreserved: 1, TotalGroups: result.TotalGroups, SplitKind: "round"})
		events := f.events(t)
		if len(events["tengu_reactive_compact_failed"]) != 1 || len(events["tengu_reactive_compact_succeeded"]) != 0 {
			t.Fatalf("terminal events=%v", events)
		}
		metadata := events["tengu_reactive_compact_failed"][0]
		want := map[string]any{
			"reason": "error", "trigger": "auto", "attempts": float64(1), "totalGroups": float64(result.TotalGroups),
			"durationMs": float64(13), "querySource": "sdk", "effort_level": "high", "preCompactTokens": float64(pre),
			"subscription_type": "pro", "cc_prompt_id": span.facts.PromptID, "desktop_app_version": "1.40609.0.0",
		}
		if !reflect.DeepEqual(metadata, want) {
			t.Fatalf("failure metadata=%v want=%v", metadata, want)
		}
	})
}

func TestSDKReactiveCompactionSummarizeAllTelemetry(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 5)
		view.ClaimReactiveFailure()
		operation := span.BeginSDKReactiveCompaction(view)
		defer operation.Close()
		result, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil,
			func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				f.clock.Advance(time.Millisecond)
				operation.ObserveAttempt(attempt)
				if attempt.SplitKind == "round" {
					return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "prompt_too_long"}, nil
				}
				return claudeprompt.SDKReactiveQueryResult[bool]{Success: true, Payload: true}, nil
			})
		if err != nil || !result.ReadyToApply || result.SplitKind != "summarize_all" || result.HeadTruncations != 1 || result.GroupsPreserved != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		operation.RecordSuccess(SDKReactiveCompactionSuccess{
			Attempts: result.Attempts, GroupsPreserved: result.GroupsPreserved, TotalGroups: result.TotalGroups,
			SplitKind: result.SplitKind, HeadTruncations: result.HeadTruncations,
			PreservedUUIDCount: len(result.Preserve), PreservedMessageCount: len(result.Preserve),
		})
		events := f.events(t)
		attempts := events["tengu_reactive_compact_attempt"]
		if len(attempts) != result.Attempts || len(attempts) == 0 {
			t.Fatalf("attempt events=%d result=%+v", len(attempts), result)
		}
		last := attempts[len(attempts)-1]
		if last["splitKind"] != "summarize_all" || last["headTruncations"] != float64(1) ||
			last["groupsToSummarize"] != float64(result.TotalGroups) || last["groupsToPreserve"] != float64(0) {
			t.Fatalf("fallback attempt metadata=%v", last)
		}
		success := events["tengu_reactive_compact_succeeded"]
		if len(success) != 1 || success[0]["splitKind"] != "summarize_all" || success[0]["headTruncations"] != float64(1) ||
			success[0]["groupsPreserved"] != float64(0) {
			t.Fatalf("fallback success metadata=%v", success)
		}
	})

	t.Run("failed-after-head-limit", func(t *testing.T) {
		f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 8)
		view.ClaimReactiveFailure()
		operation := span.BeginSDKReactiveCompaction(view)
		defer operation.Close()
		result, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil,
			func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
				operation.ObserveAttempt(attempt)
				return claudeprompt.SDKReactiveQueryResult[bool]{Reason: "prompt_too_long"}, nil
			})
		if err != nil || result.Reason != "exhausted" || result.SplitKind != "summarize_all" || result.HeadTruncations != 3 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		head := result.HeadTruncations
		operation.RecordFailure(SDKReactiveCompactionFailure{
			Reason: result.Reason, Attempts: result.Attempts, TotalGroups: result.TotalGroups,
			SplitKind: result.SplitKind, HeadTruncations: &head,
		})
		events := f.events(t)
		failed := events["tengu_reactive_compact_failed"]
		if len(failed) != 1 || failed[0]["splitKind"] != "summarize_all" || failed[0]["headTruncations"] != float64(3) {
			t.Fatalf("fallback failure metadata=%v", failed)
		}
	})
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
