package controlplane

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	log "github.com/sirupsen/logrus"
)

func consumeQueryStream(ctx context.Context, body io.ReadCloser, consumers ...func(workerFrame) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var once sync.Once
	var errClose error
	closeBody := func() { once.Do(func() { errClose = body.Close() }) }
	unwatch := context.AfterFunc(ctx, closeBody)
	errRead := readWorkerFrames(bufio.NewReader(body), func(frame workerFrame) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(consumers) != 0 && consumers[0] != nil {
			return consumers[0](frame)
		}
		return nil
	})
	unwatch()
	closeBody()
	return errors.Join(errRead, errClose)
}

func (f RequestFacts) sessionKey() string {
	if f.QueryID != "" {
		return "query:" + f.QueryID
	}
	return f.LocalSessionID
}

func (m *Manager) rememberQueryTitle(facts RequestFacts, title string) {
	if facts.QueryLifetime == nil {
		m.rememberTitle(facts.sessionKey(), title)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed && facts.QueryLifetime.Err() == nil {
		m.pendingTitles[facts.sessionKey()] = title
	}
}

func (s *sessionRuntime) operationContext(ctx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	bound, cancel := context.WithCancel(ctx)
	unwatch := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	return bound, func() { unwatch(); cancel() }
}

func (s *sessionRuntime) retirementLoop() {
	defer s.manager.wg.Done()
	<-s.ctx.Done()
	if s.ready != nil {
		<-s.ready
	}
	if s.unwatch != nil {
		s.unwatch()
	}
	// All loop admissions happen under opMu and check the canceled lifetime.
	// The barrier excludes a Wait/Add race without holding a lock over Wait.
	s.opMu.Lock()
	s.opMu.Unlock()
	s.loops.Wait()
	if !s.manager.abort.Load() {
		s.opMu.Lock()
		if s.bridgeStarted && !s.epochSuperseded.Load() {
			s.retireErr = s.checkpointBridgeLocked()
		}
		s.opMu.Unlock()
		s.retireErr = errors.Join(s.retireErr, s.shutdown(s.manager.shutdownCtx))
	}
	s.manager.takeTitle(s.key)
	if s.retireErr != nil {
		log.WithError(s.retireErr).Warn("claude desktop control-plane: query shutdown was incomplete")
	}
	s.closeDelivery()
	if s.bridgeRecord != nil {
		s.bridgeRecord.Release()
	}
	close(s.done)
}

// RetireQuery waits for the exact generation's worker cleanup. Query lifetime
// cancellation starts cleanup even when no explicit management stop occurs.
// Caller cancellation stops waiting, not cleanup, and cannot revive a worker.
func (m *Manager) RetireQuery(ctx context.Context, desktopID, queryID string) error {
	if m == nil || queryID == "" {
		return nil
	}
	m.mu.Lock()
	session := m.sessions["query:"+queryID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	if desktopID == "" || session.desktopID != desktopID {
		return fmt.Errorf("Claude Desktop control-plane query ownership mismatch")
	}
	session.setShutdownReason("host_exit")
	session.cancel()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-session.done:
		return session.retireErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PrepareQueryStop records an explicit local stop before Host cancellation can
// wake cleanup. Cancellation alone does not imply a graceful shutdown reason.
func (m *Manager) PrepareQueryStop(desktopID, queryID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	session := m.sessions["query:"+queryID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	if desktopID == "" || session.desktopID != desktopID {
		return fmt.Errorf("Claude Desktop control-plane query ownership mismatch")
	}
	session.setShutdownReason("host_exit")
	return nil
}

// PrepareClose must precede cancellation of the executor's feature Hosts.
func (m *Manager) PrepareClose() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gracefulClose = true
	for _, session := range m.sessions {
		session.setShutdownReason("host_exit")
	}
}

func (s *sessionRuntime) setShutdownReason(reason string) {
	s.retireMu.Lock()
	s.shutdownReason = reason
	s.retireMu.Unlock()
}

func (s *sessionRuntime) getShutdownReason() string {
	s.retireMu.Lock()
	defer s.retireMu.Unlock()
	return s.shutdownReason
}

// Status intentionally excludes credentials, session IDs and remote error text.
type Status struct {
	Active                   int   `json:"active"`
	InitializationFailed     int   `json:"initialization_failed"`
	WorkerStateReadFailed    int   `json:"worker_state_read_failed"`
	WorkerHydrationFailed    int   `json:"worker_hydration_failed"`
	Retiring                 int   `json:"retiring"`
	Failed                   int   `json:"failed"`
	BridgeTranscriptFailed   int   `json:"bridge_transcript_failed"`
	PlaceholderPending       int64 `json:"placeholder_pending"`
	PlaceholderStorageFailed bool  `json:"placeholder_storage_failed"`
	PlaceholderSweepFailed   bool  `json:"placeholder_sweep_failed"`
	PlaceholderUsedPreserved int64 `json:"placeholder_used_preserved"`
	InboundDispatched        int64 `json:"inbound_dispatched"`
	InboundFailed            int64 `json:"inbound_failed"`
	InboundUnhandled         int64 `json:"inbound_unhandled"`
	InboundCompleted         int64 `json:"inbound_completed"`
	InboundCanceled          int64 `json:"inbound_canceled"`
	InboundExecutionFailed   int64 `json:"inbound_execution_failed"`
}

func (m *Manager) Status() Status {
	var status Status
	if m == nil {
		return status
	}
	status.PlaceholderPending = m.placeholderPending.Load()
	status.PlaceholderStorageFailed = m.placeholderStoreFailed.Load()
	status.PlaceholderSweepFailed = m.placeholderSweepFailed.Load()
	status.PlaceholderUsedPreserved = m.placeholderUsed.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.workerStateReadFailed.Load() {
			status.WorkerStateReadFailed++
		}
		if session.workerHydrationFailed.Load() {
			status.WorkerHydrationFailed++
		}
		transcriptFailed := session.bridgeTranscriptFailed.Load()
		if transcriptFailed {
			status.BridgeTranscriptFailed++
		}
		status.InboundDispatched += session.inboundDispatched.Load()
		status.InboundFailed += session.inboundFailed.Load()
		status.InboundUnhandled += session.inboundUnhandled.Load()
		status.InboundCompleted += session.inboundCompleted.Load()
		status.InboundCanceled += session.inboundCanceled.Load()
		status.InboundExecutionFailed += session.inboundExecutionFailed.Load()
		select {
		case <-session.done:
			if session.retireErr != nil {
				status.Failed++
			}
		default:
			if session.ctx.Err() == nil {
				status.Active++
				if session.initFailed.Load() {
					status.InitializationFailed++
				}
			} else {
				status.Retiring++
			}
		}
	}
	return status
}

// RecordInboundOutcome is called by the owned model consumer, never by the
// transport's received/processed acknowledgments. It retains no error text.
func (m *Manager) RecordInboundOutcome(desktopID, queryID string, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	session := m.sessions["query:"+queryID]
	m.mu.Unlock()
	if session == nil || session.desktopID != desktopID {
		return
	}
	if errors.Is(err, context.Canceled) {
		session.inboundCanceled.Add(1)
	} else if err != nil {
		session.inboundExecutionFailed.Add(1)
	} else {
		session.inboundCompleted.Add(1)
	}
}
