package executor

import (
	"context"
	"time"

	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// recordDesktopResumeLifecycle records only an admitted durable resume. The
// gateway has no separate warm-prefix or scheduled-task state, so no_prior and
// false are facts about this owned path rather than guesses about Claude Code.
func (e *ClaudeExecutor) recordDesktopResumeLifecycle(ctx context.Context, auth *cliproxyauth.Auth, value claudesessions.Snapshot, started time.Time, resolve, scan time.Duration) {
	if e == nil || auth == nil || e.desktopTelemetry == nil || value.ID == "" || value.SDKSessionID == "" {
		return
	}
	elapsed := time.Since(started)
	facts := claudetelemetry.DesktopCodeLifecycleFacts{
		SessionID: value.ID, SDKSessionID: value.SDKSessionID,
		ResolveDuration: resolve, ScanDuration: scan, TotalDuration: elapsed,
		WarmDuration: elapsed, PrefixParity: "no_prior", HadScheduledTask: false,
	}
	for _, record := range []struct {
		name string
		fn   func() error
	}{
		{"idle warm start", func() error { return e.desktopTelemetry.RecordDesktopSessionIdleWarmStart(auth, facts) }},
		{"resume bring-home timing", func() error { return e.desktopTelemetry.RecordDesktopResumeBringHomeTiming(auth, facts) }},
		{"idle warm completion", func() error { return e.desktopTelemetry.RecordDesktopSessionIdleWarmComplete(auth, facts) }},
	} {
		if err := record.fn(); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: " + record.name + " telemetry could not be persisted")
		}
	}
}
