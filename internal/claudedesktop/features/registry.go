package features

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

var ErrAmbiguousSession = errors.New("Claude Desktop SDK transcript has multiple live query owners")

// Registry owns query handles in one live account runtime. Scope names locate
// owned roots; the Host pointer, not that name, owns feature state. Reidentify
// preserves a host, whereas Replace explicitly creates a new query generation.
type Registry struct {
	mu           sync.Mutex
	store        *SharedStore
	warm         *Host
	claimed      bool
	closed       bool
	roots        map[string]*Host
	aliases      Store
	errors       map[string]error
	queryAliases map[string]string
	sessions     map[string]string
}

func NewRegistry(store Store, aliases ...Store) *Registry {
	shared := NewSharedStore(store)
	r := &Registry{store: shared, warm: NewHost(shared.View(), ""), roots: make(map[string]*Host), errors: make(map[string]error), queryAliases: make(map[string]string), sessions: make(map[string]string)}
	if len(aliases) > 0 {
		r.aliases = aliases[0]
	}
	return r
}

func (r *Registry) Warm() *Host { return r.warm }

func (r *Registry) Main(scope, session string) (*Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mainLocked(scope, session)
}

func (r *Registry) mainLocked(scope, session string) (*Host, error) {
	if r.closed || scope == "" {
		return nil, fmt.Errorf("Claude Desktop SDK root is unavailable")
	}
	if host := r.roots[scope]; host != nil {
		if host.Context().Err() == nil {
			return host, nil
		}
		if session == "" {
			session = host.SessionID()
		}
	}
	var host *Host
	if !r.claimed && r.warm.Context().Err() == nil {
		host, r.claimed = r.warm, true
		// A fresh main adopts the prewarm identity, including the identity of
		// its already-dispatched evaluation. Only a resume reidentifies it.
		if session != "" {
			if err := host.Reidentify(session); err != nil {
				return nil, err
			}
		}
	} else {
		host = NewHost(r.store.View(), session)
	}
	r.roots[scope] = host
	return host, nil
}

type sessionAlias struct {
	Version int    `json:"version"`
	Session string `json:"session_id"`
}

// ResolveMain binds a caller root to a native SDK session before any main
// request is rendered. Only the alias is durable, never a Host or exposure set.
// Storage failures remain visible while inference can use its live owner.
func (r *Registry) ResolveMain(scope string, priorSession ...string) (*Host, error) {
	return r.ResolveQuery(scope, scope, priorSession...)
}

// ResolveQuery separates a live query's ownership key from the caller's
// durable transcript alias. Two connections may resume the same transcript
// without sharing feature state, exposure deduplication or cancellation.
func (r *Registry) ResolveQuery(scope, alias string, priorSession ...string) (*Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || scope == "" || alias == "" {
		return nil, fmt.Errorf("Claude Desktop SDK root is unavailable")
	}
	if previous := r.queryAliases[scope]; previous != "" && previous != alias {
		return nil, fmt.Errorf("Claude Desktop SDK query alias changed without an explicit transition")
	}
	r.queryAliases[scope] = alias
	if host := r.roots[scope]; host != nil {
		if host.Context().Err() == nil {
			return host, r.errors[alias]
		}
		// A later main starts a new query on the same transcript. It cannot
		// revive closed feature/exposure state or rewrite a damaged alias.
		next, err := r.mainLocked(scope, host.SessionID())
		return next, errors.Join(r.errors[alias], err)
	}
	session := r.sessions[alias]
	if session != "" {
		host, err := r.mainLocked(scope, session)
		return host, errors.Join(r.errors[alias], err)
	}
	session, revision, stateErr := r.loadAliasLocked(alias)
	if session == "" && stateErr == nil && len(priorSession) > 0 {
		id, err := uuid.Parse(priorSession[0])
		if err == nil && id != uuid.Nil {
			session = id.String()
		}
	}
	if revision == "" && stateErr == nil && r.aliases != nil {
		if session == "" {
			session = uuid.NewString()
			if !r.claimed && r.warm.Context().Err() == nil {
				session = r.warm.SessionID()
			}
		}
		payload, _ := json.Marshal(sessionAlias{Version: 1, Session: session})
		if _, stateErr = r.aliases.Save(alias, revision, payload); stateErr != nil {
			// A competing runtime may have won the first alias write. Adopt
			// that durable identity before publishing this runtime's handle.
			if saved, _, err := r.loadAliasLocked(alias); err == nil && saved != "" {
				session, stateErr = saved, nil
			}
		}
	}
	host, err := r.mainLocked(scope, session)
	r.errors[alias] = stateErr
	if host != nil {
		r.sessions[alias] = host.SessionID()
	}
	return host, errors.Join(stateErr, err)
}

// Session resolves an already-owned alias without claiming the warm query or
// starting a helper-specific evaluator. Unknown auxiliary calls stay unowned.
func (r *Registry) Session(scope string) (string, error) {
	return r.QuerySession(scope, scope)
}

