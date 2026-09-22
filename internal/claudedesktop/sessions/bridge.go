package sessions

import (
	"errors"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

var ErrBridgeOwner = errors.New("Claude Desktop saved bridge belongs to a different login")

// BridgeState is private protected record metadata. It is not a management
// response, a worker credential, or permission to replay transcript content.
type BridgeState struct {
	SessionID             string   `json:"bridgeSessionId"`
	LastSequenceNum       int64    `json:"lastSequenceNum"`
	DeclaredDialogKinds   []string `json:"declaredDialogKinds,omitempty"`
	SessionGroupingID     string   `json:"sessionGroupingId,omitempty"`
	NoHistoryBackfill     bool     `json:"noHistoryBackfill,omitempty"`
	OwnerAccountUUID      string   `json:"ownerAccountUuid"`
	OwnerOrganizationUUID string   `json:"ownerOrganizationUuid"`
}

func validBridgeState(value *BridgeState) bool {
	return value != nil && value.SessionID != "" && NormalizeRemoteSessionID(value.SessionID) == value.SessionID &&
		value.LastSequenceNum >= 0 && value.OwnerAccountUUID != "" && value.OwnerOrganizationUUID != ""
}

func cloneBridgeState(value *BridgeState) *BridgeState {
	if value == nil {
		return nil
	}
	copy := *value
	copy.DeclaredDialogKinds = slices.Clone(value.DeclaredDialogKinds)
	return &copy
}

// BridgeGrant binds the producer and loader to an exact record/Host. Its final
// checkpoint may run after cancellation, but never after a successor owns the
// record. Resolve joins the registered worker cleanup before granting that successor.
type BridgeGrant struct {
	registry *Registry
	record   *record
	host     *features.Host
}

func (r *Registry) BridgeForHost(host *features.Host) *BridgeGrant {
	if r == nil || host == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range r.records {
		if value.host == host {
			return &BridgeGrant{registry: r, record: value, host: host}
		}
	}
	return nil
}

func (g *BridgeGrant) validLocked(live bool) error {
	if g.record == nil || g.host == nil || g.registry.records[g.record.Scope] != g.record || g.record.host != g.host ||
		(live && g.host.Context().Err() != nil) {
		return ErrStaleQuery
	}
	if g.registry.loadErr != nil {
		return g.registry.loadErr
	}
	if g.record.ResumeUnverified {
		return ErrResumeUnverified
	}
	return nil
}

func (g *BridgeGrant) Load(account, organization string) (*BridgeState, error) {
	if g == nil || g.registry == nil || account == "" || organization == "" {
		return nil, ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(true); err != nil {
		return nil, err
	}
	value := g.record.Bridge
	if value != nil && (value.OwnerAccountUUID != account || value.OwnerOrganizationUUID != organization) {
		return nil, ErrBridgeOwner
	}
	return cloneBridgeState(value), nil
}

func (g *BridgeGrant) MatchQuery(recordID, queryID, sessionID string) error {
	if g == nil || g.registry == nil {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(true); err != nil {
		return err
	}
	if g.record.ID != recordID || g.host.ID() != queryID || g.host.SessionID() != sessionID {
		return ErrStaleQuery
	}
	return nil
}

func (g *BridgeGrant) BindDone(done <-chan struct{}) error {
	if g == nil || g.registry == nil || done == nil {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(true); err != nil {
		return err
	}
	if g.record.bridgeDone != nil && g.record.bridgeDone != done {
		select {
		case <-g.record.bridgeDone:
		default:
			return ErrRemoteBound
		}
	}
	g.record.bridgeDone = done
	return nil
}

// Prepare reserves a binding during worker initialization without replacing a
// durable checkpoint prematurely. The final Save commits identity and cursor
// together. Failed initialization cannot publish a half-written replacement.
func (g *BridgeGrant) Prepare(remoteID string) error {
	if g == nil || g.registry == nil || remoteID == "" || NormalizeRemoteSessionID(remoteID) != remoteID {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(true); err != nil {
		return err
	}
	if origin := g.record.Remote; origin != nil && (origin.DetachedAt != 0 || origin.CCRSessionID != remoteID) {
		if origin.DetachedAt == 0 {
			origin.DetachedAt = time.Now().UnixMilli()
		}
		return errors.Join(ErrRemoteMismatch, g.registry.saveLocked())
	}
	if g.registry.remoteBoundLocked(g.record.Owner, remoteID, g.record) {
		return ErrRemoteBound
	}
	g.record.pendingBridgeID = remoteID
	return nil
}

func (g *BridgeGrant) Release() {
	if g == nil || g.registry == nil {
		return
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if g.validLocked(false) == nil {
		g.record.pendingBridgeID = ""
	}
}

// Save is called at lifecycle checkpoints, never from the per-frame cursor
// advance. The independent protected catalog survives replacement of query IDs.
func (g *BridgeGrant) Save(value BridgeState) error {
	if g == nil || g.registry == nil || !validBridgeState(&value) {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(false); err != nil {
		return err
	}
	before := g.record.Bridge
	if before != nil && (before.OwnerAccountUUID != value.OwnerAccountUUID || before.OwnerOrganizationUUID != value.OwnerOrganizationUUID) {
		return ErrBridgeOwner
	}
	if origin := g.record.Remote; origin != nil && (origin.DetachedAt != 0 || origin.CCRSessionID != value.SessionID) {
		return ErrRemoteMismatch
	}
	if g.registry.remoteBoundLocked(g.record.Owner, value.SessionID, g.record) {
		return ErrRemoteBound
	}
	if before != nil {
		value.NoHistoryBackfill = value.NoHistoryBackfill || before.NoHistoryBackfill
		if before.SessionID == value.SessionID {
			value.LastSequenceNum = max(value.LastSequenceNum, before.LastSequenceNum)
		}
	}
	previousID := g.record.BridgeSessionID
	g.record.Bridge, g.record.BridgeSessionID = cloneBridgeState(&value), value.SessionID
	if err := g.registry.saveLocked(); err != nil {
		g.record.Bridge, g.record.BridgeSessionID = before, previousID
		return err
	}
	g.record.pendingBridgeID = ""
	return nil
}
