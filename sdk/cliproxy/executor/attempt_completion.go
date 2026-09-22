package executor

import (
	"context"
	"sort"
	"sync"
)

// BeginUpstreamCompletionScope keeps attempt failures provisional until the
// outermost conductor has exhausted retries or finished its response stream.
// Nested conductors share a scope count; releasing an inner scope cannot
// terminate an attempt that an outer conductor may still retry.
func BeginUpstreamCompletionScope(ctx context.Context) func() {
	chain := completionChain(ctx)
	if chain == nil {
		return func() {}
	}
	chain.completionMu.Lock()
	chain.completionScopes++
	chain.completionMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			chain.completionMu.Lock()
			chain.completionScopes--
			var callbacks map[string]func()
			if chain.completionScopes == 0 {
				callbacks = chain.finalizers
				chain.finalizers = nil
				chain.invocationValues = nil
			}
			chain.completionMu.Unlock()
			keys := make([]string, 0, len(callbacks))
			for key := range callbacks {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				callbacks[key]()
			}
		})
	}
}

// LoadUpstreamInvocationValue reads request-local provider state. Keys should
// be private comparable types. Values are never serialized and are released
// with the outer completion scope, including failure and cancellation paths.
func LoadUpstreamInvocationValue(ctx context.Context, key any) (any, bool) {
	chain := completionChain(ctx)
	if chain == nil {
		return nil, false
	}
	chain.completionMu.Lock()
	defer chain.completionMu.Unlock()
	value, ok := chain.invocationValues[key]
	return value, ok && chain.completionScopes > 0
}

func StoreUpstreamInvocationValue(ctx context.Context, key, value any) bool {
	chain := completionChain(ctx)
	if chain == nil || key == nil || value == nil {
		return false
	}
	chain.completionMu.Lock()
	defer chain.completionMu.Unlock()
	if chain.completionScopes == 0 {
		return false
	}
	if _, exists := chain.invocationValues[key]; !exists && len(chain.invocationValues) >= 64 {
		return false
	}
	if chain.invocationValues == nil {
		chain.invocationValues = make(map[any]any)
	}
	chain.invocationValues[key] = value
	return true
}

// RegisterUpstreamFailureFinalizer replaces an earlier failed attempt for the
// same logical call. False means no conductor owns this call; its executor can
// finalize the direct invocation immediately. Callbacks run outside locks.
func RegisterUpstreamFailureFinalizer(ctx context.Context, key string, callback func()) bool {
	chain := completionChain(ctx)
	if chain == nil || key == "" || callback == nil {
		return false
	}
	chain.completionMu.Lock()
	defer chain.completionMu.Unlock()
	if chain.completionScopes == 0 {
		return false
	}
	if chain.finalizers == nil {
		chain.finalizers = make(map[string]func())
	}
	chain.finalizers[key] = callback
	return true
}

// ClearUpstreamFailureFinalizer acknowledges a successful retry without
// emitting the prior attempt's terminal failure.
func ClearUpstreamFailureFinalizer(ctx context.Context, key string) {
	if chain := completionChain(ctx); chain != nil {
		chain.completionMu.Lock()
		delete(chain.finalizers, key)
		chain.completionMu.Unlock()
	}
}

func completionChain(ctx context.Context) *upstreamAttemptChain {
	if ctx == nil {
		return nil
	}
	if chain, ok := ctx.Value(upstreamCompletionChainContextKey{}).(*upstreamAttemptChain); ok && chain != nil {
		return chain
	}
	chain, _ := ctx.Value(upstreamAttemptChainContextKey{}).(*upstreamAttemptChain)
	return chain
}
