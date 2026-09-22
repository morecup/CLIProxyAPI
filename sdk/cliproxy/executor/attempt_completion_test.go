package executor

import (
	"context"
	"testing"
	"time"
)

func TestAttemptCompletionIsOwnedByOutermostScope(t *testing.T) {
	ctx := WithUpstreamAttemptChain(context.Background(), time.Now())
	outer := BeginUpstreamCompletionScope(ctx)
	inner := BeginUpstreamCompletionScope(ctx)
	count := 0
	if !RegisterUpstreamFailureFinalizer(ctx, "prompt", func() { count += 100 }) {
		t.Fatal("scope unavailable")
	}
	RegisterUpstreamFailureFinalizer(ctx, "prompt", func() { count++ })
	inner()
	if count != 0 {
		t.Fatal("inner conductor terminated an outer retry")
	}
	outer()
	outer()
	if count != 1 {
		t.Fatalf("terminal callbacks=%d want=1", count)
	}
	if RegisterUpstreamFailureFinalizer(ctx, "late", func() {}) {
		t.Fatal("closed scope accepted a late callback")
	}
}

func TestSuccessfulRetryClearsOnlyItsOwnFailure(t *testing.T) {
	ctx := WithUpstreamAttemptChain(context.Background(), time.Now())
	release := BeginUpstreamCompletionScope(ctx)
	a, b := 0, 0
	RegisterUpstreamFailureFinalizer(ctx, "account-a/prompt", func() { a++ })
	RegisterUpstreamFailureFinalizer(ctx, "account-b/prompt", func() { b++ })
	ClearUpstreamFailureFinalizer(ctx, "account-b/prompt")
	release()
	if a != 1 || b != 0 {
		t.Fatalf("failure isolation a=%d b=%d", a, b)
	}
}

func TestDirectAttemptHasNoDeferredCompletionScope(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background(), WithNextUpstreamAttempt(context.Background(), time.Now())} {
		if RegisterUpstreamFailureFinalizer(ctx, "direct", func() {}) {
			t.Fatal("direct executor failure was deferred indefinitely")
		}
	}
}
