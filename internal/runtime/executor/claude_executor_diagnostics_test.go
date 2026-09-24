package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopHelperDiagnosticsDoNotInheritOrCommitMainChain(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleWebSearchHelper, claudeprofile.RoleSecurityMonitor, claudeprofile.RoleCountTokens} {
		t.Run(string(role), func(t *testing.T) {
			auth := &cliproxyauth.Auth{ID: uuid.NewString()}
			session := uuid.NewString()
			_, main := injectClaudeDiagnosticsForRole([]byte(`{"messages":[]}`), auth, session, claudeprofile.RoleMain)
			commitClaudeDiagnostics(main, "msg_main")
			body, helper := injectClaudeDiagnosticsForRole([]byte(`{"messages":[],"diagnostics":{"previous_message_id":"msg_wrong"}}`), auth, session, role)
			if gjson.GetBytes(body, "diagnostics").Exists() || helper.key != "" || helper.sequence != 0 {
				t.Fatal("helper inherited a diagnostics chain")
			}
			commitClaudeDiagnostics(helper, "msg_helper")
			next, _ := injectClaudeDiagnosticsForRole([]byte(`{"messages":[]}`), auth, session, claudeprofile.RoleMain)
			if gjson.GetBytes(next, "diagnostics.previous_message_id").String() != "msg_main" {
				t.Fatal("helper changed main diagnostics")
			}
		})
	}
}

func TestClaudeDesktopCurrentCompactionContinuesDiagnosticsChain(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinCurrent()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	executor := &ClaudeExecutor{desktopOnly: true, desktopProfile: bundle}
	auth := &cliproxyauth.Auth{ID: uuid.NewString()}
	session := uuid.NewString()
	_, main := injectClaudeDiagnosticsForRole([]byte(`{"messages":[]}`), auth, session, claudeprofile.RoleMain)
	commitClaudeDiagnostics(main, "msg_main")

	body, compaction := executor.injectClaudeDesktopDiagnosticsForRole([]byte(`{"model":"claude-opus-5-5","messages":[]}`), auth, session, claudeprofile.RoleCompaction)
	if got := gjson.GetBytes(body, "diagnostics.previous_message_id").String(); got != "msg_main" {
		t.Fatalf("compaction previous_message_id = %q, want msg_main", got)
	}
	if compaction.key == "" || compaction.sequence == 0 {
		t.Fatal("compaction did not reserve diagnostics continuity")
	}
	commitClaudeDiagnostics(compaction, "msg_compaction")

	next, _ := injectClaudeDiagnosticsForRole([]byte(`{"messages":[]}`), auth, session, claudeprofile.RoleMain)
	if got := gjson.GetBytes(next, "diagnostics.previous_message_id").String(); got != "msg_compaction" {
		t.Fatalf("main previous_message_id = %q, want msg_compaction", got)
	}
}

func TestClaudeDesktopHistoricalCompactionDoesNotUseDiagnosticsChain(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	executor := &ClaudeExecutor{desktopOnly: true, desktopProfile: bundle}
	body, state := executor.injectClaudeDesktopDiagnosticsForRole(
		[]byte(`{"model":"claude-opus-5","messages":[]}`),
		&cliproxyauth.Auth{ID: uuid.NewString()},
		uuid.NewString(),
		claudeprofile.RoleCompaction,
	)
	if gjson.GetBytes(body, "diagnostics").Exists() || state.key != "" || state.sequence != 0 {
		t.Fatalf("historical compaction unexpectedly joined diagnostics chain: body=%s state=%+v", body, state)
	}
}

func TestInjectClaudeDiagnosticsMatchesNativeFieldOrderAndContinuity(t *testing.T) {
	t.Parallel()

	body := []byte(`{"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"max_tokens":1,"messages":[]}`)
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{ID: "credential-diagnostics-order-" + testID}
	first, state := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	wantOrder := `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"diagnostics":{"previous_message_id":null},"max_tokens"`
	if !bytes.Contains(first, []byte(wantOrder)) {
		t.Fatalf("diagnostics field order differs from native: %s", first)
	}
	if got := gjson.GetBytes(first, "diagnostics.previous_message_id"); got.Type != gjson.Null {
		t.Fatalf("first previous_message_id = %s, want null", got.Raw)
	}

	commitClaudeDiagnostics(state, "msg_01ABCDEF0123456789ABCDEFG")
	second, _ := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	if got := gjson.GetBytes(second, "diagnostics.previous_message_id").String(); got != "msg_01ABCDEF0123456789ABCDEFG" {
		t.Fatalf("second previous_message_id = %q, want committed upstream ID", got)
	}
}

func TestClaudeMessageIDFromSSECommitsOnlyCompletedMessage(t *testing.T) {
	t.Parallel()

	complete := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_complete\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if got := claudeMessageIDFromSSE(complete); got != "msg_complete" {
		t.Fatalf("completed SSE message ID = %q, want msg_complete", got)
	}
	incomplete := []byte(strings.Replace(string(complete), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1))
	if got := claudeMessageIDFromSSE(incomplete); got != "" {
		t.Fatalf("incomplete SSE message ID = %q, want empty", got)
	}
}
