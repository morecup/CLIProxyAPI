package executor

import (
	"context"
	"encoding/json"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
)

func claudeDesktopToolSearchTestBody(t *testing.T, tools []string, messages ...map[string]any) []byte {
	t.Helper()
	toolValues := make([]map[string]any, 0, len(tools))
	for _, name := range tools {
		toolValues = append(toolValues, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
	}
	body, err := json.Marshal(map[string]any{"model": "claude-opus-5", "tools": toolValues, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClaudeDesktopToolSearchModeDecisionReplaysNativeBranches(t *testing.T) {
	pool := []string{"Agent", "DeferredToolPlaceholder", "ToolSearch"}
	cases := []struct {
		name     string
		model    string
		tools    []string
		disabled bool
		want     claudetelemetry.ToolSearchModeDecision
	}{
		{"enabled", "claude-opus-5", pool, false, claudetelemetry.ToolSearchModeDecision{Enabled: true, Mode: "tst", Reason: "tst_enabled", CheckedModel: "claude-opus-5", MCPNonBlocking: true}},
		{"unsupported model wins", "claude-3-5-haiku-20241022", pool, false, claudetelemetry.ToolSearchModeDecision{Mode: "standard", Reason: "model_unsupported", CheckedModel: "claude-3-5-haiku-20241022", MCPNonBlocking: true}},
		{"haiku 4.5 supported", "claude-haiku-4-5-20251001", pool, false, claudetelemetry.ToolSearchModeDecision{Enabled: true, Mode: "tst", Reason: "tst_enabled", CheckedModel: "claude-haiku-4-5-20251001", MCPNonBlocking: true}},
		{"no tools", "claude-opus-5", nil, false, claudetelemetry.ToolSearchModeDecision{Mode: "tst", Reason: "no_tools_in_request", CheckedModel: "claude-opus-5", MCPNonBlocking: true}},
		{"no tools standard", "claude-opus-5", nil, true, claudetelemetry.ToolSearchModeDecision{Mode: "standard", Reason: "no_tools_in_request", CheckedModel: "claude-opus-5", MCPNonBlocking: true}},
		{"not registered", "claude-opus-5", []string{"Agent", "SendMessage", "TaskOutput", "TaskStop"}, true, claudetelemetry.ToolSearchModeDecision{Mode: "standard", Reason: "not_registered", CheckedModel: "claude-opus-5", MCPNonBlocking: true}},
		{"mcp tools counted", "claude-opus-5", []string{"Agent", "mcp__srv__tool", "ToolSearch"}, false, claudetelemetry.ToolSearchModeDecision{Enabled: true, Mode: "tst", Reason: "tst_enabled", CheckedModel: "claude-opus-5", MCPToolCount: 1, MCPNonBlocking: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeDesktopToolSearchModeDecision(tc.model, claudeDesktopToolSearchTestBody(t, tc.tools), tc.disabled, nil)
			if got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
	custom := claudeDesktopToolSearchModeDecision("claude-opus-5", claudeDesktopToolSearchTestBody(t, pool), false, []string{"OPUS-5"})
	if custom.Reason != "model_unsupported" || custom.Mode != "standard" {
		t.Fatalf("configured unsupported list ignored: %+v", custom)
	}
	if empty := claudeDesktopToolSearchModeDecision("claude-opus-5", claudeDesktopToolSearchTestBody(t, pool), false, []string{}); !empty.Enabled {
		t.Fatalf("empty unsupported list must not fall back to the default: %+v", empty)
	}
}

func TestClaudeDesktopDeferredToolsPoolChangeRecoversNativeCounters(t *testing.T) {
	reminder := "<system-reminder>\n" + claudeDesktopDeferredToolsReminderHeader + "\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>"
	first := claudeDesktopToolSearchTestBody(t, nil, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}, {"type": "text", "text": reminder}}})
	change, ok := claudeDesktopDeferredToolsPoolChange(claudeprofile.RoleMain, first)
	want := claudetelemetry.DeferredToolsPoolChange{AddedCount: 3, UnlistedCount: 3, MessagesLength: 1, CallSite: "attachments_main"}
	if !ok || change.AttachmentTypesSeen != nil || change.CallSite != want.CallSite || change.AddedCount != want.AddedCount || change.UnlistedCount != want.UnlistedCount || change.PriorAnnouncedCount != 0 || change.MessagesLength != 1 || change.DTDCount != 0 || change.ReaddedCount != 0 {
		t.Fatalf("first turn change = %+v ok=%v", change, ok)
	}
	if change, ok = claudeDesktopDeferredToolsPoolChange(claudeprofile.RoleSubagent, first); !ok || change.CallSite != "attachments_subagent" {
		t.Fatalf("subagent call site lost: %+v", change)
	}
	later := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}, {"type": "text", "text": reminder}}},
		map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "ToolSearch", "input": map[string]any{"query": "select:SendMessage"}}}},
		map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{{"type": "tool_reference", "tool_name": "SendMessage"}}}}},
	)
	if _, ok = claudeDesktopDeferredToolsPoolChange(claudeprofile.RoleMain, later); ok {
		t.Fatal("a request without a new reminder must not report a pool change")
	}
	newNames := "<system-reminder>\n" + claudeDesktopDeferredToolsReminderHeader + "\nBrief\nTaskStop\n</system-reminder>"
	third := claudeDesktopToolSearchTestBody(t, nil,
		map[string]any{"role": "user", "content": reminder},
		map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "ok"}}},
		map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "next"}, {"type": "text", "text": newNames}}},
	)
	change, ok = claudeDesktopDeferredToolsPoolChange(claudeprofile.RoleMain, third)
	if !ok || change.AddedCount != 2 || change.UnlistedCount != 2 || change.ReaddedCount != 1 || change.PriorAnnouncedCount != 3 || change.DTDCount != 1 || change.MessagesLength != 3 {
		t.Fatalf("later change = %+v ok=%v", change, ok)
	}
	if _, ok = claudeDesktopDeferredToolsPoolChange(claudeprofile.RoleMain, claudeDesktopToolSearchTestBody(t, nil)); ok {
		t.Fatal("empty history must not report a pool change")
	}
}