// QuerySession never resolves a helper to a different query on the alias.
func (r *Registry) QuerySession(scope, alias string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || scope == "" || alias == "" {
		return "", fmt.Errorf("Claude Desktop SDK root is unavailable")
	}
	if previous := r.queryAliases[scope]; previous != "" && previous != alias {
		return "", fmt.Errorf("Claude Desktop SDK query alias does not match")
	}
	if host := r.roots[scope]; host != nil {
		return host.SessionID(), r.errors[alias]
	}
	if session := r.sessions[alias]; session != "" {
		return session, r.errors[alias]
	}
	session, _, err := r.loadAliasLocked(alias)
	return session, err
}

func (r *Registry) loadAliasLocked(scope string) (string, string, error) {
	if r.aliases == nil {
		return "", "", nil
	}
	payload, revision, err := r.aliases.Load(scope)
	if err != nil {
		return "", revision, err
	}
	if len(payload) == 0 {
		if revision != "" {
			return "", revision, fmt.Errorf("Claude Desktop SDK session alias is missing its payload")
		}
		return "", "", nil
	}
	var alias sessionAlias
	if json.Unmarshal(payload, &alias) != nil || alias.Version != 1 {
		return "", revision, fmt.Errorf("Claude Desktop SDK session alias is invalid")
	}
	id, err := uuid.Parse(alias.Session)
	if err != nil || id == uuid.Nil || id.String() != alias.Session {
		return "", revision, fmt.Errorf("Claude Desktop SDK session alias is invalid")
	}
	return alias.Session, revision, nil
}

// LookupSession returns only an unambiguous live owner. Independent explicit
// forks can share a transcript label and must instead use their scoped handle.
func (r *Registry) LookupSession(session string) *Host {
	host, _ := r.FindSession(session)
	return host
}

// FindSession distinguishes an unknown transcript from multiple live queries.
// The latter requires a scoped query handle, never another fallback query.
func (r *Registry) FindSession(session string) (*Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil
	}
	var match *Host
	for _, host := range r.roots {
		if host.SessionID() == session && host.Context().Err() == nil {
			if match != nil && match != host {
				return nil, ErrAmbiguousSession
			}
			match = host
		}
	}
	return match, nil
}

func (r *Registry) Lookup(scope string) *Host {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	return r.roots[scope]
}

// Retire removes only the dispatched generation, even after reidentity. Its
// durable aliases remain resumable; a replacement or sibling is never retired.
func (r *Registry) Retire(host *Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if host == nil {
		return
	}
	for scope, candidate := range r.roots {
		if candidate == host {
			delete(r.roots, scope)
			delete(r.queryAliases, scope)
		}
	}
	host.Close()
}

func (r *Registry) Reidentify(host *Host, oldScope, newScope, session string) error {
	return r.ReidentifyQuery(host, oldScope, newScope, newScope, session)
}

func (r *Registry) ReidentifyQuery(host *Host, oldScope, newScope, alias, session string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || host == nil || r.roots[oldScope] != host || newScope == "" || alias == "" || (r.roots[newScope] != nil && r.roots[newScope] != host) {
		return fmt.Errorf("Claude Desktop SDK root identity is stale")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if session == "" || host.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop SDK host is unavailable")
	}
	if err := r.saveAliasLocked(alias, session); err != nil {
		return err
	}
	host.session = session
	delete(r.roots, oldScope)
	delete(r.queryAliases, oldScope)
	delete(r.errors, alias)
	r.queryAliases[newScope] = alias
	r.sessions[alias] = session
	r.roots[newScope] = host
	return nil
}

func (r *Registry) Replace(host *Host, scope, session string) (*Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || host == nil || r.roots[scope] != host || session == "" {
		return nil, fmt.Errorf("Claude Desktop SDK query replacement is stale")
	}
	alias := r.queryAliases[scope]
	if alias == "" {
		alias = scope
	}
	if err := r.saveAliasLocked(alias, session); err != nil {
		return nil, err
	}
	host.Close()
	next := NewHost(r.store.View(), session)
	r.roots[scope] = next
	delete(r.errors, alias)
	r.sessions[alias] = session
	return next, nil
}

// Explicit identity transitions must checkpoint before retiring their old
// owner. A failed checkpoint leaves that live owner and its evidence intact.
func (r *Registry) saveAliasLocked(scope, session string) error {
	if r.aliases == nil {
		return nil
	}
	id, err := uuid.Parse(session)
	if err != nil || id == uuid.Nil || id.String() != session {
		return fmt.Errorf("Claude Desktop SDK session alias is invalid")
	}
	current, revision, err := r.loadAliasLocked(scope)
	if err != nil || current == session {
		return err
	}
	payload, _ := json.Marshal(sessionAlias{Version: 1, Session: session})
	_, err = r.aliases.Save(scope, revision, payload)
	return err
}

func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.warm.Close()
	for _, host := range r.roots {
		host.Close()
	}
}
