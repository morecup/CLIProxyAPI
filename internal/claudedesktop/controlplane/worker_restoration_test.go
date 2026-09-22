package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func workerRestorationTestSession(t *testing.T, payload string) (*sessionRuntime, *epochChangingDoer, *atomic.Int32) {
	t.Helper()
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	doer, reads := &epochChangingDoer{}, &atomic.Int32{}
	m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return credentialBudgetDoerFunc(func(request *http.Request) (*http.Response, error) {
				response, err := doer.Do(request)
				if err == nil && request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/worker") {
					reads.Add(1)
					_ = response.Body.Close()
					response.Body = io.NopCloser(strings.NewReader(payload))
				}
				return response, err
			}), nil
		}})
	t.Cleanup(m.Close)
	if err := m.EnsureSession(t.Context(), testDesktopAuth(t), "restore-worker", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	s := m.sessions["restore-worker"]
	s.opMu.Lock()
	s.workerRestoration, s.state.WorkerInitialized = nil, false
	s.opMu.Unlock()
	return s, doer, reads
}

func TestWorkerRestorationKeepsOriginalReadAcrossConsumerRetry(t *testing.T) {
	s, doer, reads := workerRestorationTestSession(t, `{"worker":{"external_metadata":{"model":"owned","flag":false,"zero":0,"empty":""},"internal_metadata":{"pending":[{"type":"synthetic"}]}}}`)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	calls := 0
	s.inbound = &InboundConsumer{RestoreWorker: func(ctx context.Context, state *WorkerRestoration) error {
		calls++
		if state.WorkerEpoch() != 1 || string(state.ExternalMetadata()) != `{"model":"owned","flag":false,"zero":0,"empty":""}` || string(state.InternalMetadata()) != `{"pending":[{"type":"synthetic"}]}` {
			t.Fatal("read metadata changed")
		}
		copy := state.ExternalMetadata()
		copy[0] = 'x'
		if !json.Valid(state.ExternalMetadata()) {
			t.Fatal("consumer mutated retained read")
		}
		if _, err := json.Marshal(state); err == nil {
			t.Fatal("private worker metadata serialized")
		}
		requests := doer.snapshot()
		if requests[len(requests)-1].method != http.MethodPut {
			t.Fatal("callback ran before worker registration")
		}
		if calls == 1 {
			return errors.New("synthetic consumer persistence failure")
		}
		return ctx.Err()
	}}
	if err := s.initializeWorkerLocked(t.Context()); err == nil || s.workerRestoration.applied {
		t.Fatal("consumer failure accepted")
	}
	if err := s.initializeWorkerLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.restoreWorkerLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reads.Load() != 2 || !s.workerRestoration.applied {
		t.Fatal("retry reread overwritten remote state or repeated consumer", calls, reads.Load())
	}
}

func TestWorkerRestorationRejectsEpochRenewalDuringRegistration(t *testing.T) {
	s, doer, reads := workerRestorationTestSession(t, `{"worker":{"external_metadata":{"model":"owned"}}}`)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var epoch int64
	s.inbound = &InboundConsumer{RestoreWorker: func(_ context.Context, state *WorkerRestoration) error { epoch = state.WorkerEpoch(); return nil }}
	doer.gate.Lock()
	doer.failNext = true
	doer.gate.Unlock()
	if err := s.initializeWorkerLocked(t.Context()); !errors.Is(err, errWorkerRestorationStale) || epoch != 0 {
		t.Fatal("old epoch published after renewal", err, epoch)
	}
	if err := s.initializeWorkerLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
	if epoch != 2 || reads.Load() != 3 {
		t.Fatal("current epoch did not get its own read", epoch, reads.Load())
	}
}

func TestWorkerRestorationMissingMetadataAndCancellation(t *testing.T) {
	for _, payload := range []string{`{}`, `null`, `{"worker":null}`, `{"worker":{"external_metadata":null,"internal_metadata":null}}`} {
		t.Run(payload, func(t *testing.T) {
			s, _, _ := workerRestorationTestSession(t, payload)
			s.opMu.Lock()
			defer s.opMu.Unlock()
			calls := 0
			s.inbound = &InboundConsumer{RestoreWorker: func(_ context.Context, state *WorkerRestoration) error {
				calls++
				if string(state.ExternalMetadata()) != "null" || string(state.InternalMetadata()) != "null" {
					t.Fatal("missing state invented")
				}
				return nil
			}}
			if err := s.readWorkerRestorationLocked(t.Context()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := s.restoreWorkerLocked(ctx); !errors.Is(err, context.Canceled) || calls != 0 {
				t.Fatal("cancelled restoration consumed", err)
			}
			if err := s.restoreWorkerLocked(t.Context()); err != nil || calls != 1 {
				t.Fatal("empty restoration not delivered", err)
			}
		})
	}
}
