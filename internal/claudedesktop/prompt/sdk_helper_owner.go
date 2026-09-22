package prompt

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// SDKHelperOwner binds an API callback to an already observed main prompt.
// It grants ledger/disposition access only, not control over the parent's
// active request or terminal state. It is independent of telemetry workers.
// Close releases this live callback's cache-retention lease. Its original
// snapshot remains readable, but released callbacks cannot mutate the ledger.
type SDKHelperOwner struct {
	tracker *Tracker
	state   *state
	closed  bool
}

// Snapshot reads the originally bound parent's accounting without freezing it
// or changing ownership. It never selects the newest prompt in the session.
func (o *SDKHelperOwner) Snapshot() SDKSnapshot {
	if o == nil {
		return SDKSnapshot{}
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	return o.state.sdkSnapshot()
}

func (t *Tracker) BindSDKHelper(input Input) *SDKHelperOwner {
	if t == nil || input.AccountID == "" || input.SessionID == "" {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(input.ParentPromptID))
	if err != nil || id == uuid.Nil {
		return nil
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = time.Now()
	}
	scope := digest(input.AccountID, input.SessionID)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.prompts == nil {
		t.prompts = make(map[string]*state)
	}
	t.prune(input.StartedAt)
	t.loadSDKSessionLocked(scope)
	owner := t.prompts[digest(scope, id.String())]
	// Pruning already removes expired in-memory owners. A successfully restored
	// durable owner remains valid for an explicitly bound late helper; applying
	// the memory TTL again would discard its persisted session ledger.
	if owner == nil || owner.sdk.queries == 0 {
		return nil
	}
	owner.helperOwners++
	return &SDKHelperOwner{tracker: t, state: owner}
}

func (o *SDKHelperOwner) Close() {
	if o == nil {
		return
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	if !o.closed {
		o.closed = true
		o.state.helperOwners--
		if now := time.Now(); now.After(o.state.lastAt) {
			o.state.lastAt = now
		}
	}
}

// A bound late callback follows its original ledger even after another prompt
// starts. A sealed result remains immutable; no lookup of the latest prompt is
// performed at completion time.
func (o *SDKHelperOwner) RecordSuccess(source, callID string, durationMS int64) {
	if o == nil || callID == "" {
		return
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	if o.closed {
		return
	}
	defer o.tracker.saveSDKSessionLocked(o.state.scope)
	o.state.sdk.recordHelperSuccess(source, callID, durationMS)
}

func (o *SDKHelperOwner) RecordCompactionSuccess(source, callID string, durationMS int64, observation SDKCompactionObservation) {
	if o == nil || callID == "" {
		return
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	if o.closed {
		return
	}
	defer o.tracker.saveSDKSessionLocked(o.state.scope)
	o.state.sdk.recordCompactionSuccess(source, callID, durationMS, []SDKCompactionObservation{observation})
}

func (o *SDKHelperOwner) ObserveUnmodeledHelper() {
	if o == nil {
		return
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	if o.closed {
		return
	}
	defer o.tracker.saveSDKSessionLocked(o.state.scope)
	o.state.sdk.ledger.incompleteReason = "unobserved-sdk-helper-boundary"
	if o.state.sdk.result == nil {
		o.state.sdk.incompleteReason = "unobserved-sdk-helper-boundary"
	}
}