func TestObserveClaudeDesktopToolSearchRequestIgnoresUnownedOrRetriedRequests(t *testing.T) {
	body := claudeDesktopToolSearchTestBody(t, []string{"Agent", "ToolSearch"})
	// Nil spans, spans without telemetry, downstream requests and retries are all silent.
	observeClaudeDesktopToolSearchRequest(context.Background(), nil, claudeprofile.RoleMain, "claude-opus-5", body, 1, false, nil)
	observeClaudeDesktopToolSearchRequest(context.Background(), &claudeDesktopRequestSpan{}, claudeprofile.RoleMain, "claude-opus-5", body, 1, false, nil)
	span := &claudeDesktopRequestSpan{telemetry: &claudetelemetry.RequestSpan{}}
	observeClaudeDesktopToolSearchRequest(context.Background(), span, claudeprofile.RoleMain, "claude-opus-5", body, 1, false, nil)
	owned := claudetasks.WithCaller(context.Background(), claudetasks.Caller{PromptID: "prompt-1", Model: "claude-opus-5"})
	observeClaudeDesktopToolSearchRequest(owned, span, claudeprofile.RoleMain, "claude-opus-5", body, 2, false, nil)
	observeClaudeDesktopToolSearchRequest(owned, span, claudeprofile.RoleTitle, "claude-opus-5", body, 1, false, nil)
	// An owned first attempt reaches the inactive telemetry span without panicking.
	observeClaudeDesktopToolSearchRequest(owned, span, claudeprofile.RoleSubagent, "claude-opus-5", body, 1, false, nil)
	var executor *ClaudeExecutor
	executor.claudeDesktopToolSearchOutcomeObserver(nil, "session")(owned, claudetasks.Caller{}, json.RawMessage(`{"query":"select:Agent"}`), json.RawMessage(`{"matches":["Agent"],"query":"select:Agent","total_deferred_tools":3}`))
}
