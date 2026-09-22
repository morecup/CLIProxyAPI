package executor

import (
	"context"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/tidwall/gjson"
)

// Native query-build telemetry for owned requests (main turns of the remote
// input actor and child generations). The native loop emits
// tengu_query_before_attachments / tengu_attachments /
// tengu_query_after_attachments after the tools of a turn ran (before the
// next request build starts with the tool-search decision) and
// tengu_api_before_normalize inside the request build after that decision.
// Values are recovered from the wire-shaped owned request: native internal
// rows are one per assistant content block, one per tool_result and one per
// attachment (the deferred-tools reminder blocks this gateway produces);
// normalization merges them into the API messages the body carries.
const (
	claudeDesktopQueryBuildLoopHookOrder      = 400
	claudeDesktopQueryBuildNormalizeHookOrder = 600
)

func init() {
	registerClaudeDesktopRequestTelemetryHook(claudeDesktopRequestTelemetryHook{Name: "query-build-loop", Order: claudeDesktopQueryBuildLoopHookOrder, Run: observeClaudeDesktopQueryLoopAttachments})
	registerClaudeDesktopRequestTelemetryHook(claudeDesktopRequestTelemetryHook{Name: "query-build-normalize", Order: claudeDesktopQueryBuildNormalizeHookOrder, Run: observeClaudeDesktopAPIBeforeNormalize})
}

func claudeDesktopQueryBuildRequestObserved(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput) bool {
	if span == nil || span.telemetry == nil || input.Attempt > 1 || !claudetasks.IsOwnedRequest(ctx) {
		return false
	}
	return input.Role == claudeprofile.RoleMain || input.Role == claudeprofile.RoleSubagent
}

// observeClaudeDesktopQueryLoopAttachments replays the continuation-turn
// sequence for an owned request whose last user row carries tool results.
func observeClaudeDesktopQueryLoopAttachments(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput) {
	if !claudeDesktopQueryBuildRequestObserved(ctx, span, input) {
		return
	}
	if loop, ok := claudeDesktopQueryLoopAttachments(input.Body); ok {
		loop.ComputeDuration = claudeDesktopAttachmentComputeDurationEmitter(ctx, span)
		span.telemetry.ObserveQueryLoopAttachments(loop)
	}
}

// observeClaudeDesktopAPIBeforeNormalize reports the internal row count of
// every owned request build.
func observeClaudeDesktopAPIBeforeNormalize(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput) {
	if !claudeDesktopQueryBuildRequestObserved(ctx, span, input) {
		return
	}
	rows := gjson.GetBytes(input.Body, "messages").Array()
	if len(rows) == 0 {
		return
	}
	total := 0
	for _, row := range rows {
		internal := claudeDesktopInternalRows(row)
		total += internal.total()
	}
	span.telemetry.ObserveAPIBeforeNormalize(total)
}

// claudeDesktopInternalRowCounts is the native pre-normalization shape of one
// wire message: prompt/text rows, tool_result rows and attachment rows.
type claudeDesktopInternalRowCounts struct {
	prompt      int
	toolResults int
	attachments int
	assistant   int
}

func (c claudeDesktopInternalRowCounts) total() int {
	return c.prompt + c.toolResults + c.attachments + c.assistant
}

func claudeDesktopInternalRows(row gjson.Result) claudeDesktopInternalRowCounts {
	var counts claudeDesktopInternalRowCounts
	content := row.Get("content")
	if row.Get("role").String() == "assistant" {
		if content.Type == gjson.String {
			counts.assistant = 1
		} else if blocks := content.Array(); len(blocks) > 0 {
			counts.assistant = len(blocks)
		} else {
			counts.assistant = 1
		}
		return counts
	}
	reminders := len(claudeDesktopDeferredToolsReminderNames(row))
	counts.attachments = reminders
	if content.Type == gjson.String {
		if reminders == 0 {
			counts.prompt = 1
		}
		return counts
	}
	other := 0
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "tool_result":
			counts.toolResults++
		case "text":
			if strings.HasPrefix(strings.TrimSpace(block.Get("text").String()), "<system-reminder>\n"+claudeDesktopDeferredToolsReminderHeader) {
				continue
			}
			other++
		default:
			other++
		}
	}
	if other > 0 {
		counts.prompt = 1
	}
	return counts
}

// claudeDesktopQueryLoopAttachments recovers the native loop counters when
// the request continues a tool-using assistant turn: the last user row holds
// tool results and follows an assistant row.
func claudeDesktopQueryLoopAttachments(body []byte) (claudetelemetry.QueryLoopAttachments, bool) {
	rows := gjson.GetBytes(body, "messages").Array()
	if len(rows) < 2 {
		return claudetelemetry.QueryLoopAttachments{}, false
	}
	last, previous := rows[len(rows)-1], rows[len(rows)-2]
	if last.Get("role").String() != "user" || previous.Get("role").String() != "assistant" {
		return claudetelemetry.QueryLoopAttachments{}, false
	}
	lastRows := claudeDesktopInternalRows(last)
	if lastRows.toolResults == 0 {
		return claudetelemetry.QueryLoopAttachments{}, false
	}
	before := 0
	for _, row := range rows[:len(rows)-2] {
		before += claudeDesktopInternalRows(row).total()
	}
	loop := claudetelemetry.QueryLoopAttachments{
		MessagesForQueryCount:  before,
		AssistantMessagesCount: claudeDesktopInternalRows(previous).assistant,
		ToolResultsCount:       lastRows.toolResults,
	}
	for index := 0; index < lastRows.attachments; index++ {
		loop.AttachmentTypes = append(loop.AttachmentTypes, claudetelemetry.AttachmentTypeDeferredToolsDelta)
	}
	return loop, true
}
