package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var _ cliproxyexecutor.ClaudeDesktopSessionController = (*ClaudeAccountExecutor)(nil)
var _ cliproxyexecutor.ClaudeDesktopSessionHeartbeatController = (*ClaudeAccountExecutor)(nil)

// The control plane invokes this only after exact record/Host owner validation.
// No inbound payload or management header can supply these transcript fields.
func (e *ClaudeExecutor) desktopBridgeTranscript(auth *cliproxyauth.Auth, sessionID string) claudecontrol.BridgeTranscriptSink {
	accountID := helps.ClaudeDesktopPromptAccountScope(auth, e.desktopProfile.ProfileID)
	return func(value claudesessions.BridgeState, onFailure func(error)) error {
		return e.desktopPrompts.RecordNativeBridgeTranscript(accountID, claudeprompt.SDKBridgeTranscriptRecord{
			Type: "bridge-session", SessionID: sessionID, BridgeSessionID: value.SessionID, LastSequenceNum: value.LastSequenceNum,
			DeclaredDialogKinds: value.DeclaredDialogKinds, SessionGroupingID: value.SessionGroupingID,
			NoHistoryBackfill: value.NoHistoryBackfill, OwnerAccountUUID: value.OwnerAccountUUID, OwnerOrganizationUUID: value.OwnerOrganizationUUID,
		}, onFailure)
	}
}

func (e *ClaudeExecutor) claudeDesktopRecordOwner(auth *cliproxyauth.Auth) string {
	if e == nil || e.desktopProfile == nil || auth == nil {
		return ""
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return ""
	}
	egress := auth.ProxyURL
	if egress == "" && e.cfg != nil {
		egress = e.cfg.ProxyURL
	}
	payload, _ := json.Marshal([]string{"desktop-session-record-owner-v1", auth.ID, e.desktopProfile.ProfileID, egress, claudeDesktopATISIdentityHash(enrollment)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// Management operations never provision an account or start background senders.
// The active runtime reference prevents account shutdown during the operation.
func (e *ClaudeAccountExecutor) acquireDesktopSessionRuntime(authID string) (*claudeAccountRuntime, error) {
	if e == nil || authID == "" {
		return nil, claudesessions.ErrUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.stoppingRuntimeLocked(authID) != nil {
		return nil, claudesessions.ErrUnavailable
	}
	runtime := e.runtimes[authID]
	if runtime == nil || !runtime.acquire() {
		return nil, claudesessions.ErrUnavailable
	}
	return runtime, nil
}

func (e *ClaudeAccountExecutor) ListDesktopSessions(authID string) ([]cliproxyexecutor.ClaudeDesktopSession, error) {
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return nil, err
	}
	defer runtime.release()
	values, observed, err := runtime.executor.desktopATIS.desktopRecords.ListObserved(runtime.recordOwner)
	result := make([]cliproxyexecutor.ClaudeDesktopSession, len(values))
	for index, value := range values {
		result[index] = cliproxyexecutor.ClaudeDesktopSession(value)
	}
	if err == nil {
		runtime.executor.desktopTelemetry.RecordDesktopSessionListLoaded(authID, observed.Duration, len(values), observed.Cached)
	}
	return result, err
}

func (e *ClaudeAccountExecutor) CheckDesktopSessionHeartbeats(ctx context.Context, authID string) error {
	if ctx == nil || ctx.Err() != nil {
		return claudesessions.ErrInvalid
	}
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return err
	}
	defer runtime.release()
	inner := runtime.executor
	observed, err := inner.desktopATIS.desktopRecords.HeartbeatCheckBatch(runtime.recordOwner)
	if err != nil {
		return err
	}
	return inner.desktopTelemetry.RecordDesktopSessionHeartbeatBatch(auth, claudetelemetry.SessionHeartbeatBatch{
		Sent: observed.Sent, Fresh: observed.Fresh, ProbeDispatched: observed.ProbeDispatched,
		NoWorker: observed.NoWorker, RecentlyChecked: observed.RecentlyChecked, Unknown: observed.Unknown,
	})
}

func (e *ClaudeAccountExecutor) StopDesktopSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionStop) (cliproxyexecutor.ClaudeDesktopSession, error) {
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	value, err := inner.desktopATIS.desktopRecords.Stop(ctx, runtime.recordOwner, operation.SessionID, operation.ExpectedQueryID,
		func(before claudesessions.Snapshot) (func() error, error) {
			emit, errPrepare := inner.desktopTelemetry.PrepareDesktopSessionStop(authID, before.ID, before.QueryID)
			if errPrepare != nil {
				return nil, errPrepare
			}
			return emit, inner.desktopControlPlane.PrepareQueryStop(before.ID, before.QueryID)
		}, inner.desktopATIS.featureHosts.Retire)
	// Wait outside the record admission lock. Unrelated records remain usable;
	// a successor joins its own bridge cleanup before reattaching that remote ID.
	if value.ID == operation.SessionID && value.QueryID == operation.ExpectedQueryID && !value.Running {
		runtime.mu.Lock()
		input := runtime.remoteInputs[value.QueryID]
		runtime.mu.Unlock()
		if input != nil {
			input.Stop()
			// Done includes the final outcome callback, not just cancellation
			// of inference. No record/runtime lock may be held while joining.
			select {
			case <-input.Done():
			case <-ctx.Done():
				err = errors.Join(err, ctx.Err())
			}
		}
		err = errors.Join(err, inner.desktopATIS.desktopRecords.WaitLocalTurn(ctx, runtime.recordOwner, value.ID, value.QueryID))
		err = errors.Join(err, inner.desktopControlPlane.RetireQuery(ctx, value.ID, value.QueryID))
	}
	return cliproxyexecutor.ClaudeDesktopSession(value), err
}
