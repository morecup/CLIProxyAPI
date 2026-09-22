package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDeliveryFirstBatchDoesNotBlockIngressAndFlushesInOrder(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("delivery", t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	s := span.session
	posted := make(chan []deliveryUpdate, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/worker/events/delivery") {
				var body struct {
					WorkerEpoch int64            `json:"worker_epoch"`
					Updates     []deliveryUpdate `json:"updates"`
				}
				raw, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					return nil, err
				}
				if body.WorkerEpoch != 1 {
					t.Error("delivery epoch not bound")
				}
				posted <- body.Updates
				if calls.Add(1) == 1 {
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}), nil
	}
	m.disableLoops = false
	s.enqueueDelivery("one", "received")
	first := awaitQueryControl(t, posted)
	if len(first) != 1 || first[0].Status != "received" {
		t.Fatal("wrong first batch", first)
	}
	enqueued := make(chan struct{})
	go func() { s.enqueueDelivery("one", "processed"); s.enqueueDelivery("two", "received"); close(enqueued) }()
	awaitQueryControl(t, enqueued)
	if calls.Load() != 1 {
		t.Fatal("parallel delivery batches overlapped")
	}
	close(release)
	second := awaitQueryControl(t, posted)
	if len(second) != 2 || second[0].Status != "processed" || second[1].EventID != "two" {
		t.Fatal("pending order lost", second)
	}
	m.Close()
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if len(s.deliveryPending) != 0 || len(s.deliveryWaiting) != 0 || s.deliveryRunning {
		t.Fatal("closed uploader retained pending work")
	}
}

func TestDeliveryBackpressureAndTerminalDisposition(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("backpressure", t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	s := span.session
	for i := 0; i < 70; i++ {
		s.enqueueDelivery("event", "received")
	}
	s.deliveryMu.Lock()
	if len(s.deliveryPending) != 64 || len(s.deliveryWaiting) != 6 {
		t.Fatal("native pending/waiting capacity lost")
	}
	batch := s.takeDeliveryBatchLocked()
	s.releaseDeliveryBackpressureLocked()
	if len(batch) != 64 || len(s.deliveryPending) != 6 || len(s.deliveryWaiting) != 0 {
		t.Fatal("waiting updates were dropped")
	}
	s.deliveryMu.Unlock()
	for _, code := range []int{400, 413, 422} {
		if err := s.deliveryDisposition(&statusError{code: code}); err != nil {
			t.Fatal("terminal batch retried", code)
		}
	}
	if err := s.deliveryDisposition(&statusError{code: 502}); err == nil {
		t.Fatal("retryable batch dropped")
	}
	for _, attempt := range []int{1, 5, 20} {
		delay := deliveryRetryDelay(attempt, nil)
		if delay < 500*time.Millisecond || delay >= 30500*time.Millisecond {
			t.Fatal("retry cadence out of range", delay)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if waitDeliveryRetry(ctx, time.Hour) {
		t.Fatal("retry ignored cancellation")
	}
	m.Quarantine()
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if len(s.deliveryPending) != 0 || len(s.deliveryWaiting) != 0 {
		t.Fatal("quarantine retained process-local delivery queue")
	}
}
