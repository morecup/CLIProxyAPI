package helps

import (
	"bytes"
	"sync"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ClaudeDesktopSDKAccounting observes the physical API outcome even when
// telemetry is disabled or its workers cannot initialize. Content is transient;
// only a completed compact response's fingerprint enters the parent's ledger.
type ClaudeDesktopSDKAccounting struct {
	sessionStateError  error
	mu                 sync.Mutex
	main               *claudeprompt.Request
	helper             *claudeprompt.SDKHelperOwner
	role               claudeprofile.RequestRole
	clientID           string
	desktopVersion     string
	codeVersion        string
	startedAt          time.Time
	chainStartedAt     time.Time
	requestObserved    bool
	responseStatus     int
	finished           bool
	response           claudeprompt.Response
	compactionInput    claudeprompt.SDKCompactionInput
	compactionResponse claudeprompt.SDKCompactionResponse
}

func (a *ClaudeDesktopSDKAccounting) SDKSessionStateError() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessionStateError != nil {
		return a.sessionStateError
	}
	if a.main != nil {
		return a.main.SDKSessionStateError()
	}
	return a.helper.SDKSessionStateError()
}

func BeginClaudeDesktopSDKAccounting(tracker *claudeprompt.Tracker, auth *cliproxyauth.Auth, bundle *claudeprofile.Bundle, main *claudeprompt.Request, role claudeprofile.RequestRole, sessionID, parentID, clientID string, startedAt, chainStartedAt time.Time) *ClaudeDesktopSDKAccounting {
	if auth == nil || auth.ID == "" || bundle == nil || role == claudeprofile.RoleCountTokens {
		return nil
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	if chainStartedAt.IsZero() || chainStartedAt.After(startedAt) {
		chainStartedAt = startedAt
	}
	value := &ClaudeDesktopSDKAccounting{main: main, role: role, clientID: clientID,
		desktopVersion: bundle.DesktopVersion, codeVersion: bundle.CodeVersion, startedAt: startedAt, chainStartedAt: chainStartedAt}
	if role != claudeprofile.RoleMain {
		scope := ClaudeDesktopPromptAccountScope(auth, bundle.ProfileID)
		value.helper = tracker.BindSDKHelper(claudeprompt.Input{AccountID: scope, SessionID: sessionID, ParentPromptID: parentID, StartedAt: startedAt})
		// A failed restore can prevent binding the parent altogether. Keep the
		// scoped store error even without an owner; nil is not healthy state.
		value.sessionStateError = tracker.SDKSessionStateError(scope, sessionID)
	}
	return value
}

func (a *ClaudeDesktopSDKAccounting) ObserveRequest(body []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished || a.requestObserved {
		return
	}
	a.requestObserved = true
	if a.role == claudeprofile.RoleCompaction {
		a.compactionInput = claudeprompt.ObserveSDKCompactionInput(body)
	}
}

func (a *ClaudeDesktopSDKAccounting) ObserveHTTPResponse(status int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.finished {
		a.responseStatus = status
	}
}

func (a *ClaudeDesktopSDKAccounting) ObservePayload(payload []byte, streaming bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished || !a.requestObserved || a.responseStatus < 200 || a.responseStatus >= 300 {
		return
	}
	a.response.ObservePayload(payload, streaming)
	if a.role == claudeprofile.RoleCompaction {
		if streaming {
			for _, line := range bytes.Split(payload, []byte{'\n'}) {
				a.compactionResponse.ObserveStreamLine(line)
			}
		} else {
			a.compactionResponse.ObserveJSON(payload)
		}
	}
}

func (a *ClaudeDesktopSDKAccounting) ObserveStreamLine(line []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished || !a.requestObserved || a.responseStatus < 200 || a.responseStatus >= 300 {
		return
	}
	a.response.ObserveStreamLine(line)
	if a.role == claudeprofile.RoleCompaction {
		a.compactionResponse.ObserveStreamLine(line)
	}
}

// FinishSuccess is called after the main owner settles response decoding and
// before telemetry assembles its result snapshot. Both use this same end clock.
func (a *ClaudeDesktopSDKAccounting) FinishSuccess(at time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	defer a.releaseHelperLocked()
	defer a.compactionResponse.Discard()
	_, _, complete := a.response.Outcome()
	if !a.requestObserved || a.responseStatus < 200 || a.responseStatus >= 300 || !complete {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	duration := at.Sub(a.startedAt)
	if duration < 0 {
		duration = 0
	}
	duration += a.startedAt.Sub(a.chainStartedAt)
	durationMS := duration.Round(time.Millisecond).Milliseconds()
	if a.role == claudeprofile.RoleMain {
		if err := a.main.RecordSDKAPISuccess(durationMS); err != nil && a.sessionStateError == nil {
			a.sessionStateError = err
		}
		return
	}
	if a.helper == nil {
		return
	}
	switch a.role {
	case claudeprofile.RoleTitle:
		a.helper.RecordSuccess("generate_session_title", a.clientID, durationMS)
	case claudeprofile.RoleLightHelper:
		a.helper.RecordSuccess("prompt_suggestion", a.clientID, durationMS)
	case claudeprofile.RoleCompaction:
		summary := a.compactionResponse.TakeSummary(a.desktopVersion, a.codeVersion)
		a.helper.RecordCompactionSuccess("compact", a.clientID, durationMS, claudeprompt.CompletedSDKCompaction(a.compactionInput, summary))
	case claudeprofile.RoleSecurityMonitor:
		// The native classifier callback does not write the shared API ledger.
	default:
		a.helper.ObserveUnmodeledHelper()
	}
}

func (a *ClaudeDesktopSDKAccounting) FinishFailure() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	defer a.releaseHelperLocked()
	a.compactionResponse.Discard()
}

func (a *ClaudeDesktopSDKAccounting) releaseHelperLocked() {
	if a.helper == nil {
		return
	}
	// Preserve an observed persistence failure after the lease is released and
	// its idle scope can be evicted. Cache eviction is not a healthy recovery.
	if err := a.helper.SDKSessionStateError(); err != nil && a.sessionStateError == nil {
		a.sessionStateError = err
	}
	a.helper.Close()
}
