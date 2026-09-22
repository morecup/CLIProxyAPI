package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Lane W5 (desktop-process2): Desktop 1.40609 main-process events emitted at
// application activation. Native pins live in
// testdata/desktop-telemetry-process2-native.json (produced by
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-process2-source.mjs).

// FactDesktopWindowsElevationDetected maps to desktop_windows_elevation_detected:
// the app-ready handler read the process token elevation type through the
// native addon and logged it once per application launch.
const FactDesktopWindowsElevationDetected = "desktop_windows_elevation_detected"

// Addon enum returned by getWindowsElevationType(); the two native consumers
// only compare against these literals (anything else is treated as unknown).
// They mirror TOKEN_ELEVATION_TYPE of the process token.
const (
	desktopElevationTypeDefault = "default"
	desktopElevationTypeFull    = "full"
	desktopElevationTypeLimited = "limited"
)

// TOKEN_ELEVATION_TYPE values (winnt.h).
const (
	tokenElevationTypeDefault uint32 = 1
	tokenElevationTypeFull    uint32 = 2
	tokenElevationTypeLimited uint32 = 3
)

// desktopWindowsElevationMetadata mirrors the native builder key order:
// {elevation_type, can_elevate}.
type desktopWindowsElevationMetadata struct {
	ElevationType string `json:"elevation_type"`
	CanElevate    bool   `json:"can_elevate"`
}

func (m desktopWindowsElevationMetadata) toMap() map[string]any {
	return map[string]any{
		"elevation_type": m.ElevationType,
		"can_elevate":    m.CanElevate,
	}
}

// desktopElevationTypeName maps the token TOKEN_ELEVATION_TYPE value onto the
// addon string enum. Unknown values are not reported (the native try/catch
// suppresses the event when detection fails).
func desktopElevationTypeName(kind uint32) (string, bool) {
	switch kind {
	case tokenElevationTypeDefault:
		return desktopElevationTypeDefault, true
	case tokenElevationTypeFull:
		return desktopElevationTypeFull, true
	case tokenElevationTypeLimited:
		return desktopElevationTypeLimited, true
	default:
		return "", false
	}
}

// buildDesktopWindowsElevationMetadata is the native builder:
// {elevation_type:t,can_elevate:t===`limited`||t===`full`}.
func buildDesktopWindowsElevationMetadata(elevationType string) desktopWindowsElevationMetadata {
	return desktopWindowsElevationMetadata{
		ElevationType: elevationType,
		CanElevate:    elevationType == desktopElevationTypeLimited || elevationType == desktopElevationTypeFull,
	}
}

// desktopActivationHook runs on the renderer (desktop-event-logging) worker
// when an account activates its emulated Desktop main process, which is the
// gateway's counterpart of the native app-ready handler.
type desktopActivationHook func(m *Manager, worker *accountWorker)

var desktopActivationHooks []desktopActivationHook

func registerDesktopActivationHook(hook desktopActivationHook) {
	if hook != nil {
		desktopActivationHooks = append(desktopActivationHooks, hook)
	}
}

// runDesktopActivationHooks is the single insertion point used by
// manager.go Activate. Hook failures never block activation.
func (m *Manager) runDesktopActivationHooks(worker *accountWorker) {
	if m == nil || worker == nil {
		return
	}
	for _, hook := range desktopActivationHooks {
		hook(m, worker)
	}
}

// desktopElevationReported dedupes the once-per-launch event per emulated
// main process (one renderer worker per account for the Manager lifetime).
var desktopElevationReported sync.Map

func emitDesktopWindowsElevationDetected(m *Manager, worker *accountWorker) {
	if m == nil || worker == nil {
		return
	}
	if _, declared := m.profile.Events[FactDesktopWindowsElevationDetected]; !declared {
		return
	}
	elevationType, ok := probeDesktopWindowsElevation()
	if !ok {
		return
	}
	if _, loaded := desktopElevationReported.LoadOrStore(worker, struct{}{}); loaded {
		return
	}
	metadata := buildDesktopWindowsElevationMetadata(elevationType)
	if errEnqueue := worker.enqueueProjected(m.ctx, FactDesktopWindowsElevationDetected, "", "", metadata.toMap()); errEnqueue != nil {
		desktopElevationReported.Delete(worker)
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: windows elevation event was not persisted")
	}
}

// FactDesktopOAuthFailed maps to desktop_oauth_failed: an OAuth token flow of
// the main process finished with a failure result (gPt: !lk(t)).
const FactDesktopOAuthFailed = "desktop_oauth_failed"

