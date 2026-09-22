package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
)

var errWorkerRestorationStale = errors.New("Claude Desktop worker restoration epoch changed during initialization")

// WorkerRestoration is a private snapshot of this query's successful worker
// read. It never crosses the management API or enters the record catalog.
// Metadata is retained even when no current consumer uses a particular field.
type WorkerRestoration struct {
	epoch              int64
	external, internal json.RawMessage
	applied            bool
	readFailed         bool
	reader             *workerReadClient
	readLifetime       context.Context
	abort              context.CancelFunc
	hydrationFailure   func(bool)
}

func (*WorkerRestoration) MarshalJSON() ([]byte, error) {
	return nil, errors.New("Claude Desktop worker restoration is private")
}

func (r *WorkerRestoration) WorkerEpoch() int64 { return r.epoch }
func (r *WorkerRestoration) ReadFailed() bool   { return r.readFailed }

// RecordHydrationFailure remains scoped to this restoration capability. A
// retained old callback cannot publish a successor query's health.
func (r *WorkerRestoration) RecordHydrationFailure(failed bool) {
	if r != nil && r.readLifetime != nil && r.readLifetime.Err() == nil && r.hydrationFailure != nil {
		r.hydrationFailure(failed)
	}
}
func (r *WorkerRestoration) ExternalMetadata() json.RawMessage {
	return append(json.RawMessage(nil), r.external...)
}
func (r *WorkerRestoration) InternalMetadata() json.RawMessage {
	return append(json.RawMessage(nil), r.internal...)
}

type workerReadFlight struct {
	epoch    int64
	done     chan struct{}
	cancel   context.CancelFunc
	response workerReadResponse
	err      error
}

func (f *workerReadFlight) conflict() error {
	select {
	case <-f.done:
		var conflict *WorkerEpochConflict
		if errors.As(f.err, &conflict) {
			return f.err
		}
	default:
	}
	return nil
}

// This is an ordering bound, not a network deadline: an outstanding GET keeps
// running while registration proceeds, and restoration still joins that GET.
func waitWorkerReadOrdering(ctx context.Context, done <-chan struct{}) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	case <-timer.C:
	}
	return nil
}

func (s *sessionRuntime) beginWorkerRestorationLocked(ctx context.Context) error {
	epoch, err := s.workerEpochLocked()
	if err != nil {
		return err
	}
	if s.workerRestoration != nil && s.workerRestoration.epoch == epoch && !s.bridgeExpiredLocked() {
		return nil
	}
	if s.workerRead != nil {
		if s.workerRead.epoch == epoch {
			return nil
		}
		s.workerRead.cancel()
		s.workerRead = nil
	}
	reader, err := s.workerReaderLocked(ctx, claudeprofile.ControlEndpointWorkerRead)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(s.ctx)
	flight := &workerReadFlight{epoch: epoch, done: make(chan struct{}), cancel: cancel}
	s.workerRead = flight
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		defer cancel()
		flight.response, flight.err = reader.get(lifetime, "", 10, false)
		var conflict *WorkerEpochConflict
		if errors.As(flight.err, &conflict) {
			s.epochSuperseded.Store(true)
		}
		// Publish the typed result before cancelling the query. A waiter must
		// not mistake an epoch conflict for caller cancellation.
		close(flight.done)
		if errors.As(flight.err, &conflict) {
			s.stopForEpochConflict()
		}
	}()
	return nil
}

func (s *sessionRuntime) readWorkerRestorationLocked(ctx context.Context) error {
	if err := s.beginWorkerRestorationLocked(ctx); err != nil {
		return err
	}
	flight := s.workerRead
	if flight == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		if conflict := flight.conflict(); conflict != nil {
			s.stopForEpochConflict()
			return conflict
		}
		return ctx.Err()
	case <-flight.done:
	}
	var conflict *WorkerEpochConflict
	if errors.As(flight.err, &conflict) {
		s.stopForEpochConflict()
		return flight.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	epoch, err := s.workerEpochLocked()
	if err != nil {
		return err
	}
	if epoch != flight.epoch {
		return errWorkerRestorationStale
	}
	response := flight.response.body
	readFailed := flight.err != nil || strings.TrimSpace(string(response)) == "null"
	metadata := func(key string) json.RawMessage {
		if readFailed {
			return json.RawMessage("null")
		}
		value := gjson.GetBytes(response, "worker."+key)
		if !value.Exists() || value.Type == gjson.Null {
			return json.RawMessage("null")
		}
		return append(json.RawMessage(nil), value.Raw...)
	}
	s.workerRestoration = &WorkerRestoration{epoch: epoch,
		external: metadata("external_metadata"), internal: metadata("internal_metadata"), readFailed: readFailed}
	s.workerRead = nil
	s.workerStateReadFailed.Store(readFailed)
	return nil
}

func (s *sessionRuntime) registerWorkerLocked(ctx context.Context, body any) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = errors.Join(ctx.Err(), s.ctx.Err()); err != nil {
			return err
		}
		err = s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerUpdate, body, nil)
		if err == nil {
			return nil
		}
		var status *statusError
		if errors.As(err, &status) {
			if status.code == 409 {
				s.stopForEpochConflict()
				return &WorkerEpochConflict{Reason: status.conflictReason}
			}
			if status.code == 400 || status.code == 413 || status.code == 422 {
				return err
			}
		}
		if attempt < 9 {
			wait := s.manager.workerReadWait
			if wait == nil {
				wait = workerReadPause
			}
			delay := min(500*time.Millisecond*time.Duration(1<<attempt), 30*time.Second) + time.Duration(rand.Float64()*float64(500*time.Millisecond))
			if errWait := wait(ctx, delay); errWait != nil {
				return errWait
			}
		}
	}
	return err
}

func (s *sessionRuntime) restoreWorkerLocked(ctx context.Context) error {
	epoch, err := s.workerEpochLocked()
	if err != nil {
		return err
	}
	r := s.workerRestoration
	if r == nil || r.epoch != epoch {
		return errWorkerRestorationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.applied {
		return nil
	}
	if s.inbound != nil && s.inbound.RestoreWorker != nil {
		reader, err := s.internalEventReaderLocked(ctx)
		if err != nil {
			return err
		}
		lifetime, cancel := context.WithCancel(ctx)
		defer cancel()
		// Lend a separate immutable capability. Retaining it after the callback
		// cannot observe or use a later restoration attempt's reader.
		capability := &WorkerRestoration{epoch: r.epoch, external: r.ExternalMetadata(), internal: r.InternalMetadata(),
			reader: reader, readLifetime: lifetime, abort: s.stopForEpochConflict, readFailed: r.readFailed, hydrationFailure: s.workerHydrationFailed.Store}
		if err := s.inbound.RestoreWorker(ctx, capability); err != nil {
			return err
		}
	}
	r.applied = true
	return nil
}

func (s *sessionRuntime) stopForEpochConflict() {
	s.epochSuperseded.Store(true)
	s.cancel()
}
