package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestWorkerInitializationReadFailureDoesNotPreventRegistration(t *testing.T) {
	for _, status := range []int{400, 413, 422, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, _, _ := workerRestorationTestSession(t, `{}`)
			var reads, puts atomic.Int32
			s.manager.workerReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			s.manager.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
				return credentialBudgetDoerFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == "GET" {
						reads.Add(1)
						return internalEventTestResponse(status, `{}`), nil
					}
					puts.Add(1)
					return internalEventTestResponse(200, `{}`), nil
				}), nil
			}
			restored := false
			s.inbound = &InboundConsumer{RestoreWorker: func(_ context.Context, state *WorkerRestoration) error {
				restored = true
				if !state.ReadFailed() || string(state.ExternalMetadata()) != "null" || string(state.InternalMetadata()) != "null" || puts.Load() != 2 {
					t.Fatal("failed GET did not reach native post-registration restoration")
				}
				return nil
			}}
			s.opMu.Lock()
			err := s.initializeWorkerLocked(t.Context())
			s.opMu.Unlock()
			want := int32(1)
			if status == 500 {
				want = 10
			}
			if err != nil || !restored || reads.Load() != want || s.manager.Status().WorkerStateReadFailed != 1 {
				t.Fatal("GET failure incorrectly rejected registration or hid degraded health", err, reads.Load())
			}
		})
	}
}

func TestWorkerInitializationJoinsSlowOriginalReadAfterRegistration(t *testing.T) {
	s, _, _ := workerRestorationTestSession(t, `{}`)
	started, registered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var reads, puts atomic.Int32
	s.manager.workerReadOrderingWait = func(ctx context.Context, done <-chan struct{}) error {
		select {
		case <-started:
			// Advance the native ordering timer without installing a network
			// deadline or completing/cancelling the original GET.
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.manager.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" {
				reads.Add(1)
				if _, deadline := r.Context().Deadline(); deadline {
					return nil, errors.New("unexpected network deadline")
				}
				once.Do(func() { close(started) })
				select {
				case <-registered:
					return internalEventTestResponse(200, `{"worker":{"external_metadata":{"model":"original-before-PUT"}}}`), nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			if puts.Add(1) == 2 {
				close(registered)
			}
			return internalEventTestResponse(200, `{}`), nil
		}), nil
	}
	s.inbound = &InboundConsumer{RestoreWorker: func(_ context.Context, state *WorkerRestoration) error {
		if state.ReadFailed() || !strings.Contains(string(state.ExternalMetadata()), "original-before-PUT") || puts.Load() != 2 || reads.Load() != 1 {
			t.Fatal("slow GET was lost, cancelled or reread after destructive registration fields")
		}
		return nil
	}}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.initializeWorkerLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerInitializationRegistrationRetriesActualPUT(t *testing.T) {
	s, _, _ := workerRestorationTestSession(t, `{}`)
	var puts atomic.Int32
	s.manager.workerReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.manager.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "PUT" && puts.Add(1) < 3 {
				return internalEventTestResponse(502, `{}`), nil
			}
			return internalEventTestResponse(200, `{}`), nil
		}), nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.initializeWorkerLocked(t.Context()); err != nil || puts.Load() != 4 {
		t.Fatal("registration retries did not precede the permission update", err, puts.Load())
	}
}

func TestWorkerStateReadDoesNotBorrowInternalHistoryAnchorPolicy(t *testing.T) {
	s, _, _ := workerRestorationTestSession(t, `{}`)
	var reads atomic.Int32
	s.manager.workerReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.manager.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" {
				reads.Add(1)
				return internalEventTestResponse(404, `{"error":{"type":"after_event_id_not_found"}}`), nil
			}
			return internalEventTestResponse(200, `{}`), nil
		}), nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.initializeWorkerLocked(t.Context()); err != nil || reads.Load() != 10 || !s.workerRestoration.ReadFailed() {
		t.Fatal("worker-state GET inherited the paginated reader's permanent-anchor disposition", err, reads.Load())
	}
}

func TestWorkerInitializationConflictCannotRegisterSuccessor(t *testing.T) {
	s, _, _ := workerRestorationTestSession(t, `{}`)
	var puts atomic.Int32
	s.manager.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				puts.Add(1)
			}
			return internalEventTestResponse(409, `{"error":{"reason":"session_not_active"}}`), nil
		}), nil
	}
	s.opMu.Lock()
	err := s.initializeWorkerLocked(t.Context())
	s.opMu.Unlock()
	var conflict *WorkerEpochConflict
	if !errors.As(err, &conflict) || conflict.Reason != "session_not_active" || s.ctx.Err() == nil || puts.Load() != 0 {
		t.Fatal("terminal GET conflict continued registration", err, puts.Load())
	}
	// The fixture had an initialized bridge before the failing new worker
	// read. Even that old bridge cannot archive the remotely owned successor.
	if err := s.shutdown(t.Context()); err != nil || puts.Load() != 0 {
		t.Fatal("conflicted worker mutated remote state during teardown", err)
	}
}
