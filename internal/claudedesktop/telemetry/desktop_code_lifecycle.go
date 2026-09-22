package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSessionResumeBringHomeTiming = "session_resume_bring_home_timing"
	FactSessionIdlePauseDeclined     = "session_idle_pause_declined"
	FactSessionIdleTimeoutCancelled  = "session_idle_timeout_cancelled"
	FactSessionIdleWarmStart         = "session_idle_warm_start"
	FactSessionIdleWarmComplete      = "session_idle_warm_complete"
	FactSessionPauseBlockedByRC      = "session_pause_blocked_by_rc"
	FactTranscriptLeasePass          = "transcript_lease_pass"
)

// DesktopCodeLifecycleFacts contains only measured durable-session lifecycle
// facts. SessionID is the Desktop record ID; SDKSessionID is the owned CLI
// transcript identity.
type DesktopCodeLifecycleFacts struct {
	SessionID                string
	SDKSessionID             string
	ResolveDuration          time.Duration
	ScanDuration             time.Duration
	TotalDuration            time.Duration
	WarmDuration             time.Duration
	SecondsSinceLastActivity *int64
	ConsecutiveDeclines      int
	PrefixParity             string
	HadScheduledTask         bool
}

// DesktopTranscriptLeasePass is a content-free verification result over the
// protected transcript candidates owned by one account runtime.
type DesktopTranscriptLeasePass struct {
	Candidates      int
	Renewed         int
	Fresh           int
	Missing         int
	Errors          int
	Skipped         string
	RetentionDays   int
	RetentionSource string
}

// DesktopSessionActivity is a point-in-time view of the real renderer request
// state. Missing activity remains distinguishable from a measured zero.
type DesktopSessionActivity struct {
	Found           bool
	PendingRequests int
	LastActivityAt  time.Time
}

func desktopDurationMS(value time.Duration) float64 {
	if value < 0 {
		value = 0
	}
	return float64(value) / float64(time.Millisecond)
}

func (m *Manager) recordDesktopCodeLifecycle(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts, fact string, metadata map[string]any) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	worker, err := m.workerFor(auth)
	if err != nil {
		return err
	}
	err = worker.enqueueProjected(context.Background(), fact, facts.SDKSessionID, "", metadata)
	if err != nil {
		worker.recordQueueFailure(err)
	}
	return err
}

func (m *Manager) RecordDesktopResumeBringHomeTiming(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	metadata := map[string]any{
		"session_id":     facts.SessionID,
		"cli_session_id": facts.SDKSessionID,
		"outcome":        "home",
		"resolve_ms":     desktopDurationMS(facts.ResolveDuration),
		"scan_ms":        desktopDurationMS(facts.ScanDuration),
		"total_ms":       desktopDurationMS(facts.TotalDuration),
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionResumeBringHomeTiming, metadata)
}

func (m *Manager) RecordDesktopSessionIdleWarmStart(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionIdleWarmStart, map[string]any{
		"session_id": facts.SessionID, "is_ssh": false, "backend_kind": "local",
	})
}

func (m *Manager) RecordDesktopSessionIdleWarmComplete(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	prefixParity := strings.TrimSpace(facts.PrefixParity)
	if prefixParity == "" {
		return fmt.Errorf("desktop session warm completion requires prefix parity")
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionIdleWarmComplete, map[string]any{
		"session_id": facts.SessionID, "warm_duration_ms": desktopDurationMS(facts.WarmDuration),
		"is_ssh": false, "backend_kind": "local", "prefix_parity": prefixParity,
		"had_scheduled_task": facts.HadScheduledTask,
	})
}

func (m *Manager) RecordDesktopSessionPauseBlockedByRemoteControl(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	seconds, err := lifecycleIdleSeconds(facts.SecondsSinceLastActivity)
	if err != nil {
		return err
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionPauseBlockedByRC, map[string]any{
		"session_id": facts.SessionID, "auto_enabled": true, "auto_source": "gb_default",
		"seconds_since_last_activity": seconds,
	})
}

