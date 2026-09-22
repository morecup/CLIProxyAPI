package features

import (
	"context"
	"sync"
)

// InputLease owns the first input's admission, not its model API outcome.
// Desktop drops a failed prewarm claim when sendMessage does not accept input;
// later API errors must not retrospectively turn accepted input into a drop.
type InputLease struct {
	host   *Host
	ctx    context.Context
	retire func()
	once   sync.Once
	err    error
}

// BeginInput serializes only the first admission decision. Waiting requests
// retain the exact query generation and cannot retire another request's claim.
func (h *Host) BeginInput(ctx context.Context, retire func()) (*InputLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		h.inputMu.Lock()
		if h.inputDropped || h.ctx.Err() != nil {
			h.inputMu.Unlock()
			return nil, context.Canceled
		}
		if h.inputAccepted {
			h.inputMu.Unlock()
			return nil, ctx.Err()
		}
		pending := h.inputPending
		if pending == nil {
			h.inputPending = make(chan struct{})
			lease := &InputLease{host: h, ctx: ctx, retire: retire}
			h.inputMu.Unlock()
			if err := ctx.Err(); err != nil {
				lease.Close()
				return nil, err
			}
			return lease, nil
		}
		h.inputMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, context.Canceled
		case <-pending:
		}
	}
}

// Accept commits at the input consumer boundary, before model dispatch. A
// queued input counts as accepted without waiting for any upstream response.
func (l *InputLease) Accept() error { return l.finish(true) }

// Close reclaims only an unaccepted first claim. The callback must retire the
// exact Host, without deleting its durable transcript or any replacement.
func (l *InputLease) Close() { _ = l.finish(false) }

func (l *InputLease) finish(accept bool) error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		h := l.host
		h.inputMu.Lock()
		l.err = l.ctx.Err()
		if l.err == nil {
			l.err = h.ctx.Err()
		}
		accepted := accept && l.err == nil
		if !accepted && l.err == nil {
			l.err = context.Canceled
		}
		h.inputAccepted, h.inputDropped = accepted, !accepted
		close(h.inputPending)
		h.inputMu.Unlock()
		if !accepted {
			if l.retire != nil {
				l.retire()
			} else {
				h.Close()
			}
		}
	})
	return l.err
}
