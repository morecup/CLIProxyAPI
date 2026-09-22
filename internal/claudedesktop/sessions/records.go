// Package sessions owns durable Desktop records independently of SDK transcripts
// and process-local query generations.
package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

var (
	ErrUnavailable      = errors.New("Claude Desktop session records are unavailable")
	ErrInvalid          = errors.New("Claude Desktop session record is invalid")
	ErrNotFound         = errors.New("Claude Desktop session record was not found")
	ErrStaleQuery       = errors.New("Claude Desktop session query generation changed")
	ErrResumeUnverified = errors.New("Claude Desktop session resume identity needs verification")
)

// Snapshot contains no credentials, caller aliases, transcript content or paths.
// QueryID is process-local; ID and SDKSessionID survive runtime reconstruction.
type Snapshot struct {
	ID                string    `json:"id"`
	Generation        string    `json:"generation"`
	SDKSessionID      string    `json:"sdk_session_id,omitempty"`
	QueryID           string    `json:"query_id,omitempty"`
	Running           bool      `json:"running"`
	CreatedAt         time.Time `json:"created_at"`
	RemoteState       string    `json:"remote_state,omitempty"`
	LocalConversation bool      `json:"local_conversation,omitempty"`
}

type record struct {
	Local            *LocalConversation `json:"local_conversation,omitempty"`
	localTurn        *LocalTurn
	ID               string        `json:"id"`
	Generation       string        `json:"generation,omitempty"`
	Owner            string        `json:"owner"`
	Scope            string        `json:"scope"`
	SDKSessionID     string        `json:"sdk_session_id,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	ResumeUnverified bool          `json:"resume_unverified,omitempty"`
	Remote           *remoteOrigin `json:"remote_control_spawn,omitempty"`
	BridgeSessionID  string        `json:"bridge_session_id,omitempty"`
	Bridge           *BridgeState  `json:"bridge_checkpoint,omitempty"`
	host             *features.Host
	inputDone        <-chan struct{}
	bridgeDone       <-chan struct{}
	pendingBridgeID  string
}

type catalog struct {
	Version int      `json:"version"`
	Records []record `json:"records"`
}

// Registry serializes query admission and explicit record operations. A caller
// transcript is not the record key: separate connections can resume the same
// transcript without sharing Desktop identity or cancellation ownership.
type Registry struct {
	mu             sync.Mutex
	store          features.Store
	loaded         bool
	revision       string
	loadErr        error
	persistErr     error
	records        map[string]*record
	remoteCreates  map[string]bool
	resumes        map[*record]chan struct{}
	uiObservations map[string]time.Time
}

func NewRegistry(store features.Store) *Registry {
	return &Registry{store: store, records: make(map[string]*record)}
}

func catalogKey() string {
	digest := sha256.Sum256([]byte("desktop-session-record-catalog-v1"))
	return hex.EncodeToString(digest[:])
}

func validKey(value string) bool {
	bytes, err := hex.DecodeString(value)
	return err == nil && len(bytes) == sha256.Size && strings.ToLower(value) == value
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (r *Registry) loadLocked() error {
	if r.loaded {
		return r.loadErr
	}
	r.loaded = true
	if r.store == nil {
		return nil
	}
	payload, revision, err := r.store.Load(catalogKey())
	if err != nil {
		r.loadErr = err
		return err
	}
	r.revision = revision
	if len(payload) == 0 && revision == "" {
		return nil
	}
	var saved catalog
	if json.Unmarshal(payload, &saved) != nil || saved.Version != 1 {
		r.loadErr = ErrInvalid
		return r.loadErr
	}
	records, ids := make(map[string]*record), make(map[string]bool)
	bindings := make(map[string]string)
	for _, value := range saved.Records {
		if !strings.HasPrefix(value.ID, "local_") || !validUUID(strings.TrimPrefix(value.ID, "local_")) ||
			!validKey(value.Scope) || !validKey(value.Owner) || value.CreatedAt.IsZero() ||
			(value.SDKSessionID != "" && !validUUID(value.SDKSessionID)) || (value.Generation != "" && !validUUID(value.Generation)) || records[value.Scope] != nil || ids[value.ID] {
			r.loadErr = ErrInvalid
			return r.loadErr
		}
		if !validRemoteRecord(value) {
			r.loadErr = ErrInvalid
			return r.loadErr
		}
		remoteIDs := []string{value.BridgeSessionID}
		if value.Remote != nil && value.Remote.DetachedAt == 0 {
			remoteIDs = append(remoteIDs, value.Remote.CCRSessionID)
		}
		for _, remoteID := range remoteIDs {
			if remoteID == "" {
				continue
			}
			key := value.Owner + ":" + remoteID
			if previous := bindings[key]; previous != "" && previous != value.ID {
				r.loadErr = ErrInvalid
				return r.loadErr
			}
			bindings[key] = value.ID
		}
		owned := value
		records[value.Scope], ids[value.ID] = &owned, true
	}
	r.records = records
	return nil
}

func (r *Registry) saveLocked() error {
	if r.loadErr != nil {
		return r.loadErr // Never replace unreadable originals with an empty catalog.
	}
	if r.store == nil {
		return nil
	}
	saved := catalog{Version: 1, Records: make([]record, 0, len(r.records))}
	for _, value := range r.records {
		saved.Records = append(saved.Records, *value)
	}
	sort.Slice(saved.Records, func(i, j int) bool { return saved.Records[i].Scope < saved.Records[j].Scope })
	payload, err := json.Marshal(saved)
	if err == nil {
		var revision string
		revision, err = r.store.Save(catalogKey(), r.revision, payload)
		if err == nil {
			r.revision = revision
		}
	}
	r.persistErr = err
	return err
}

// Resolve supplies nil only for a new record, allowing migration of its old
// caller-to-SDK alias. Existing records supply their own resume handle; an empty
// handle is intentional and must never fall back to a shared transcript alias.
func (r *Registry) Resolve(ctx context.Context, owner, scope string, start func(resume *string) (*features.Host, error)) (*features.Host, Snapshot, error) {
	if r == nil || ctx == nil || !validKey(owner) || !validKey(scope) || start == nil {
		return nil, Snapshot{}, ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.loadLocked()
	value := r.records[scope]
	for {
		if err := ctx.Err(); err != nil {
			return nil, Snapshot{}, err
		}
		if pending := r.resumes[value]; pending != nil {
			r.mu.Unlock()
			select {
			case <-pending:
			case <-ctx.Done():
			}
			r.mu.Lock()
			value = r.records[scope]
			continue
		}
		if value == nil || value.Owner != owner || value.host == nil || value.host.Context().Err() == nil {
			break
		}
		done := value.inputDone
		if done == nil {
			done = value.bridgeDone
		}
		if done == nil {
			break
		}
		select {
		case <-done:
			if value.inputDone == done {
				value.inputDone = nil
			}
			if value.bridgeDone == done {
				value.bridgeDone = nil
			}
		default:
			// Cancellation is not completion: the old actor can still own a
			// final accounting/outcome callback. Never join it under the
			// record lock, which those callbacks may need themselves.
			r.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
			}
			r.mu.Lock()
		}
		// Admission may have changed while this caller was waiting.
		value = r.records[scope]
	}
	if value != nil && value.Owner != owner {
		return nil, Snapshot{}, ErrInvalid
	}
	if value != nil && value.host != nil && value.host.Context().Err() == nil {
		if value.SDKSessionID != value.host.SessionID() || r.persistErr != nil {
			value.SDKSessionID = value.host.SessionID()
			_ = r.saveLocked()
		}
		return value.host, snapshot(value), errors.Join(r.loadErr, r.persistErr, resumeError(value))
	}
	var resume *string
	if value != nil {
		id := value.SDKSessionID
		resume = &id
	}
	host, errStart := start(resume)
	if host == nil {
		return nil, Snapshot{}, errors.Join(r.loadErr, r.persistErr, errStart)
	}
	if value == nil {
		value = &record{ID: "local_" + uuid.NewString(), Owner: owner, Scope: scope, CreatedAt: time.Now().UTC()}
		r.records[scope] = value
	}
	value.host, value.SDKSessionID, value.inputDone = host, host.SessionID(), nil
	value.Generation = uuid.NewString()
	value.bridgeDone = nil
	value.pendingBridgeID = ""
	if errStart != nil {
		value.ResumeUnverified = true
	}
	_ = r.saveLocked()
	return host, snapshot(value), errors.Join(r.loadErr, r.persistErr, errStart, resumeError(value))
}

func resumeError(value *record) error {
	if value.ResumeUnverified {
		return ErrResumeUnverified
	}
	return nil
}

func snapshot(value *record) Snapshot {
	result := Snapshot{ID: value.ID, Generation: value.Generation, SDKSessionID: value.SDKSessionID, CreatedAt: value.CreatedAt, LocalConversation: value.Local != nil}
	if value.Remote != nil {
		result.RemoteState = "pending"
		if value.Remote.DetachedAt != 0 {
			result.RemoteState = "detached"
		} else if value.BridgeSessionID == value.Remote.CCRSessionID {
			result.RemoteState = "attached"
		}
	}
	if value.host != nil {
		result.QueryID = value.host.ID()
		result.SDKSessionID = value.host.SessionID()
		result.Running = value.host.Context().Err() == nil
	}
	return result
}

func (r *Registry) ByHost(host *features.Host) Snapshot {
	if r == nil || host == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range r.records {
		if value.host == host {
			return snapshot(value)
		}
	}
	return Snapshot{}
}

// ListObservation describes this registry read, not a renderer paint or a
// guessed cache hit. Failed reads retain their error and are not success events.
type ListObservation struct {
	Duration time.Duration
	Cached   bool
}

// HeartbeatBatchObservation classifies every owner-scoped record at one real
// registry read. Fresh/recent classifications stay zero until a measured
// renderer cache exists; they are not inferred from record age.
type HeartbeatBatchObservation struct {
	Sent, Fresh, ProbeDispatched, NoWorker, RecentlyChecked, Unknown int
}

func (r *Registry) List(owner string) ([]Snapshot, error) {
	values, _, err := r.ListObserved(owner)
	return values, err
}

func (r *Registry) ListObserved(owner string) ([]Snapshot, ListObservation, error) {
	started := time.Now()
	if r == nil || !validKey(owner) {
		return nil, ListObservation{}, ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cached := r.loaded
	_ = r.loadLocked()
	// Legacy catalogs have no durable admission token. Issue one only through
	// the protected store; a failed migration cannot authorize a resume.
	changed := false
	for _, value := range r.records {
		if value.Generation == "" {
			value.Generation, changed = uuid.NewString(), true
		}
	}
	if changed || r.persistErr != nil {
		_ = r.saveLocked()
	}
	result := make([]Snapshot, 0)
	var errResume error
	for _, value := range r.records {
		if value.Owner == owner {
			result = append(result, snapshot(value))
			errResume = errors.Join(errResume, resumeError(value))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, ListObservation{Duration: time.Since(started), Cached: cached}, errors.Join(r.loadErr, r.persistErr, errResume)
}

// HeartbeatCheckBatch validates the live Host bound to each durable record.
// The caller supplies only the owner; all counters are calculated here.
func (r *Registry) HeartbeatCheckBatch(owner string) (HeartbeatBatchObservation, error) {
	if r == nil || !validKey(owner) {
		return HeartbeatBatchObservation{}, ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.loadLocked()
	var result HeartbeatBatchObservation
	var errResume error
	for _, value := range r.records {
		if value.Owner != owner {
			continue
		}
		result.Sent++
		errResume = errors.Join(errResume, resumeError(value))
		host := value.host
		if host == nil || host.Context() == nil || host.Context().Err() != nil {
			result.NoWorker++
			continue
		}
		if strings.TrimSpace(host.ID()) == "" || strings.TrimSpace(host.SessionID()) == "" || strings.TrimSpace(value.Generation) == "" {
			result.Unknown++
			continue
		}
		// Reading the live Host identity under its own locks is the probe. The
		// control-plane runtime owns the separate network heartbeat loop.
		result.ProbeDispatched++
	}
	return result, errors.Join(r.loadErr, r.persistErr, errResume)
}

// Stop takes the pre-close observation while admission is excluded. The final
// emitter runs after exact query retirement, still before a replacement can
// start. A repeated stop is inert, and stale generations cannot stop successors.
func (r *Registry) Stop(ctx context.Context, owner, id, expectedQuery string,
	prepare func(Snapshot) (func() error, error), retire func(*features.Host)) (Snapshot, error) {
	if r == nil || ctx == nil || !validKey(owner) || id == "" || expectedQuery == "" || retire == nil {
		return Snapshot{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	errLoad := r.loadLocked()
	var value *record
	for _, candidate := range r.records {
		if candidate.ID == id && candidate.Owner == owner {
			value = candidate
			break
		}
	}
	if value == nil {
		return Snapshot{}, errors.Join(errLoad, ErrNotFound)
	}
	before := snapshot(value)
	if before.QueryID != expectedQuery {
		return before, ErrStaleQuery
	}
	if !before.Running {
		return before, nil
	}
	var emit func() error
	if prepare != nil {
		var err error
		emit, err = prepare(before)
		if err != nil {
			return before, err
		}
	}
	var errSave error
	if value.SDKSessionID != before.SDKSessionID {
		value.SDKSessionID = before.SDKSessionID
		errSave = r.saveLocked()
	}
	retire(value.host)
	var errEmit error
	if emit != nil {
		errEmit = emit()
	}
	return snapshot(value), errors.Join(errLoad, errSave, errEmit)
}
