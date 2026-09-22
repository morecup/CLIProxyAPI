package helps

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ClaudeDesktopSessionUUID preserves an internally bound side-query session
// only in the same account/profile/egress. Ordinary downstream hints keep the
// existing derivation rules and cannot inject this context value.
func ClaudeDesktopSessionUUID(ctx context.Context, auth *cliproxyauth.Auth, profileID, fallback string) string {
	binding, ok := cliproxyexecutor.ClaudeDesktopSessionBindingFromContext(ctx)
	if !ok || auth == nil || auth.ID == "" || binding.AccountID != auth.ID || binding.ProfileID != profileID || binding.Egress != auth.ProxyURL {
		return fallback
	}
	id, err := uuid.Parse(binding.SessionID)
	if err != nil || id == uuid.Nil {
		return fallback
	}
	return id.String()
}

// ClaudeDesktopPromptAccountScope keeps prompt, helper and transcript ownership
// on the same account/profile/egress partition. It is not a display account ID.
func ClaudeDesktopPromptAccountScope(auth *cliproxyauth.Auth, profileID string) string {
	if auth == nil || auth.ID == "" {
		return ""
	}
	scope, _ := json.Marshal([]string{auth.ID, profileID, auth.ProxyURL})
	return string(scope)
}

// BeginClaudeDesktopPrompt binds all Messages entry points to the same
// account/profile/session-owned loop before rendering prompt IDs on the wire.
func BeginClaudeDesktopPrompt(tracker *claudeprompt.Tracker, ctx context.Context, auth *cliproxyauth.Auth, profileID, role, sessionID, promptID, clientRequestID string, body []byte, metadata ...map[string]any) *claudeprompt.Request {
	if auth == nil || auth.ID == "" {
		return nil
	}
	scope := ClaudeDesktopPromptAccountScope(auth, profileID)
	startedAt, attempt := time.Now(), 1
	if upstream, ok := cliproxyexecutor.UpstreamAttemptFromContext(ctx); ok {
		attempt = upstream.Number
		startedAt = upstream.StartedAt
	}
	request := tracker.Begin(claudeprompt.Input{AccountID: scope, SessionID: sessionID, PromptID: promptID, ParentPromptID: ClaudeDesktopParentPromptID(ctx, metadata...), ClientRequestID: clientRequestID, Role: role, Body: body, StartedAt: startedAt, Attempt: attempt, TaskNotification: claudeprompt.SDKTaskNotificationFromContext(ctx), MetaInput: claudeprompt.SDKMetaInputFromContext(ctx)})
	if request != nil {
		for _, values := range metadata {
			if values != nil {
				values["claude_desktop_prompt_id"] = request.Identity().PromptID
			}
		}
	}
	return request
}
