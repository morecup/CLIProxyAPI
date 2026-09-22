package executor

import (
	"context"
	"sync"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// claudeDesktopResumeTelemetryKey carries the telemetry owner, account and
// owned model of one remote hydration through the restore context so the
// package-level helps resume observer can emit tengu_session_resumed for the
// right session without global executor state.
type claudeDesktopResumeTelemetryKey struct{}

type claudeDesktopResumeTelemetry struct {
	telemetry *claudetelemetry.Manager
	auth      *cliproxyauth.Auth
	model     string
}

var installClaudeDesktopResumeObserver sync.Once

// withClaudeDesktopResumeTelemetry installs the shared resume observer once
// and scopes this hydration's telemetry identity to ctx. The observer fires
// on the adoption success path of helps.HydrateClaudeDesktopRemote (native
// print-lane tengu_session_resumed, entrypoint "print", success true).
func withClaudeDesktopResumeTelemetry(ctx context.Context, manager *claudetelemetry.Manager, auth *cliproxyauth.Auth, model string) context.Context {
	installClaudeDesktopResumeObserver.Do(func() {
		helps.ClaudeDesktopRemoteResumeObserver = func(ctx context.Context, resumed helps.ClaudeDesktopRemoteResumed) {
			if ctx == nil {
				return
			}
			scope, ok := ctx.Value(claudeDesktopResumeTelemetryKey{}).(claudeDesktopResumeTelemetry)
			if !ok || scope.telemetry == nil {
				return
			}
			err := scope.telemetry.RecordSDKSessionResumed(ctx, scope.auth, resumed.Session, scope.model, "", claudetelemetry.SessionResumed{
				InterruptionKind: claudetelemetry.SessionResumedInterruptionNone, Duration: resumed.Duration,
			})
			if err != nil {
				helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: session resumed telemetry was not persisted")
			}
		}
	})
	if ctx == nil || manager == nil {
		return ctx
	}
	return context.WithValue(ctx, claudeDesktopResumeTelemetryKey{}, claudeDesktopResumeTelemetry{telemetry: manager, auth: auth, model: model})
}
