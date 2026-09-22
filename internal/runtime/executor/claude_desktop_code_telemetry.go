package executor

import (
	"context"
	"time"

	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (e *ClaudeExecutor) claudeDesktopCompactHookTelemetryRunner(auth *cliproxyauth.Auth, session, model, promptID string, runner claudeprompt.SDKCompactHookRunner) claudeprompt.SDKCompactHookRunner {
	if runner == nil {
		return nil
	}
	return func(ctx context.Context, input claudeprompt.SDKCompactHookInput) ([]claudeprompt.SDKCompactHookExecution, error) {
		started := time.Now()
		executions, err := runner(ctx, input)
		if e != nil && e.desktopTelemetry != nil && auth != nil && len(executions) > 0 {
			recordCtx := context.Background()
			if ctx != nil {
				recordCtx = context.WithoutCancel(ctx)
			}
			if errRecord := e.desktopTelemetry.RecordSDKCompactHook(recordCtx, auth, session, model, promptID, input, executions, time.Since(started), err == nil); errRecord != nil {
				helps.LogWithRequestID(recordCtx).WithError(errRecord).Warn("claude desktop: compact hook telemetry was not persisted")
			}
		}
		return executions, err
	}
}

func (e *ClaudeExecutor) claudeDesktopShellTelemetryObserver(auth *cliproxyauth.Auth, host *claudefeatures.Host) func(context.Context, claudefeatures.ShellTelemetryEvent) error {
	return func(ctx context.Context, event claudefeatures.ShellTelemetryEvent) error {
		if e == nil || e.desktopTelemetry == nil || auth == nil || host == nil {
			return nil
		}
		return e.desktopTelemetry.RecordSDKShellCommand(ctx, auth, host.SessionID(), event.Model, event.PromptID, event.Name, event.Metadata)
	}
}
