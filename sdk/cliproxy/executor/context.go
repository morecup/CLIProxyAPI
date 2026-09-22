package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type downstreamWebsocketContextKey struct{}
type requireUpstreamWebsocketContextKey struct{}
type upstreamAttemptChainContextKey struct{}
type upstreamAttemptContextKey struct{}
type upstreamCompletionChainContextKey struct{}

type upstreamAttemptChain struct {
	startedAt        time.Time
	next             atomic.Int64
	completionMu     sync.Mutex
	completionScopes int
	finalizers       map[string]func()
	invocationValues map[any]any
}

// UpstreamAttempt identifies one concrete provider invocation inside a
// request-level retry chain.
type UpstreamAttempt struct {
	Number         int
	ChainStartedAt time.Time
	StartedAt      time.Time
}

// WithUpstreamAttemptChain starts request-level attempt accounting unless the
// context already belongs to a chain. Nested conductors therefore preserve the
// original start time and monotonic attempt sequence.
func WithUpstreamAttemptChain(ctx context.Context, startedAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if chain, ok := ctx.Value(upstreamAttemptChainContextKey{}).(*upstreamAttemptChain); ok && chain != nil {
		return ctx
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return context.WithValue(ctx, upstreamAttemptChainContextKey{}, &upstreamAttemptChain{startedAt: startedAt})
}

// WithIndependentUpstreamAttempt starts an owned side-query, not a retry of
// the parent API call. Cancellation and unrelated context values are retained;
// attempt numbering, start time and failure finalizers cannot cross the fork.
func WithIndependentUpstreamAttempt(ctx context.Context, startedAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	chain := &upstreamAttemptChain{startedAt: startedAt}
	ctx = context.WithValue(ctx, upstreamAttemptChainContextKey{}, chain)
	ctx = context.WithValue(ctx, upstreamCompletionChainContextKey{}, chain)
	return WithNextUpstreamAttempt(ctx, startedAt)
}

// WithFreshUpstreamQuery starts a new API query inside the same logical
// invocation. Unlike an independent helper, its terminal decision still
// belongs to the outer conductor's completion scope.
func WithFreshUpstreamQuery(ctx context.Context, startedAt time.Time) context.Context {
	ctx = WithUpstreamAttemptChain(ctx, startedAt)
	owner := completionChain(ctx)
	ctx = WithIndependentUpstreamAttempt(ctx, startedAt)
	return context.WithValue(ctx, upstreamCompletionChainContextKey{}, owner)
}

// WithNextOwnedUpstreamQueryAttempt preserves a resumed query's API retry
// clock and counter while keeping the current invocation's cancellation and
// completion owner. A helper cannot resume a query from another invocation.
func WithNextOwnedUpstreamQueryAttempt(ctx, query context.Context, startedAt time.Time) (context.Context, bool) {
	if ctx == nil || query == nil || completionChain(ctx) == nil || completionChain(ctx) != completionChain(query) {
		return ctx, false
	}
	chain, ok := query.Value(upstreamAttemptChainContextKey{}).(*upstreamAttemptChain)
	if !ok || chain == nil {
		return ctx, false
	}
	ctx = context.WithValue(ctx, upstreamCompletionChainContextKey{}, completionChain(ctx))
	return WithNextUpstreamAttempt(context.WithValue(ctx, upstreamAttemptChainContextKey{}, chain), startedAt), true
}

// WithNextUpstreamAttempt marks the next concrete provider invocation. It is
// intentionally separate from auth refresh and credential selection contexts,
// neither of which is an API attempt.
func WithNextUpstreamAttempt(ctx context.Context, startedAt time.Time) context.Context {
	ctx = WithUpstreamAttemptChain(ctx, startedAt)
	chain, _ := ctx.Value(upstreamAttemptChainContextKey{}).(*upstreamAttemptChain)
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	attempt := UpstreamAttempt{
		Number:         int(chain.next.Add(1)),
		ChainStartedAt: chain.startedAt,
		StartedAt:      startedAt,
	}
	return context.WithValue(ctx, upstreamAttemptContextKey{}, attempt)
}

// UpstreamAttemptFromContext returns the current concrete provider attempt.
func UpstreamAttemptFromContext(ctx context.Context) (UpstreamAttempt, bool) {
	if ctx == nil {
		return UpstreamAttempt{}, false
	}
	attempt, ok := ctx.Value(upstreamAttemptContextKey{}).(UpstreamAttempt)
	return attempt, ok && attempt.Number > 0 && !attempt.ChainStartedAt.IsZero() && !attempt.StartedAt.IsZero()
}

// WithDownstreamWebsocket marks the current request as coming from a downstream websocket connection.
func WithDownstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, downstreamWebsocketContextKey{}, true)
}

// DownstreamWebsocket reports whether the current request originates from a downstream websocket connection.
func DownstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(downstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithRequiredUpstreamWebsocket marks a request whose incremental context is valid only on the current upstream websocket.
func WithRequiredUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requireUpstreamWebsocketContextKey{}, true)
}

// RequiredUpstreamWebsocket reports whether falling back to an HTTP upstream would lose request context.
func RequiredUpstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(requireUpstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}
