package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

var (
	ErrRemoteBound    = errors.New("Claude Desktop remote session is already bound or being created")
	ErrRemoteDetached = errors.New("Claude Desktop remote origin is detached")
	ErrRemoteMismatch = errors.New("Claude Desktop bridge returned a different remote session")
)

type remoteOrigin struct {
	CCRSessionID  string `json:"ccrSessionId"`
	Folder        string `json:"folder"`
	Model         string `json:"model"`
	DefaultModel  string `json:"defaultModel,omitempty"`
	System        string `json:"systemPrompt,omitempty"`
	DetachedAt    int64  `json:"detachedAt,omitempty"`
	ResumeHistory bool   `json:"resumeHistory,omitempty"`
}

// RemoteGrant is issued only by record creation or exact-generation lookup.
// Caller request flags cannot construct an adoption capability for a record.
type RemoteGrant struct {
	registry *Registry
	record   *record
	host     *features.Host
}

func (g *RemoteGrant) BridgeRecord() *BridgeGrant {
	if g == nil {
		return nil
	}
	return &BridgeGrant{registry: g.registry, record: g.record, host: g.host}
}

// NormalizeRemoteSessionID follows the pinned Desktop cse/session normalizer.
// It deliberately does not trim or accept URL/path characters.
func NormalizeRemoteSessionID(value string) string {
	suffix := ""
	switch {
	case strings.HasPrefix(value, "cse_"):
		suffix = strings.TrimPrefix(value, "cse_")
	case strings.HasPrefix(value, "session_"):
		suffix = strings.TrimPrefix(value, "session_")
	default:
		return ""
	}
	if len(suffix) == 0 || len(suffix) > 64 {
		return ""
	}
	for _, ch := range suffix {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return ""
		}
	}
	return "cse_" + suffix
}

func validRemoteRecord(value record) bool {
	if value.Bridge != nil && (!validBridgeState(value.Bridge) || value.Bridge.SessionID != value.BridgeSessionID) {
		return false
	}
	if value.BridgeSessionID != "" && NormalizeRemoteSessionID(value.BridgeSessionID) != value.BridgeSessionID {
		return false
	}
	if origin := value.Remote; origin != nil {
		return NormalizeRemoteSessionID(origin.CCRSessionID) == origin.CCRSessionID && origin.CCRSessionID != "" &&
			origin.Folder != "" && strings.TrimSpace(origin.Model) != "" && origin.DetachedAt >= 0 &&
			(origin.DefaultModel == "" || strings.TrimSpace(origin.DefaultModel) != "")
	}
	return true
}

func (r *Registry) remoteBoundLocked(owner, remoteID string, except *record) bool {
	for _, value := range r.records {
		if value == except || value.Owner != owner {
			continue
		}
		if value.BridgeSessionID == remoteID || value.pendingBridgeID == remoteID || value.Remote != nil && value.Remote.DetachedAt == 0 && value.Remote.CCRSessionID == remoteID {
			return true
		}
	}
	return false
}

// CreateRemote never accepts an existing local record ID or a user message.
// The reservation spans asynchronous SDK preparation, while the second check
// catches a bridge binding that appeared during preparation.
func (r *Registry) CreateRemote(ctx context.Context, owner, remoteID, folder, model string,
	start func(scope string) (*features.Host, error)) (*RemoteGrant, Snapshot, error) {
	remoteID = NormalizeRemoteSessionID(remoteID)
	if r == nil || ctx == nil || !validKey(owner) || remoteID == "" || folder == "" || strings.TrimSpace(model) == "" || start == nil {
		return nil, Snapshot{}, ErrInvalid
	}
	reservation := owner + ":" + remoteID
	r.mu.Lock()
	if err := errors.Join(ctx.Err(), r.loadLocked()); err != nil {
		r.mu.Unlock()
		return nil, Snapshot{}, err
	}
	if r.remoteCreates[reservation] || r.remoteBoundLocked(owner, remoteID, nil) {
		r.mu.Unlock()
		return nil, Snapshot{}, ErrRemoteBound
	}
	if r.remoteCreates == nil {
		r.remoteCreates = make(map[string]bool)
	}
	r.remoteCreates[reservation] = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.remoteCreates, reservation); r.mu.Unlock() }()
	localID := "local_" + uuid.NewString()
	digest := sha256.Sum256([]byte("desktop-remote-record-v1\x00" + owner + "\x00" + localID))
	scope := hex.EncodeToString(digest[:])
	host, errStart := start(scope)
	if errStart != nil || host == nil {
		if host != nil {
			host.Close()
		}
		if errStart == nil {
			errStart = ErrUnavailable
		}
		return nil, Snapshot{}, errStart
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), host.Context().Err(), r.loadErr); err != nil {
		host.Close()
		return nil, Snapshot{}, err
	}
	if r.remoteBoundLocked(owner, remoteID, nil) {
		host.Close()
		return nil, Snapshot{}, ErrRemoteBound
	}
	value := &record{ID: localID, Generation: uuid.NewString(), Owner: owner, Scope: scope, SDKSessionID: host.SessionID(), CreatedAt: time.Now().UTC(), host: host,
		Remote: &remoteOrigin{CCRSessionID: remoteID, Folder: folder, Model: strings.TrimSpace(model)}}
	r.records[scope] = value
	grant := &RemoteGrant{registry: r, record: value, host: host}
	return grant, snapshot(value), r.saveLocked()
}

