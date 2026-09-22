package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ClaudeDesktopExecutionSessions owns downstream connection lifetimes, not
// transcript identities. Closed IDs stay tombstoned for this router's lifetime:
// a delayed dispatch must not resurrect a connection the scheduler has closed.
// Only hashes and cancellation state are retained, never caller IDs or payloads.
type ClaudeDesktopExecutionSessions struct {
	mu       sync.Mutex
	sessions map[string]*claudeDesktopExecutionSession
}

type claudeDesktopExecutionSessionKey struct{}

type claudeDesktopExecutionOwner struct {
	retire func()
	stop   func() bool
}

type claudeDesktopExecutionSession struct {
	registry *ClaudeDesktopExecutionSessions
	key      string
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	owners   map[string]claudeDesktopExecutionOwner
}

// Bind reads internal dispatch metadata only. Headers and body session IDs
// describe conversation identity and cannot authorize connection ownership.
// An inherited helper stays on its exact connection, including after closure.
func (r *ClaudeDesktopExecutionSessions) Bind(ctx context.Context, metadata ...map[string]any) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return ctx, nil
	}
	id := ""
	for _, values := range metadata {
		switch value := values[cliproxyexecutor.ExecutionSessionMetadataKey].(type) {
		case string:
			id = strings.TrimSpace(value)
		case []byte:
			id = strings.TrimSpace(string(value))
		}
		if id != "" {
			break
		}
	}
	if inherited, _ := ctx.Value(claudeDesktopExecutionSessionKey{}).(*claudeDesktopExecutionSession); inherited != nil {
		if inherited.registry != r || (id != "" && inherited.key != claudeDesktopExecutionKey(id)) {
			return ctx, errors.New("Claude Desktop execution session ownership does not match dispatch")
		}
		return ctx, inherited.ctx.Err()
	}
	if id == "" {
		return ctx, nil
	}
	r.mu.Lock()
	session := r.sessionLocked(claudeDesktopExecutionKey(id))
	r.mu.Unlock()
	return context.WithValue(ctx, claudeDesktopExecutionSessionKey{}, session), session.ctx.Err()
}

func (r *ClaudeDesktopExecutionSessions) sessionLocked(key string) *claudeDesktopExecutionSession {
	if r.sessions == nil {
		r.sessions = make(map[string]*claudeDesktopExecutionSession)
	}
	if session := r.sessions[key]; session != nil {
		return session
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &claudeDesktopExecutionSession{registry: r, key: key, ctx: ctx, cancel: cancel}
	r.sessions[key] = session
	return session
}

// Close retires only owners actually registered to this execution ID, across
// account retries. An unseen close is still recorded before a queued dispatch.
func (r *ClaudeDesktopExecutionSessions) Close(id string) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	session := r.sessionLocked(claudeDesktopExecutionKey(strings.TrimSpace(id)))
	r.mu.Unlock()
	session.mu.Lock()
	session.cancel()
	owners := session.owners
	session.owners = nil
	session.mu.Unlock()
	for _, owner := range owners {
		owner.stop()
		owner.retire()
	}
}

func claudeDesktopExecutionKey(id string) string {
	digest := sha256.Sum256([]byte("desktop-execution-session-v1\x00" + id))
	return hex.EncodeToString(digest[:])
}

// Scope separates a live query key from its durable transcript alias. Requests
// without a long-lived connection keep the previous stateless HTTP behavior.
func (r *ClaudeDesktopExecutionSessions) Scope(ctx context.Context, alias string) string {
	if session := r.fromContext(ctx); session != nil {
		digest := sha256.Sum256([]byte("desktop-execution-query-v1\x00" + session.key + "\x00" + alias))
		return hex.EncodeToString(digest[:])
	}
	return alias
}

func (r *ClaudeDesktopExecutionSessions) Lifetime(ctx context.Context) context.Context {
	if session := r.fromContext(ctx); session != nil {
		return session.ctx
	}
	return nil
}

func (r *ClaudeDesktopExecutionSessions) fromContext(ctx context.Context) *claudeDesktopExecutionSession {
	if r == nil || ctx == nil {
		return nil
	}
	session, _ := ctx.Value(claudeDesktopExecutionSessionKey{}).(*claudeDesktopExecutionSession)
	if session != nil && session.registry == r {
		return session
	}
	return nil
}

// Own registers an exact live query before it can start background work. Close
// and registration serialize; a losing registration retires its owner at once.
// Query teardown removes its registration without closing the connection.
func (r *ClaudeDesktopExecutionSessions) Own(ctx context.Context, id string, lifetime context.Context, retire func()) bool {
	session := r.fromContext(ctx)
	if session == nil {
		return true
	}
	session.mu.Lock()
	if session.ctx.Err() != nil {
		session.mu.Unlock()
		retire()
		return false
	}
	if lifetime.Err() != nil {
		session.mu.Unlock()
		return false
	}
	if session.owners == nil {
		session.owners = make(map[string]claudeDesktopExecutionOwner)
	}
	if _, exists := session.owners[id]; !exists {
		stop := context.AfterFunc(lifetime, func() {
			session.mu.Lock()
			delete(session.owners, id)
			session.mu.Unlock()
		})
		session.owners[id] = claudeDesktopExecutionOwner{retire: retire, stop: stop}
	}
	session.mu.Unlock()
	return true
}
