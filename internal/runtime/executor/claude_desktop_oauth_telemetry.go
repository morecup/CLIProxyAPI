package executor

import (
	"context"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// recordDesktopOAuthRefreshFailure mirrors the native refresh flow (gk ->
// gPt(`refresh`, result)): only failures with a pinned typed result
// (network_error / server_error / auth_error) become desktop_oauth_failed.
func (e *ClaudeExecutor) recordDesktopOAuthRefreshFailure(ctx context.Context, auth *cliproxyauth.Auth, errRefresh error) {
	if e == nil || e.desktopTelemetry == nil || auth == nil {
		return
	}
	failure, ok := claudetelemetry.ClassifyDesktopOAuthRefreshError(errRefresh)
	if !ok {
		return
	}
	if errRecord := e.desktopTelemetry.RecordDesktopOAuthFailed(ctx, auth, claudetelemetry.DesktopOAuthTypeRefresh, failure); errRecord != nil {
		log.WithError(errRecord).Debug("claude desktop executor: oauth failure telemetry was not persisted")
	}
}
