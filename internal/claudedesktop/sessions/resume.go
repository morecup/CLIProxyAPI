package sessions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

// ResumeRecord is private preparation input, never a management response.
type ResumeRecord struct {
	SDKSessionID, Folder, Model, DefaultModel, System string
}

func (ResumeRecord) MarshalJSON() ([]byte, error) { return nil, ErrInvalid }

// ResumeSession reopens either an ordinary Desktop query host or a remotely
// created query. Ordinary records preserve their SDK session without creating
// remote input; remote records additionally prepare their saved wire history.
func (r *Registry) ResumeSession(ctx context.Context, owner, id, expected string,
	prepare func(ResumeRecord) error, start func(scope, session string) (*features.Host, error)) (*RemoteGrant, Snapshot, error) {
	return r.resume(ctx, owner, id, expected, true, prepare, start)
}

// ResumeRemote preserves the remote-only contract for callers that already
// know the record has a remote origin.
func (r *Registry) ResumeRemote(ctx context.Context, owner, id, expected string,
	prepare func(ResumeRecord) error, start func(scope, session string) (*features.Host, error)) (*RemoteGrant, Snapshot, error) {
	return r.resume(ctx, owner, id, expected, false, prepare, start)
}

// resume reserves the exact saved generation before joining old actors and
// preparing remote history. Preparation and cleanup may acquire other runtime
// locks, so neither runs under the catalog lock. Only a successful durable CAS
// publishes the new Host; old pages and stale catalogs cannot resurrect it.
func (r *Registry) resume(ctx context.Context, owner, id, expected string, allowLocal bool,
	prepare func(ResumeRecord) error, start func(scope, session string) (*features.Host, error)) (*RemoteGrant, Snapshot, error) {
	if r == nil || ctx == nil || !validKey(owner) || id == "" || !validUUID(expected) || prepare == nil || start == nil {
		return nil, Snapshot{}, ErrInvalid
	}
	r.mu.Lock()
	if err := errors.Join(ctx.Err(), r.loadLocked()); err != nil {
		r.mu.Unlock()
		return nil, Snapshot{}, err
	}
	var value *record
	for _, candidate := range r.records {
		if candidate.Owner == owner && candidate.ID == id {
			value = candidate
			break
		}
	}
	if value == nil {
		r.mu.Unlock()
		return nil, Snapshot{}, ErrNotFound
	}
	before := snapshot(value)
	var err error
	switch {
	case value.Generation != expected || before.Running || r.resumes[value] != nil:
		err = ErrStaleQuery
	case r.store == nil:
		err = ErrUnavailable
	case value.ResumeUnverified:
		err = ErrResumeUnverified
	case !validUUID(value.SDKSessionID):
		err = ErrInvalid
	case value.Remote == nil && !allowLocal:
		err = ErrInvalid
	case value.Remote != nil && value.Remote.DetachedAt != 0:
		err = ErrRemoteDetached
	}
	if err != nil {
		r.mu.Unlock()
		return nil, before, err
	}
	if r.resumes == nil {
		r.resumes = make(map[*record]chan struct{})
	}
	pending := make(chan struct{})
	r.resumes[value] = pending
	inputDone, bridgeDone := value.inputDone, value.bridgeDone
	scope, session := value.Scope, value.SDKSessionID
	var origin *remoteOrigin
	if value.Remote != nil {
		copied := *value.Remote
		origin = &copied
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.resumes, value)
		close(pending)
		r.mu.Unlock()
	}()
	for _, done := range []<-chan struct{}{inputDone, bridgeDone} {
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return nil, before, ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, before, err
	}
	if origin != nil {
		if origin.DefaultModel == "" {
			origin.DefaultModel = origin.Model
		}
		if err := prepare(ResumeRecord{SDKSessionID: session, Folder: origin.Folder, Model: origin.Model, DefaultModel: origin.DefaultModel, System: origin.System}); err != nil {
			return nil, before, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, before, err
	}
	host, err := start(scope, session)
	if err != nil || host == nil {
		if host != nil {
			host.Close()
		}
		if err == nil {
			err = ErrUnavailable
		}
		return nil, before, err
	}
	published := false
	defer func() {
		if !published {
			host.Close()
		}
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), host.Context().Err()); err != nil {
		return nil, before, err
	}
	if r.records[scope] != value || value.Generation != expected || snapshot(value).Running || host.SessionID() != session {
		return nil, snapshot(value), ErrStaleQuery
	}
	prior := *value
	if value.Remote != nil {
		remote := *value.Remote
		remote.ResumeHistory = true
		value.Remote = &remote
	}
	value.host, value.Generation = host, uuid.NewString()
	value.inputDone, value.bridgeDone, value.pendingBridgeID = nil, nil, ""
	if err := r.saveLocked(); err != nil {
		*value = prior
		return nil, before, errors.Join(ErrUnavailable, err)
	}
	published = true
	if origin == nil {
		return nil, snapshot(value), nil
	}
	return &RemoteGrant{registry: r, record: value, host: host}, snapshot(value), nil
}
