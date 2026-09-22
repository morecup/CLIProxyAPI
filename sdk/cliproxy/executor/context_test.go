package executor

import (
	"context"
	"testing"
	"time"
)

func TestUpstreamAttemptChainIsMonotonicAndPreserved(t *testing.T) {
	chainStart := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	firstStart := chainStart.Add(10 * time.Millisecond)
	secondStart := chainStart.Add(30 * time.Millisecond)
	ctx := WithUpstreamAttemptChain(context.Background(), chainStart)
	firstCtx := WithNextUpstreamAttempt(ctx, firstStart)
	secondCtx := WithNextUpstreamAttempt(ctx, secondStart)

	first, okFirst := UpstreamAttemptFromContext(firstCtx)
	second, okSecond := UpstreamAttemptFromContext(secondCtx)
	if !okFirst || !okSecond {
		t.Fatal("attempt metadata was not available")
	}
	if first.Number != 1 || second.Number != 2 {
		t.Fatalf("attempt numbers = %d, %d; want 1, 2", first.Number, second.Number)
	}
	if first.ChainStartedAt != chainStart || second.ChainStartedAt != chainStart {
		t.Fatalf("chain start changed: first=%v second=%v", first.ChainStartedAt, second.ChainStartedAt)
	}
	if first.StartedAt != firstStart || second.StartedAt != secondStart {
		t.Fatalf("attempt starts = %v, %v", first.StartedAt, second.StartedAt)
	}

	nestedStart := chainStart.Add(time.Hour)
	nested := WithUpstreamAttemptChain(secondCtx, nestedStart)
	third, okThird := UpstreamAttemptFromContext(WithNextUpstreamAttempt(nested, nestedStart))
	if !okThird || third.Number != 3 || third.ChainStartedAt != chainStart {
		t.Fatalf("nested attempt = %+v, available=%v", third, okThird)
	}
}

func TestIndependentUpstreamAttemptPreservesCancellationNotParentAccounting(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	parent = WithNextUpstreamAttempt(parent, time.Unix(100, 0))
	parent = WithNextUpstreamAttempt(parent, time.Unix(101, 0))
	child := WithIndependentUpstreamAttempt(parent, time.Unix(200, 0))
	got, ok := UpstreamAttemptFromContext(child)
	if !ok || got.Number != 1 || got.StartedAt != time.Unix(200, 0) || got.ChainStartedAt != got.StartedAt {
		t.Fatalf("side-query inherited parent attempt: %+v", got)
	}
	if parent.Value(upstreamAttemptChainContextKey{}) == child.Value(upstreamAttemptChainContextKey{}) {
		t.Fatal("side-query shares its parent's failure finalizers")
	}
	next, _ := UpstreamAttemptFromContext(WithNextUpstreamAttempt(parent, time.Unix(102, 0)))
	if next.Number != 3 || next.ChainStartedAt != time.Unix(100, 0) {
		t.Fatal("side-query advanced or reset the parent retry chain")
	}
	cancel()
	if child.Err() != context.Canceled {
		t.Fatal("side-query lost parent cancellation")
	}
}

func TestFreshUpstreamQueryKeepsOuterCompletionOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = WithNextUpstreamAttempt(ctx, time.Unix(100, 0))
	ctx = WithNextUpstreamAttempt(ctx, time.Unix(101, 0))
	release := BeginUpstreamCompletionScope(ctx)
	defer release()
	old, next := 0, 0
	if !RegisterUpstreamFailureFinalizer(ctx, "old-query", func() { old++ }) {
		t.Fatal("no outer owner")
	}
	fresh := WithFreshUpstreamQuery(ctx, time.Unix(200, 0))
	attempt, _ := UpstreamAttemptFromContext(fresh)
	if attempt.Number != 1 || attempt.ChainStartedAt != time.Unix(200, 0) {
		t.Fatal("new query inherited old API attempt accounting")
	}
	ClearUpstreamFailureFinalizer(fresh, "old-query")
	if !RegisterUpstreamFailureFinalizer(fresh, "new-query", func() { next++ }) {
		t.Fatal("new query lost the conductor's completion owner")
	}
	helper := WithIndependentUpstreamAttempt(fresh, time.Unix(201, 0))
	if RegisterUpstreamFailureFinalizer(helper, "new-query", func() { t.Error("helper crossed into parent scope") }) {
		t.Fatal("helper reused inherited completion owner")
	}
	resumed, sameOwner := WithNextOwnedUpstreamQueryAttempt(ctx, fresh, time.Unix(202, 0))
	if !sameOwner || !RegisterUpstreamFailureFinalizer(resumed, "new-query", func() { next++ }) {
		t.Fatal("resumed query lost completion owner")
	}
	if attempt, _ := UpstreamAttemptFromContext(resumed); attempt.Number != 2 || attempt.ChainStartedAt != time.Unix(200, 0) {
		t.Fatal("resumed query lost independent API clock")
	}
	if _, ok := WithNextOwnedUpstreamQueryAttempt(helper, fresh, time.Unix(203, 0)); ok {
		t.Fatal("helper resumed a parent query")
	}
	if !StoreUpstreamInvocationValue(resumed, "synthetic", "request-local") {
		t.Fatal("could not retain request-local continuation")
	}
	if value, ok := LoadUpstreamInvocationValue(ctx, "synthetic"); !ok || value != "request-local" {
		t.Fatal("outer invocation lost resumed state")
	}
	if _, ok := LoadUpstreamInvocationValue(helper, "synthetic"); ok {
		t.Fatal("helper read parent continuation state")
	}
	if next != 0 || old != 0 {
		t.Fatal("provisional failure completed early")
	}
	release()
	if _, ok := LoadUpstreamInvocationValue(ctx, "synthetic"); ok {
		t.Fatal("completion retained request-local plaintext")
	}
	if old != 0 || next != 1 {
		t.Fatalf("wrong finalizer ownership: old=%d next=%d", old, next)
	}
	cancel()
	if fresh.Err() != context.Canceled || helper.Err() != context.Canceled {
		t.Fatal("query/helper lost cancellation")
	}
}
