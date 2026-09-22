package features

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSDKFirstInputDecision(t *testing.T) {
	for _, outcome := range []string{"accept", "drop", "cancel-before-begin", "cancel-before-accept", "close-host"} {
		t.Run(outcome, func(t *testing.T) {
			host := NewHost(nil, "session")
			defer host.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var retired atomic.Int32
			if outcome == "cancel-before-begin" {
				cancel()
			}
			lease, err := host.BeginInput(ctx, func() { retired.Add(1); host.Close() })
			if outcome == "cancel-before-begin" {
				if !errors.Is(err, context.Canceled) || lease != nil || retired.Load() != 1 {
					t.Fatal("cancelled first claim was not reclaimed", err)
				}
				return
			}
			if err != nil || lease == nil {
				t.Fatal("first input did not own admission", err)
			}
			if outcome == "cancel-before-accept" {
				cancel()
			}
			if outcome == "close-host" {
				host.Close()
			}
			if outcome != "drop" {
				err = lease.Accept()
				if (outcome == "accept") != (err == nil) {
					t.Fatal("incorrect admission outcome", err)
				}
			}
			lease.Close()
			lease.Close()
			if outcome == "accept" {
				cancel()
				if retired.Load() != 0 || host.Context().Err() != nil {
					t.Fatal("post-admission request cancellation retired the query")
				}
				later, err := host.BeginInput(t.Context(), func() { t.Error("later request reclaimed the first input") })
				if later != nil || err != nil {
					t.Fatal("accepted query demanded another first input", err)
				}
				later.Close()
			} else if retired.Load() != 1 || host.Context().Err() == nil || !errors.Is(lease.Accept(), context.Canceled) {
				t.Fatal("drop was not terminal and exactly once")
			}
		})
	}
}

type inputWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *inputWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSDKFirstInputWaitingRequest(t *testing.T) {
	for _, outcome := range []string{"accept", "drop", "cancel-waiter", "close-host"} {
		t.Run(outcome, func(t *testing.T) {
			host := NewHost(nil, "session")
			defer host.Close()
			lease, err := host.BeginInput(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			waiter, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := &inputWaitContext{Context: waiter, entered: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				next, err := host.BeginInput(observed, func() { t.Error("waiter retired first owner") })
				if next != nil {
					t.Error("waiter took another first lease")
					next.Close()
				}
				done <- err
			}()
			select {
			case <-observed.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("waiter did not reach pending admission")
			}
			switch outcome {
			case "accept":
				if err := lease.Accept(); err != nil {
					t.Fatal(err)
				}
			case "drop":
				lease.Close()
			case "cancel-waiter":
				cancel()
			case "close-host":
				host.Close()
			}
			select {
			case err := <-done:
				if (outcome == "accept") != (err == nil) {
					t.Fatal("waiter did not observe the first decision", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first decision stranded waiter")
			}
			if outcome == "cancel-waiter" && (host.Context().Err() != nil || lease.Accept() != nil) {
				t.Fatal("cancelled waiter invalidated the first owner")
			}
		})
	}
}

func TestSDKFirstInputConcurrentDecision(t *testing.T) {
	for range 64 {
		host := NewHost(nil, "session")
		var retired atomic.Int32
		lease, err := host.BeginInput(t.Context(), func() { retired.Add(1); host.Close() })
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(func() { _ = lease.Accept() })
		wg.Go(lease.Close)
		wg.Wait()
		if (lease.Accept() == nil) != (retired.Load() == 0) || retired.Load() > 1 {
			t.Fatal("concurrent decisions disagreed")
		}
		host.Close()
	}
}

func TestSDKFirstInputRetiresExactGenerationWithoutDeletingAlias(t *testing.T) {
	for _, transition := range []string{"none", "reidentify", "replace"} {
		t.Run(transition, func(t *testing.T) {
			aliases := &memoryStore{}
			r := NewRegistry(nil, aliases)
			defer r.Close()
			first, _ := r.ResolveQuery("first", "durable")
			sibling, _ := r.ResolveQuery("sibling", "durable")
			lease, err := first.BeginInput(t.Context(), func() { r.Retire(first) })
			if err != nil {
				t.Fatal(err)
			}
			var replacement *Host
			switch transition {
			case "reidentify":
				err = r.ReidentifyQuery(first, "first", "renamed", "durable", uuid.NewString())
			case "replace":
				replacement, err = r.Replace(first, "first", uuid.NewString())
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := r.QuerySession("unowned", "durable")
			writes := aliases.writes
			lease.Close()
			after, _ := r.QuerySession("unowned", "durable")
			if first.Context().Err() == nil || sibling.Context().Err() != nil || aliases.writes != writes || after != before {
				t.Fatal("failed admission changed durable history or sibling lifetime")
			}
			if replacement != nil {
				if r.Lookup("first") != replacement || replacement.Context().Err() != nil {
					t.Fatal("late failed claim retired its replacement")
				}
			} else if r.Lookup("first") != nil || r.Lookup("renamed") != nil {
				t.Fatal("failed claim remained registered")
			}
		})
	}
}