// DesktopOAuthTypeRefresh is the gPt oauth_type literal of the refresh flow
// (gk -> FPt). The gateway only performs refreshes; `initial` (browser cookie
// exchange) never happens here.
const DesktopOAuthTypeRefresh = "refresh"

// Refresh-flow failure result `type` literals (FPt).
const (
	desktopOAuthFailureNetworkError = "network_error"
	desktopOAuthFailureServerError  = "server_error"
	desktopOAuthFailureAuthError    = "auth_error"
)

// DesktopOAuthFailure is the pinned failure result: `status` exists only for
// HTTP outcomes (server_error / auth_error), never for network_error.
type DesktopOAuthFailure struct {
	Reason    string
	Status    int
	HasStatus bool
}

// desktopOAuthFailedMetadata mirrors the native builder key order:
// {oauth_type, failure_reason, status?}. The `client` key only exists for the
// `user:profile org:desktop_config` scope, which the gateway never requests.
type desktopOAuthFailedMetadata struct {
	OAuthType     string `json:"oauth_type"`
	FailureReason string `json:"failure_reason"`
	Status        *int   `json:"status,omitempty"`
}

func (m desktopOAuthFailedMetadata) toMap() map[string]any {
	out := map[string]any{
		"oauth_type":     m.OAuthType,
		"failure_reason": m.FailureReason,
	}
	if m.Status != nil {
		out["status"] = *m.Status
	}
	return out
}

func buildDesktopOAuthFailedMetadata(oauthType string, failure DesktopOAuthFailure) desktopOAuthFailedMetadata {
	metadata := desktopOAuthFailedMetadata{OAuthType: oauthType, FailureReason: failure.Reason}
	if failure.HasStatus {
		status := failure.Status
		metadata.Status = &status
	}
	return metadata
}

// ClassifyDesktopOAuthRefreshError maps the gateway refresh error onto the
// native FPt result: a transport failure of the token POST is network_error
// (no status); a non-200 response is server_error (>= 500) or auth_error with
// its status. Every other failure (response parse, missing access_token,
// missing refresh token, invalid enrollment) has no native typed result — the
// native flow throws there and logs nothing — so it is not reported.
func ClassifyDesktopOAuthRefreshError(err error) (DesktopOAuthFailure, bool) {
	if err == nil {
		return DesktopOAuthFailure{}, false
	}
	var statusErr *claudedesktop.HTTPStatusError
	if errors.As(err, &statusErr) {
		reason := desktopOAuthFailureAuthError
		if statusErr.Status >= 500 {
			reason = desktopOAuthFailureServerError
		}
		return DesktopOAuthFailure{Reason: reason, Status: statusErr.Status, HasStatus: true}, true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return DesktopOAuthFailure{Reason: desktopOAuthFailureNetworkError}, true
	}
	return DesktopOAuthFailure{}, false
}

// RecordDesktopOAuthFailed emits desktop_oauth_failed on the renderer worker
// of the account whose token flow failed. Like the update-check facts it is an
// application fact without a session. Undeclared profiles stay silent.
func (m *Manager) RecordDesktopOAuthFailed(ctx context.Context, auth *cliproxyauth.Auth, oauthType string, failure DesktopOAuthFailure) error {
	if m == nil || !m.Enabled() || m.ctx.Err() != nil {
		return nil
	}
	if _, declared := m.profile.Events[FactDesktopOAuthFailed]; !declared {
		return nil
	}
	if strings.TrimSpace(oauthType) == "" || strings.TrimSpace(failure.Reason) == "" {
		return fmt.Errorf("desktop oauth failure needs an oauth_type and a failure_reason")
	}
	worker, errWorker := m.workerForDelivery(auth, m.rendererDelivery)
	if errWorker != nil {
		return errWorker
	}
	if ctx == nil {
		ctx = m.ctx
	}
	metadata := buildDesktopOAuthFailedMetadata(oauthType, failure)
	if errEnqueue := worker.enqueueProjected(ctx, FactDesktopOAuthFailed, "", "", metadata.toMap()); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

func init() {
	registerExecutableEvents("desktop-event-logging", map[string]string{
		FactDesktopWindowsElevationDetected: "desktop_windows_elevation_detected",
		FactDesktopOAuthFailed:              "desktop_oauth_failed",
	})
	registerDesktopActivationHook(emitDesktopWindowsElevationDetected)
}
