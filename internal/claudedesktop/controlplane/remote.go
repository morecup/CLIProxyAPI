package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func controlResponseStatus(response *http.Response, body []byte, now time.Time) *statusError {
	err := &statusError{code: response.StatusCode, untrustedDevice: response.StatusCode == http.StatusForbidden && isUntrustedDevice(body)}
	if response.StatusCode == http.StatusConflict {
		err.conflictReason = workerConflictReason(response.Header, body)
	}
	var data struct {
		Error struct {
			Resource string `json:"resource"`
		} `json:"error"`
	}
	if response.StatusCode == http.StatusForbidden && json.Unmarshal(body, &data) == nil {
		err.staleRelogin = data.Error.Resource == "session_stale_relogin"
	}
	if seconds, parseErr := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); parseErr == nil && seconds > 0 {
		err.retryAfter = time.Duration(min(seconds, 30) * float64(time.Second))
	} else if at, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil && at.After(now) {
		err.retryAfter = at.Sub(now)
	}
	return err
}

// AttachRemoteQuery starts an idle, owned SDK bridge without inventing a main
// user message. Repeated attempts address the same query and remote origin.
func (m *Manager) AttachRemoteQuery(ctx context.Context, auth *cliproxyauth.Auth, origin *claudesessions.RemoteGrant,
	inbound *InboundConsumer, bind func(string) error, transcript ...BridgeTranscriptSink) error {
	if m == nil || !m.Enabled() || auth == nil || inbound == nil || bind == nil {
		return claudesessions.ErrUnavailable
	}
	value, host, _, _, model, err := origin.Read()
	if err != nil {
		return err
	}
	enrollment, err := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return err
	}
	facts := RequestFacts{Role: claudeprofile.RoleMain, DesktopSessionID: value.ID, LocalSessionID: value.SDKSessionID,
		QueryID: value.QueryID, QueryLifetime: host.Context(), RequireQueryOwnership: true, Model: model,
		RemoteOrigin: origin, Inbound: inbound, BindBridge: bind, BridgeRecord: origin.BridgeRecord()}
	if len(transcript) == 1 {
		facts.BridgeTranscript = transcript[0]
	}
	session, err := m.sessionForQuery(auth, enrollment, facts)
	if err != nil {
		return err
	}
	return session.ensure(ctx)
}

func (s *sessionRuntime) reviveRemoteLocked(ctx context.Context) error {
	remoteID := s.state.RemoteSessionID
	if s.remoteOrigin != nil {
		value, _, id, _, _, err := s.remoteOrigin.Read()
		if err != nil {
			return err
		}
		if value.ID != s.desktopID || value.QueryID != s.queryID || value.SDKSessionID != s.state.LocalSessionID {
			return claudesessions.ErrStaleQuery
		}
		remoteID = id
	}
	if claudesessions.NormalizeRemoteSessionID(remoteID) != remoteID || remoteID == "" {
		return claudesessions.ErrInvalid
	}
	s.state.RemoteSessionID = remoteID
	s.state.ArchiveSessionID = "session_" + strings.TrimPrefix(remoteID, "cse_")
	code := 0
	err := s.doJSONObserved(ctx, claudeprofile.ControlEndpointSessionUnarchive, s.state.ArchiveSessionID, struct{}{}, nil, &code)
	if code == http.StatusUnauthorized && s.manager.credentials != nil && ctx.Err() == nil {
		current, errRefresh := s.manager.credentials.Refresh(ctx, s.auth.Clone())
		if errRefresh != nil {
			return errors.Join(err, errRefresh)
		}
		if errRefresh = s.useCredentialLocked(current); errRefresh != nil {
			return errRefresh
		}
		err = s.doJSONObserved(ctx, claudeprofile.ControlEndpointSessionUnarchive, s.state.ArchiveSessionID, struct{}{}, nil, &code)
	}
	if err == nil || code == http.StatusConflict {
		s.state.Archived = false
		s.remoteRevived = true
		return s.persistLocked()
	}
	var status *statusError
	if errors.As(err, &status) && !status.untrustedDevice && !status.staleRelogin && (code == 400 || code == 403 || code == 404) {
		// Native non-revive attachment can mint a replacement for a gone
		// pointer. The record consumer then detects the ID mismatch, detaches
		// the origin and cleans up only this newly created replacement.
		s.resetRemoteLocked()
		if errCreate := s.createSessionLocked(ctx); errCreate != nil {
			return errors.Join(err, errCreate)
		}
		s.createdFallback, s.remoteRevived = true, true
		return nil
	}
	return fmt.Errorf("reattach Claude Desktop bridge: %w", err)
}
