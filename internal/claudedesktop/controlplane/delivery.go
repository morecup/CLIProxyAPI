package controlplane

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"net/http"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type deliveryUpdate struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// The query's delivery uploader is process-local, unlike durable telemetry
// queues. Native close drops pending updates after any explicit final flush.
func (s *sessionRuntime) closeDelivery() {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	s.deliveryPending, s.deliveryWaiting = nil, nil
	s.deliveryRunning = false
}

// The native delivery uploader admits at most 64 pending updates; blocked
// enqueue promises retain the remainder. In-flight updates do not consume the
// pending capacity, and failed batches are prepended before the next retry.
func (s *sessionRuntime) enqueueDelivery(eventID, status string) {
	if eventID == "" {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	s.deliveryMu.Lock()
	update := deliveryUpdate{eventID, status}
	if len(s.deliveryPending) < 64 && len(s.deliveryWaiting) == 0 {
		s.deliveryPending = append(s.deliveryPending, update)
	} else {
		s.deliveryWaiting = append(s.deliveryWaiting, update)
	}
	if s.deliveryRunning || s.manager.disableLoops {
		s.deliveryMu.Unlock()
		return
	}
	s.deliveryRunning = true
	first := s.takeDeliveryBatchLocked()
	s.deliveryMu.Unlock()
	s.loops.Add(1)
	go s.deliveryLoop(first)
}

func (s *sessionRuntime) takeDeliveryBatchLocked() []deliveryUpdate {
	count := min(64, len(s.deliveryPending))
	batch := append([]deliveryUpdate(nil), s.deliveryPending[:count]...)
	s.deliveryPending = append([]deliveryUpdate(nil), s.deliveryPending[count:]...)
	return batch
}

func (s *sessionRuntime) releaseDeliveryBackpressureLocked() {
	count := min(max(0, 64-len(s.deliveryPending)), len(s.deliveryWaiting))
	s.deliveryPending = append(s.deliveryPending, s.deliveryWaiting[:count]...)
	s.deliveryWaiting = append([]deliveryUpdate(nil), s.deliveryWaiting[count:]...)
}

func (s *sessionRuntime) sendDeliveryLocked(ctx context.Context, batch []deliveryUpdate) error {
	epoch, err := s.workerEpochLocked()
	if err != nil {
		return err
	}
	err = s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerDelivery, struct {
		WorkerEpoch int64            `json:"worker_epoch"`
		Updates     []deliveryUpdate `json:"updates"`
	}{epoch, batch}, nil)
	return s.deliveryDisposition(err)
}

func (s *sessionRuntime) deliveryDisposition(err error) error {
	var status *statusError
	if errors.As(err, &status) && (status.code == 400 || status.code == 413 || status.code == 422) {
		s.inboundFailed.Add(1)
		return nil
	}
	return err
}

func (s *sessionRuntime) sendDelivery(ctx context.Context, batch []deliveryUpdate) error {
	var previous *http.Request
	for attempt := 0; attempt < 2; attempt++ {
		s.opMu.Lock()
		if err := ctx.Err(); err != nil {
			s.opMu.Unlock()
			return err
		}
		if s.bridgeExpiredLocked() || previous != nil && previous.Header.Get("Authorization") == "Bearer "+s.state.WorkerJWT {
			if err := s.bridgeLocked(ctx); err != nil {
				s.opMu.Unlock()
				return err
			}
		}
		epoch, err := s.workerEpochLocked()
		if err != nil {
			s.opMu.Unlock()
			return err
		}
		request, doer, err := s.buildRequest(ctx, claudeprofile.ControlEndpointWorkerDelivery, s.state.RemoteSessionID, struct {
			WorkerEpoch int64            `json:"worker_epoch"`
			Updates     []deliveryUpdate `json:"updates"`
		}{epoch, batch})
		s.opMu.Unlock()
		if err != nil {
			return err
		}
		err = s.manager.performJSON(request, doer, nil, nil)
		var status *statusError
		if attempt == 0 && errors.As(err, &status) && (status.code == 401 || status.code == 403) {
			previous = request
			continue
		}
		return s.deliveryDisposition(err)
	}
	return nil
}

func deliveryRetryDelay(attempt int, err error) time.Duration {
	delay := 500 * time.Millisecond
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	delay = min(delay, 30*time.Second)
	var status *statusError
	if errors.As(err, &status) && status.retryAfter > 0 {
		delay = max(500*time.Millisecond, min(status.retryAfter, 30*time.Second))
	}
	return delay + time.Duration(mathrand.Int64N(int64(500*time.Millisecond)))
}

func waitDeliveryRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *sessionRuntime) deliveryLoop(batch []deliveryUpdate) {
	defer s.loops.Done()
	attempt := 0
	for len(batch) > 0 {
		err := s.sendDelivery(s.ctx, batch)
		s.deliveryMu.Lock()
		if err != nil {
			s.deliveryPending = append(batch, s.deliveryPending...)
		} else {
			s.releaseDeliveryBackpressureLocked()
		}
		if s.ctx.Err() != nil {
			s.deliveryRunning = false
			s.deliveryMu.Unlock()
			return
		}
		s.deliveryMu.Unlock()
		if err != nil {
			attempt++
			if !waitDeliveryRetry(s.ctx, deliveryRetryDelay(attempt, err)) {
				s.deliveryMu.Lock()
				s.deliveryRunning = false
				s.deliveryMu.Unlock()
				return
			}
		} else {
			attempt = 0
		}
		s.deliveryMu.Lock()
		batch = s.takeDeliveryBatchLocked()
		if len(batch) == 0 {
			s.deliveryRunning = false
		}
		s.deliveryMu.Unlock()
	}
}

// Called after query loops have joined, while opMu is held. Graceful stop
// drains admitted obligations; quarantine cancellation interrupts the drain.
func (s *sessionRuntime) flushDeliveryLocked(ctx context.Context) error {
	attempt := 0
	for {
		s.deliveryMu.Lock()
		s.releaseDeliveryBackpressureLocked()
		batch := s.takeDeliveryBatchLocked()
		s.deliveryMu.Unlock()
		if len(batch) == 0 {
			return nil
		}
		err := s.sendDeliveryLocked(ctx, batch)
		if err == nil {
			attempt = 0
			continue
		}
		s.deliveryMu.Lock()
		s.deliveryPending = append(batch, s.deliveryPending...)
		s.deliveryMu.Unlock()
		attempt++
		if !waitDeliveryRetry(ctx, deliveryRetryDelay(attempt, err)) {
			return ctx.Err()
		}
	}
}
