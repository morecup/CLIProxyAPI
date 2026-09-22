package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// PrepareDesktopSessionStop is called only by the typed user-stop operation,
// before query cancellation clears its pending state. Transport disconnects do
// not infer a user action. Missing Renderer state means no visible cycle existed.
func (m *Manager) PrepareDesktopSessionStop(authID, recordID, queryID string) (func() error, error) {
	return m.PrepareDesktopSessionRetirement(authID, recordID, queryID, true)
}

// PrepareDesktopSessionRetirement snapshots without closing renderer state.
// The returned callback is the commit boundary; a later failed control-plane
// preparation must leave both the live query and its telemetry state intact.
func (m *Manager) PrepareDesktopSessionRetirement(authID, recordID, queryID string, userStop bool) (func() error, error) {
	if m == nil || !m.Enabled() {
		return nil, nil
	}
	m.lifecycleMu.Lock()
	var state *rendererSessionState
	for _, candidate := range m.sessions {
		if candidate.worker != nil && candidate.worker.binding.AuthID == authID && candidate.facts.DesktopSessionID == recordID {
			state = candidate
			break
		}
	}
	if state == nil || state.queryClosed {
		m.lifecycleMu.Unlock()
		return nil, nil
	}
	if state.facts.QueryID != queryID {
		m.lifecycleMu.Unlock()
		return nil, fmt.Errorf("Claude Desktop Renderer query generation changed")
	}
	now := m.now()
	metadata := map[string]any{
		"session_id": recordID, "cli_session_id": state.facts.SessionID,
		"had_pending_cycle":          state.pendingRequests > 0,
		"pending_had_first_response": nil, "pending_seconds": nil,
		"is_ssh": false, "backend_kind": "local", "trigger": "user",
	}
	if state.pendingRequests > 0 {
		metadata["pending_had_first_response"] = state.pendingHadFirstResponse
		metadata["pending_seconds"] = maxInt64(0, int64(now.Sub(state.pendingStartedAt).Round(time.Second)/time.Second))
	}
	worker, facts := state.worker, state.facts
	m.lifecycleMu.Unlock()
	for key, value := range m.rendererCLIProcessMetadata() {
		metadata[key] = value
	}
	// Armed-work and Renderer-buffer fields need their real owners. Do not fill
	// missing observations with fixed zeroes or API-message counts.
	var once sync.Once
	var errEmit error
	return func() error {
		once.Do(func() {
			m.lifecycleMu.Lock()
			current := m.sessions[rendererSessionKey(worker, rendererRecordKey(facts))]
			if current != state || current.facts.QueryID != queryID || current.queryClosed {
				m.lifecycleMu.Unlock()
				return
			}
			state.queryClosed = true
			state.pendingRequests = 0
			state.pendingStartedAt = time.Time{}
			state.pendingHadFirstResponse = false
			m.lifecycleMu.Unlock()
			errEmit = worker.enqueueProjected(context.Background(), FactSessionStopped, facts.SessionID, facts.ClientRequestID, metadata)
			if errEmit != nil {
				worker.recordQueueFailure(errEmit)
			}
			if userStop {
				errEmit = errors.Join(errEmit, m.emitRetainedRendererSessionStopped(worker, facts))
			}
		})
		return errEmit
	}, nil
}
