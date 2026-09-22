package executor

import (
	"context"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopQueryLoopAttachmentsRecoversNativeRowCounts(t *testing.T) {
	reminder := "<system-reminder>\n" + claudeDesktopDeferredToolsReminderHeader + "\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>"
	first := claudeDesktopToolSearchTestBody(t, nil, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}, {"type": "text", "text": reminder}}})
	if _, ok := claudeDesktopQueryLoopAttachments(first); ok {
		t.Fatal("a first turn has no tool results and must not replay the loop events")
	}
	continuation := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}, {"type": "text", "text": reminder}}},
		map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "ok"}, {"type": "tool_use", "id": "toolu_1", "name": "ToolSearch", "input": map[string]any{"query": "select:SendMessage"}}, {"type": "tool_use", "id": "toolu_2", "name": "Agent", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "x"}, {"type": "tool_result", "tool_use_id": "toolu_2", "content": "y"}, {"type": "text", "text": "<system-reminder>\n" + claudeDesktopDeferredToolsReminderHeader + "\nBrief\n</system-reminder>"}}},
	)
	loop, ok := claudeDesktopQueryLoopAttachments(continuation)
	want := claudetelemetry.QueryLoopAttachments{MessagesForQueryCount: 2, AssistantMessagesCount: 3, ToolResultsCount: 2, AttachmentTypes: []string{"deferred_tools_delta"}}
	if !ok || loop.MessagesForQueryCount != want.MessagesForQueryCount || loop.AssistantMessagesCount != want.AssistantMessagesCount || loop.ToolResultsCount != want.ToolResultsCount || len(loop.AttachmentTypes) != 1 || loop.AttachmentTypes[0] != "deferred_tools_delta" || loop.FileChangeAttachmentCount != 0 {
		t.Fatalf("loop = %+v ok=%v", loop, ok)
	}
	total := 0
	for _, row := range gjson.GetBytes(continuation, "messages").Array() {
		total += claudeDesktopInternalRows(row).total()
	}
	if total != 2+3+3 {
		t.Fatalf("pre-normalized rows = %d, want 8", total)
	}
	plain := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "Agent", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "x"}}},
	)
	if loop, ok = claudeDesktopQueryLoopAttachments(plain); !ok || loop.MessagesForQueryCount != 1 || loop.AssistantMessagesCount != 1 || loop.ToolResultsCount != 1 || len(loop.AttachmentTypes) != 0 {
		t.Fatalf("plain continuation = %+v ok=%v", loop, ok)
	}
	noResults := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "next"},
	)
	if _, ok = claudeDesktopQueryLoopAttachments(noResults); ok {
		t.Fatal("a follow-up prompt without tool results is not a continuation turn")
	}
}

func TestClaudeDesktopQueryBuildHooksIgnoreUnownedOrRetriedRequests(t *testing.T) {
	body := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "Agent", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "x"}}},
	)
	span := &claudeDesktopRequestSpan{telemetry: &claudetelemetry.RequestSpan{}}
	owned := claudetasks.WithCaller(context.Background(), claudetasks.Caller{PromptID: "prompt-1", Model: "claude-opus-5"})
	for _, input := range []struct {
		ctx     context.Context
		span    *claudeDesktopRequestSpan
		role    claudeprofile.RequestRole
		attempt int
	}{
		{context.Background(), nil, claudeprofile.RoleMain, 1},
		{context.Background(), &claudeDesktopRequestSpan{}, claudeprofile.RoleMain, 1},
		{context.Background(), span, claudeprofile.RoleMain, 1},
		{owned, span, claudeprofile.RoleMain, 2},
		{owned, span, claudeprofile.RoleTitle, 1},
		// An owned first attempt reaches the inactive telemetry span without panicking.
		{owned, span, claudeprofile.RoleSubagent, 1},
	} {
		request := claudeDesktopRequestTelemetryInput{Role: input.role, Body: body, Attempt: input.attempt}
		observeClaudeDesktopQueryLoopAttachments(input.ctx, input.span, request)
		observeClaudeDesktopAPIBeforeNormalize(input.ctx, input.span, request)
	}
	var loopOrder, normalizeOrder int
	for _, hook := range claudeDesktopRequestTelemetryHooks {
		switch hook.Name {
		case "query-build-loop":
			loopOrder = hook.Order
		case "query-build-normalize":
			normalizeOrder = hook.Order
		}
	}
	if loopOrder == 0 || normalizeOrder == 0 || loopOrder >= claudeDesktopToolSearchHookOrder || normalizeOrder <= claudeDesktopToolSearchHookOrder {
		t.Fatalf("hooks must bracket the tool-search decision: loop=%d normalize=%d", loopOrder, normalizeOrder)
	}
}
