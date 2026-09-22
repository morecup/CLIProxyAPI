package helps

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopContinuationIsolationAndLifetime(t *testing.T) {
	for _, mode := range []string{"same", "already-restored", "account", "profile", "egress", "session", "client", "changed-body", "independent-helper", "released", "cancelled", "finalized", "still-running"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
			release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
			defer release()
			query := cliproxyexecutor.WithFreshUpstreamQuery(ctx, time.Now())
			auth := &cliproxyauth.Auth{ID: "synthetic-account", ProxyURL: "http://synthetic-proxy"}
			profile, session := "synthetic-profile", uuid.NewString()
			prompt, originalID, continuedID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			source := []byte(`{"model":"synthetic","messages":[{"role":"user","content":"original"}]}`)
			continued := []byte(`{"model":"synthetic","messages":[{"role":"user","content":"continued"}]}`)
			var tracker claudeprompt.Tracker
			scope, _ := json.Marshal([]string{auth.ID, profile, auth.ProxyURL})
			owner := tracker.Begin(claudeprompt.Input{AccountID: string(scope), SessionID: session, PromptID: prompt, ClientRequestID: continuedID, Role: "main", Body: continued})
			owner.ObserveSDKQuery(continued)
			if mode != "still-running" {
				owner.FinishFailure()
			}
			RememberClaudeDesktopContinuation(ctx, query, auth, profile, session, originalID, source, continued, owner, continuedID)
			key := claudeDesktopContinuationScope(auth, profile, session, originalID)
			saved, ok := cliproxyexecutor.LoadUpstreamInvocationValue(ctx, key)
			if !ok {
				t.Fatal("continuation was not retained")
			}
			serialized, _ := json.Marshal(saved)
			if string(serialized) != "{}" {
				t.Fatal("saved request content is serializable")
			}
			switch mode {
			case "already-restored":
				source = continued
			case "account":
				auth.ID = "another-account"
			case "profile":
				profile = "another-profile"
			case "egress":
				auth.ProxyURL = "http://another-proxy"
			case "session":
				session = uuid.NewString()
			case "client":
				originalID = uuid.NewString()
			case "changed-body":
				source = []byte(`{"messages":[{"role":"user","content":"changed"}]}`)
			case "independent-helper":
				ctx = cliproxyexecutor.WithIndependentUpstreamAttempt(ctx, time.Now())
			case "released":
				release()
			case "cancelled":
				cancel()
			case "finalized":
				owner.FinalizeFailure(time.Now())
			}
			next, body, nextPrompt, client := ResumeClaudeDesktopContinuation(ctx, auth, profile, session, prompt, originalID, source)
			if mode == "same" || mode == "already-restored" {
				if next == ctx || gjson.GetBytes(body, "messages.0.content").String() != "continued" || client != continuedID || nextPrompt != prompt {
					t.Fatal("exact owner did not resume compacted history")
				}
				attempt, _ := cliproxyexecutor.UpstreamAttemptFromContext(next)
				if attempt.Number != 2 {
					t.Fatal("resumed request did not advance its API query")
				}
				if !cliproxyexecutor.RegisterUpstreamFailureFinalizer(next, "synthetic", func() {}) {
					t.Fatal("lost outer finalizer owner")
				}
			} else if next != ctx || string(body) != string(source) || client != originalID || nextPrompt != prompt {
				t.Fatal("unrelated, closed or active query borrowed a continuation")
			}
			release()
			if _, ok := cliproxyexecutor.LoadUpstreamInvocationValue(query, key); ok {
				t.Fatal("scope completion retained plaintext")
			}
		})
	}
}
