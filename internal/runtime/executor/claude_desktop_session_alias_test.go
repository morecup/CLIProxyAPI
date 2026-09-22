package executor

import (
	"context"
	"testing"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClaudeDesktopSessionAliasesResolveBeforeInternalHelperBinding(t *testing.T) {
	e := newClaudeDesktopTestExecutor(t)
	e.desktopTelemetry = nil
	e.desktopATIS = newClaudeDesktopATISManager(t.TempDir(), e.desktopProfile.CodeVersion, nil)
	t.Cleanup(e.desktopATIS.Close)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	caller := uuid.NewString()
	warmSession := e.desktopATIS.featureHosts.Warm().SessionID()
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleCompaction, claudeprofile.RoleCountTokens} {
		if got := e.resolveClaudeDesktopSession(t.Context(), auth, caller, role); got != caller || e.desktopATIS.featureHosts.LookupSession(warmSession) != nil {
			t.Fatal("an unowned helper claimed the warm host")
		}
	}
	native := e.resolveClaudeDesktopSession(t.Context(), auth, caller, claudeprofile.RoleMain)
	if native != warmSession || native == caller {
		t.Fatal("fresh main did not adopt the prewarm session")
	}
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleCompaction, claudeprofile.RoleCountTokens} {
		if got := e.resolveClaudeDesktopSession(t.Context(), auth, caller, role); got != native {
			t.Fatal("owned caller alias resolved differently by request role")
		}
	}
	binding := cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: native}
	ctx := cliproxyexecutor.WithClaudeDesktopSessionBinding(context.Background(), binding)
	if got := e.resolveClaudeDesktopSession(ctx, auth, uuid.NewString(), claudeprofile.RoleCompaction); got != native {
		t.Fatal("trusted internal native session was remapped")
	}
	binding.ProfileID = "foreign-profile"
	ctx = cliproxyexecutor.WithClaudeDesktopSessionBinding(context.Background(), binding)
	unknown := uuid.NewString()
	if got := e.resolveClaudeDesktopSession(ctx, auth, unknown, claudeprofile.RoleCountTokens); got != unknown {
		t.Fatal("foreign helper context imported native session ownership")
	}
}
