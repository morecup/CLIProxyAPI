package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// assertClaudeDesktopAttachmentComputeDurationStream checks, on the delivered
// SDK stream of the owned tool-search scenario (deterministic draw below the
// 5% threshold), that tengu_attachment_compute_duration fired once per owned
// request with the native position and values: standalone before the
// tool-search decision on first turns, between tengu_query_before_attachments
// and tengu_attachments/tengu_query_after_attachments on continuation turns;
// attachment_count 1 with the pinned deferred_tools_delta size when that
// request announced the three deferrable names, 0 otherwise.
func assertClaudeDesktopAttachmentComputeDurationStream(t *testing.T, doer *claudeDesktopTelemetryTestDoer, session, model string, ownedRequests int) {
	t.Helper()
	const (
		durationEvent = "tengu_attachment_compute_duration"
		beforeEvent   = "tengu_query_before_attachments"
		attachEvent   = "tengu_attachments"
		afterEvent    = "tengu_query_after_attachments"
		decisionEvent = "tengu_tool_search_mode_decision"
		poolEvent     = "tengu_deferred_tools_pool_change"
	)
	stream := sdkDeliveredEventStream(t, doer, session, model, durationEvent, beforeEvent, attachEvent, afterEvent, decisionEvent, poolEvent)
	announcedSize := float64(len(claudetelemetry.NewDeferredToolsDeltaAttachment([]string{"SendMessage", "TaskOutput", "TaskStop"}, nil)))
	fired := 0
	for index, event := range stream {
		if event.name != durationEvent {
			continue
		}
		fired++
		// Position: the previous kept event is before_attachments (continuation)
		// or the previous request's tail; the next kept event is attachments /
		// after_attachments (continuation) or this request's decision (first turn).
		next := ""
		if index+1 < len(stream) {
			next = stream[index+1].name
		}
		previous := ""
		if index > 0 {
			previous = stream[index-1].name
		}
		switch {
		case previous == beforeEvent && (next == attachEvent || next == afterEvent):
		case previous != beforeEvent && next == decisionEvent:
		default:
			t.Fatalf("%s at position %d sits between %q and %q, want native y0s position", durationEvent, index, previous, next)
		}
		if event.fields["label"] != "deferred_tools_delta" {
			t.Fatalf("%s label = %v", durationEvent, event.fields["label"])
		}
		duration, ok := event.fields["duration_ms"].(float64)
		if !ok || duration < 0 {
			t.Fatalf("%s duration_ms = %v", durationEvent, event.fields["duration_ms"])
		}
		// A pool change of this request means the generator produced the attachment.
		announced := false
		for cursor := index + 1; cursor < len(stream) && stream[cursor].name != durationEvent; cursor++ {
			if stream[cursor].name == poolEvent {
				announced = true
			}
		}
		if announced {
			assertSDKEventFields(t, event, map[string]any{"attachment_count": float64(1), "attachment_size_bytes": announcedSize})
		} else {
			assertSDKEventFields(t, event, map[string]any{"attachment_count": float64(0), "attachment_size_bytes": float64(0)})
		}
	}
	if fired != ownedRequests {
		t.Fatalf("%s fired %d times, want once per owned request (%d)", durationEvent, fired, ownedRequests)
	}
}

func TestClaudeDesktopAttachmentTimingWrapsTheRealGenerator(t *testing.T) {
	ctx, timing := helps.WithClaudeDesktopAttachmentTiming(t.Context())
	if helps.ClaudeDesktopAttachmentTimingFromContext(ctx) != timing {
		t.Fatal("timing recorder not installed on the context")
	}
	if _, _, ok := timing.Result(); ok {
		t.Fatal("recorder measured before the generator ran")
	}
	reminder := "<system-reminder>\n" + claudeDesktopDeferredToolsReminderHeader + "\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>"
	tools := helps.ClaudeDesktopOwnedRequestTools{DeferredToolsReminder: func([]json.RawMessage) string {
		time.Sleep(2 * time.Millisecond)
		return reminder
	}}.Timed(timing)
	prepared, err := tools.Prepare([]json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if err != nil || prepared.Announced != 0 {
		t.Fatalf("prepare: %v announced=%d", err, prepared.Announced)
	}
	duration, got, ok := timing.Result()
	if !ok || got != reminder || duration < 2*time.Millisecond {
		t.Fatalf("recorded ok=%t duration=%s reminder match=%t", ok, duration, got == reminder)
	}
	added, removed := claudeDesktopDeferredToolsReminderDelta(reminder)
	if strings.Join(added, ",") != "SendMessage,TaskOutput,TaskStop" || len(removed) != 0 {
		t.Fatalf("delta added=%v removed=%v", added, removed)
	}
	if emit := claudeDesktopAttachmentComputeDurationEmitter(ctx, nil); emit != nil {
		t.Fatal("emitter without a span")
	}
	if emit := claudeDesktopAttachmentComputeDurationEmitter(t.Context(), &claudeDesktopRequestSpan{}); emit != nil {
		t.Fatal("emitter without a recorded generator run")
	}
	// Timing without a generator leaves the tools untouched.
	if (helps.ClaudeDesktopOwnedRequestTools{}).Timed(timing).DeferredToolsReminder != nil {
		t.Fatal("wrapper invented a generator")
	}
}

func TestClaudeDesktopAttachmentComputeDurationAboveThresholdEmitsNothing(t *testing.T) {
	// The native draw at or above 0.05 is silent; the span-level emitter is
	// the same one the hooks call.
	if claudetelemetry.AttachmentComputeSampled(0.05) || claudetelemetry.AttachmentComputeSampled(0.9) || !claudetelemetry.AttachmentComputeSampled(0.049) {
		t.Fatal("sampling threshold differs from the native Math.random()<0.05")
	}
}
