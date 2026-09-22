package features

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// Host is one live SDK query instance. Session identity may change without
// replacing it; a new query must receive a new Host even when resuming the same
// transcript. Host state and exposure acknowledgments are never persisted.
type Host struct {
	mu      sync.RWMutex
	id      string
	service *Service
	session string
	ctx     context.Context
	cancel  context.CancelFunc

	// Native sticky beta decisions belong to the live SDK instance, not to
	// a transcript, account-wide cache or a persisted feature value.
	rejectedBetas map[string]bool

	inputMu       sync.Mutex
	inputPending  chan struct{}
	inputAccepted bool
	inputDropped  bool

	shellTelemetryMu       sync.RWMutex
	shellTelemetryObserver func(context.Context, ShellTelemetryEvent) error
}

func NewHost(store Store, session string) *Host {
	ctx, cancel := context.WithCancel(context.Background())
	if session == "" {
		session = uuid.NewString()
	}
	return &Host{id: uuid.NewString(), service: New(store), session: session, ctx: ctx, cancel: cancel}
}

// ID identifies this live query, never a transcript or a durable alias.
func (h *Host) ID() string               { return h.id }
func (h *Host) Service() *Service        { return h.service }
func (h *Host) Context() context.Context { return h.ctx }
func (h *Host) SessionID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.session
}

// Reidentify models a root-session change, not process replacement. Existing
// references and feature deduplication keep following the same host.
func (h *Host) Reidentify(session string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if session == "" || h.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop SDK host is unavailable")
	}
	h.session = session
	return nil
}

func (h *Host) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancel()
	h.service.Close()
}

// RejectBetaForSession records an attributed server rejection on this live
// instance. Repeated in-flight rejections are idempotent. Reidentify retains
// the instance; a replacement Host starts with a fresh beta state. The held
// request's session is checked under the mutation lock after response I/O.
func (h *Host) RejectBetaForSession(session, beta string) bool {
	if h == nil || session == "" || beta == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil || h.session != session {
		return false
	}
	if h.rejectedBetas == nil {
		h.rejectedBetas = make(map[string]bool)
	}
	h.rejectedBetas[beta] = true
	return true
}

func (h *Host) BetaRejectedForSession(session, beta string) bool {
	if h == nil || session == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ctx.Err() == nil && h.session == session && h.rejectedBetas[beta]
}
