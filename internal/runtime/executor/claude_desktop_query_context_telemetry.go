package executor

import (
	"context"
	"encoding/json"
	"strings"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// Native tengu_attachment_compute_duration for owned requests. The native
// attachment generator set y0s runs every generator through fi(label, fn),
// which times the run and samples the event at Math.random()<0.05. The
// gateway runs one real generator, the deferred_tools_delta reminder
// (helps.ClaudeDesktopOwnedRequestTools.Prepare); its run is measured into
// the request context by helps.ClaudeDesktopAttachmentTiming and reported
// here: on continuation turns at the native position inside the query-build
// loop sequence (after tengu_query_before_attachments, before
// tengu_attachments) through QueryLoopAttachments.ComputeDuration, and on
// first turns standalone before that loop hook.
const claudeDesktopQueryContextAttachmentHookOrder = 399

// claudeDesktopAttachmentComputeDraw replaces the native Math.random() draw
// in tests; nil uses the process random source.
var claudeDesktopAttachmentComputeDraw func() float64

// Native removed-names header (tasks.deferredToolsRemovedHeader).
const claudeDesktopDeferredToolsRemovedHeader = "The following deferred tools are no longer available (their MCP server disconnected). Do not search for them \u2014 ToolSearch will return no match:"

func init() {
	registerClaudeDesktopRequestTelemetryHook(claudeDesktopRequestTelemetryHook{Name: "query-context-attachment-duration", Order: claudeDesktopQueryContextAttachmentHookOrder, Run: observeClaudeDesktopFirstTurnAttachmentComputeDuration})
}

// observeClaudeDesktopFirstTurnAttachmentComputeDuration emits the sampled
// event for owned requests that are not continuation turns (no loop events
// precede it natively); continuation turns are handled by the loop hook.
func observeClaudeDesktopFirstTurnAttachmentComputeDuration(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput) {
	if !claudeDesktopQueryBuildRequestObserved(ctx, span, input) {
		return
	}
	if _, continuation := claudeDesktopQueryLoopAttachments(input.Body); continuation {
		return
	}
	if emit := claudeDesktopAttachmentComputeDurationEmitter(ctx, span); emit != nil {
		emit()
	}
}

// claudeDesktopAttachmentComputeDurationEmitter returns the emitter of this
// request's measured generator run, or nil when no generator ran.
func claudeDesktopAttachmentComputeDurationEmitter(ctx context.Context, span *claudeDesktopRequestSpan) func() {
	if span == nil || span.telemetry == nil {
		return nil
	}
	duration, reminder, ok := helps.ClaudeDesktopAttachmentTimingFromContext(ctx).Result()
	if !ok {
		return nil
	}
	sample := claudetelemetry.AttachmentComputeSample{Label: claudetelemetry.AttachmentTypeDeferredToolsDelta, Duration: duration}
	if reminder != "" {
		added, removed := claudeDesktopDeferredToolsReminderDelta(reminder)
		sample.Attachments = []json.RawMessage{claudetelemetry.NewDeferredToolsDeltaAttachment(added, removed)}
	}
	telemetry := span.telemetry
	return func() { telemetry.ObserveAttachmentComputeDuration(sample, claudeDesktopAttachmentComputeDraw) }
}

// claudeDesktopDeferredToolsReminderDelta recovers the added and removed
// names the reminder announced: the lines following the exact added or
// removed header up to the end of that paragraph.
func claudeDesktopDeferredToolsReminderDelta(reminder string) (added, removed []string) {
	lines := strings.Split(reminder, "\n")
	for i := 0; i < len(lines); i++ {
		isAdded := lines[i] == claudeDesktopDeferredToolsReminderHeader
		if !isAdded && lines[i] != claudeDesktopDeferredToolsRemovedHeader {
			continue
		}
		for i++; i < len(lines); i++ {
			name := lines[i]
			if name == "" || name == "</system-reminder>" {
				break
			}
			if isAdded {
				added = append(added, name)
			} else {
				removed = append(removed, name)
			}
		}
	}
	return added, removed
}