func (r *Registry) Remote(owner, id, queryID string) (*RemoteGrant, error) {
	if r == nil || !validKey(owner) || id == "" || queryID == "" {
		return nil, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return nil, err
	}
	for _, value := range r.records {
		if value.Owner != owner || value.ID != id {
			continue
		}
		if value.Remote == nil {
			return nil, ErrInvalid
		}
		if value.host == nil || value.host.ID() != queryID || value.host.Context().Err() != nil {
			return nil, ErrStaleQuery
		}
		if value.Remote.DetachedAt != 0 {
			return nil, ErrRemoteDetached
		}
		return &RemoteGrant{registry: r, record: value, host: value.host}, nil
	}
	return nil, ErrNotFound
}

func (g *RemoteGrant) validLocked() error {
	if g.record == nil || g.host == nil || g.registry.records[g.record.Scope] != g.record || g.record.host != g.host || g.host.Context().Err() != nil {
		return ErrStaleQuery
	}
	if g.record.Remote == nil {
		return ErrInvalid
	}
	if g.record.Remote.DetachedAt != 0 {
		return ErrRemoteDetached
	}
	return nil
}

// Read checks the record and query again at each consuming boundary. The
// returned host is the owned SDK generation, not a caller-supplied identity.
func (g *RemoteGrant) Read() (Snapshot, *features.Host, string, string, string, error) {
	if g == nil || g.registry == nil {
		return Snapshot{}, nil, "", "", "", ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(); err != nil {
		return Snapshot{}, nil, "", "", "", err
	}
	o := g.record.Remote
	return snapshot(g.record), g.host, o.CCRSessionID, o.Folder, o.Model, nil
}

// BindInputDone registers the one actor that can outlive cancellation of this
// exact query. Its channel closes only after the actor's final outcome callback.
func (g *RemoteGrant) BindInputDone(done <-chan struct{}) error {
	if g == nil || g.registry == nil || done == nil {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(); err != nil {
		return err
	}
	if g.record.inputDone != nil && g.record.inputDone != done {
		return ErrRemoteBound
	}
	g.record.inputDone = done
	return nil
}

// Configuration is private owned state, never part of a management snapshot.
// Older records predate model switching persistence; their creation model is
// also their default. A later switch must retain that original default.
func (g *RemoteGrant) Configuration() (model, defaultModel, system string, err error) {
	if g == nil || g.registry == nil {
		return "", "", "", ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(); err != nil {
		return "", "", "", err
	}
	o := g.record.Remote
	defaultModel = o.DefaultModel
	if defaultModel == "" {
		defaultModel = o.Model
	}
	return o.Model, defaultModel, o.System, nil
}

// A retry after attachment failure must restore the same saved history, not
// construct an empty actor merely because its first preparation was lost.
func (g *RemoteGrant) RequiresSavedHistory() (bool, error) {
	if g == nil || g.registry == nil {
		return false, ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := g.validLocked(); err != nil {
		return false, err
	}
	return g.record.Remote.ResumeHistory, nil
}

// SetConfiguration commits before the control response acknowledges success.
// A rejected write leaves both the record and the actor's prior state intact.
// The registry store is the account's protected catalog, not an HTTP response.
func (g *RemoteGrant) SetConfiguration(ctx context.Context, model, system string) error {
	if g == nil || g.registry == nil || ctx == nil || strings.TrimSpace(model) == "" {
		return ErrInvalid
	}
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	if err := errors.Join(ctx.Err(), g.validLocked()); err != nil {
		return err
	}
	if g.registry.store == nil {
		return ErrUnavailable
	}
	o := g.record.Remote
	before := *o
	if o.DefaultModel == "" {
		o.DefaultModel = o.Model
	}
	o.Model, o.System = model, system
	if err := g.registry.saveLocked(); err != nil {
		*o = before
		return err
	}
	return nil
}

// BindBridge records ordinary and remotely-created query bindings alike, so a
// later remote creation cannot adopt another local record's bridge session.
func (r *Registry) BindBridge(host *features.Host, remoteID string) error {
	if r == nil || host == nil {
		return ErrInvalid
	}
	remoteID = NormalizeRemoteSessionID(remoteID)
	if remoteID == "" {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range r.records {
		if value.host != host {
			continue
		}
		if host.Context().Err() != nil {
			return ErrStaleQuery
		}
		if value.Remote != nil && (value.Remote.DetachedAt != 0 || value.Remote.CCRSessionID != remoteID) {
			if value.Remote.DetachedAt == 0 {
				value.Remote.DetachedAt = time.Now().UnixMilli()
			}
			return errors.Join(ErrRemoteMismatch, r.saveLocked())
		}
		if r.remoteBoundLocked(value.Owner, remoteID, value) {
			return ErrRemoteBound
		}
		if value.BridgeSessionID == remoteID {
			return r.persistErr
		}
		previousID, previousCheckpoint := value.BridgeSessionID, value.Bridge
		value.BridgeSessionID = remoteID
		value.Bridge = nil
		if err := r.saveLocked(); err != nil {
			value.BridgeSessionID, value.Bridge = previousID, previousCheckpoint
			return err
		}
		return nil
	}
	return ErrNotFound
}
