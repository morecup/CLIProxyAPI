package helps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func executionMetadata(id string) map[string]any {
	return map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: id}
}

func TestClaudeDesktopExecutionSessionsOwnExactLifetimes(t *testing.T) {
	r := &ClaudeDesktopExecutionSessions{}
	a, err := r.Bind(t.Context(), executionMetadata("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Bind(t.Context(), executionMetadata("b"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope(a, "transcript") == r.Scope(b, "transcript") || r.Scope(t.Context(), "transcript") != "transcript" {
		t.Fatal("execution query scope merged with another connection or changed stateless identity")
	}
	var callsA, callsB atomic.Int32
	queryA, stopA := context.WithCancel(context.Background())
	queryB, stopB := context.WithCancel(context.Background())
	defer stopA()
	defer stopB()
	for range 2 {
		if !r.Own(a, "query-a", queryA, func() { callsA.Add(1); stopA() }) {
			t.Fatal("live query not owned")
		}
	}
	r.Own(b, "query-b", queryB, func() { callsB.Add(1); stopB() })
	r.Close("a")
	r.Close("a")
	if callsA.Load() != 1 || callsB.Load() != 0 || queryA.Err() == nil || queryB.Err() != nil {
		t.Fatal("close was duplicated or crossed execution ownership")
	}
	if _, err := r.Bind(context.WithoutCancel(a)); !errors.Is(err, context.Canceled) {
		t.Fatal("late helper escaped closure", err)
	}
	if _, err := r.Bind(t.Context(), executionMetadata("a")); !errors.Is(err, context.Canceled) {
		t.Fatal("closed ID was resurrected", err)
	}
	if _, err := r.Bind(a, executionMetadata("b")); err == nil {
		t.Fatal("helper switched connection")
	}
	other := &ClaudeDesktopExecutionSessions{}
	if _, err := other.Bind(b); err == nil {
		t.Fatal("query crossed router lifetime")
	}
	r.Close("b")
}

func TestClaudeDesktopExecutionSessionsCloseBeforeDispatchAndRegistration(t *testing.T) {
	for range 64 {
		r := &ClaudeDesktopExecutionSessions{}
		ctx, err := r.Bind(t.Context(), executionMetadata("queued"))
		if err != nil {
			t.Fatal(err)
		}
		query, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		var wg sync.WaitGroup
		wg.Go(func() { r.Close("queued") })
		wg.Go(func() { r.Own(ctx, "query", query, func() { calls.Add(1); cancel() }) })
		wg.Wait()
		if calls.Load() != 1 || query.Err() == nil {
			t.Fatal("registration raced past close")
		}
	}
	r := &ClaudeDesktopExecutionSessions{}
	r.Close("not-yet-dispatched")
	if _, err := r.Bind(t.Context(), executionMetadata("not-yet-dispatched")); !errors.Is(err, context.Canceled) {
		t.Fatal("unseen close did not tombstone queued dispatch", err)
	}
}

func TestClaudeDesktopExecutionSessionsRequestCompletionDoesNotCloseConnection(t *testing.T) {
	r := &ClaudeDesktopExecutionSessions{}
	bound, err := r.Bind(t.Context(), executionMetadata("connection"))
	if err != nil {
		t.Fatal(err)
	}
	request, release := BindClaudeDesktopQueryLifetime(bound, r.Lifetime(bound))
	release()
	if request.Err() == nil || r.Lifetime(bound).Err() != nil {
		t.Fatal("request cleanup retired its execution")
	}
	if _, err := r.Bind(t.Context(), executionMetadata("connection")); err != nil {
		t.Fatal(err)
	}
	r.Close("connection")
}