func (m *Manager) RecordDesktopSessionIdlePauseDeclined(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	if facts.ConsecutiveDeclines < 1 {
		return fmt.Errorf("desktop session idle decline count must be positive")
	}
	seconds, err := lifecycleIdleSeconds(facts.SecondsSinceLastActivity)
	if err != nil {
		return err
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionIdlePauseDeclined, map[string]any{
		"session_id": facts.SessionID, "consecutive_declines": facts.ConsecutiveDeclines,
		"seconds_since_last_activity": seconds,
	})
}

func (m *Manager) RecordDesktopSessionIdleTimeoutStarted(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	seconds, err := lifecycleIdleSeconds(facts.SecondsSinceLastActivity)
	if err != nil {
		return err
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionIdleTimeout, map[string]any{
		"session_id": facts.SessionID, "timeout_ms": int64(900000), "configured_timeout_ms": int64(900000),
		"seconds_since_last_activity": seconds,
	})
}

func (m *Manager) RecordDesktopSessionIdleTimeoutCancelled(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts) error {
	var seconds any
	if facts.SecondsSinceLastActivity != nil {
		seconds = *facts.SecondsSinceLastActivity
	}
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionIdleTimeoutCancelled, map[string]any{
		"session_id": facts.SessionID, "seconds_since_last_activity": seconds,
	})
}

func (m *Manager) RecordDesktopSessionVisibility(auth *cliproxyauth.Auth, facts DesktopCodeLifecycleFacts, visible, active bool) error {
	return m.recordDesktopCodeLifecycle(auth, facts, FactSessionVisibility, map[string]any{
		"session_id": facts.SessionID, "is_visible": visible, "has_active_query": active,
		"is_ssh": false, "backend_kind": "local",
	})
}

func lifecycleIdleSeconds(value *int64) (int64, error) {
	if value == nil || *value < 0 {
		return 0, fmt.Errorf("desktop session lifecycle requires measured last activity")
	}
	return *value, nil
}

func (m *Manager) RecordDesktopTranscriptLeasePass(auth *cliproxyauth.Auth, pass DesktopTranscriptLeasePass) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	worker, err := m.workerFor(auth)
	if err != nil {
		return err
	}
	metadata := map[string]any{
		"candidates": pass.Candidates, "renewed": pass.Renewed, "fresh": pass.Fresh,
		"missing": pass.Missing, "errors": pass.Errors,
	}
	if skipped := strings.TrimSpace(pass.Skipped); skipped != "" {
		metadata["skipped"] = skipped
	} else {
		metadata["retention_days"] = pass.RetentionDays
		metadata["retention_source"] = strings.TrimSpace(pass.RetentionSource)
	}
	err = worker.enqueueProjected(context.Background(), FactTranscriptLeasePass, "", "", metadata)
	if err != nil {
		worker.recordQueueFailure(err)
	}
	return err
}

// DesktopActivity returns the real request state for a durable session. It
// also accepts the SDK identity for older direct observers that have no record
// ID, but never manufactures a last-activity timestamp.
func (m *Manager) DesktopActivity(auth *cliproxyauth.Auth, sessionID, sdkSessionID string) (DesktopSessionActivity, error) {
	if m == nil || !m.Enabled() {
		return DesktopSessionActivity{}, nil
	}
	worker, err := m.workerFor(auth)
	if err != nil {
		return DesktopSessionActivity{}, err
	}
	keys := []string{strings.TrimSpace(sessionID), strings.TrimSpace(sdkSessionID)}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	for _, value := range keys {
		if value == "" {
			continue
		}
		state := m.sessions[rendererSessionKey(worker, value)]
		if state == nil || state.queryClosed {
			continue
		}
		return DesktopSessionActivity{Found: true, PendingRequests: state.pendingRequests, LastActivityAt: state.lastActivityAt}, nil
	}
	return DesktopSessionActivity{}, nil
}
