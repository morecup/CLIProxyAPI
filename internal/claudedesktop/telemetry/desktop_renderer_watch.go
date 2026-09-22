package telemetry

import (
	"encoding/json"
	"fmt"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	// FactRendererSessionsHeartbeatCheckBatch mirrors
	// claudeai.code.sessions.heartbeat_check_batch.
	FactRendererSessionsHeartbeatCheckBatch = "renderer_sessions_heartbeat_check_batch"
	// FactRendererSessionsWatchRetryLoop mirrors
	// claudeai.television.sessions_watch.retry_loop.
	FactRendererSessionsWatchRetryLoop = "renderer_sessions_watch_retry_loop"

	rendererSessionsWatchRetryTags = "sidebar+cowork-remote+idle-notifications"
)

func init() {
	events := map[string]string{
		FactRendererSessionsHeartbeatCheckBatch: "claudeai.code.sessions.heartbeat_check_batch",
		FactRendererSessionsWatchRetryLoop:      "claudeai.television.sessions_watch.retry_loop",
	}
	registerExecutableEvents(segmentRole, events)
	registerExecutableEvents("desktop-event-logging", events)
}

// SessionHeartbeatBatch is calculated from the owner-scoped durable registry.
// The renderer tick cannot supply or override these counters.
type SessionHeartbeatBatch struct {
	Sent, Fresh, ProbeDispatched, NoWorker, RecentlyChecked, Unknown int
}

func (b SessionHeartbeatBatch) valid() bool {
	if b.Sent < 0 || b.Fresh < 0 || b.ProbeDispatched < 0 || b.NoWorker < 0 || b.RecentlyChecked < 0 || b.Unknown < 0 {
		return false
	}
	return b.Sent == b.Fresh+b.ProbeDispatched+b.NoWorker+b.RecentlyChecked+b.Unknown
}

type rendererSessionsHeartbeatCheckBatchProperties struct {
	rendererShellBaseProperties
	Trigger         string `json:"trigger"`
	Sent            int    `json:"sent"`
	Fresh           int    `json:"fresh"`
	ProbeDispatched int    `json:"probe_dispatched"`
	NoWorker        int    `json:"no_worker"`
	RecentlyChecked int    `json:"recently_checked"`
	Unknown         int    `json:"unknown"`
}

type rendererSessionsWatchRetryLoopProperties struct {
	rendererShellBaseProperties
	HTTPStatus int    `json:"http_status"`
	Detail     string `json:"detail"`
	WatchTags  string `json:"watch_tags"`
}

func buildRendererSessionsHeartbeatCheckBatch(base rendererShellBaseProperties, batch SessionHeartbeatBatch) rendererSessionsHeartbeatCheckBatchProperties {
	return rendererSessionsHeartbeatCheckBatchProperties{
		rendererShellBaseProperties: base,
		Trigger:                     "tick", Sent: batch.Sent, Fresh: batch.Fresh, ProbeDispatched: batch.ProbeDispatched,
		NoWorker: batch.NoWorker, RecentlyChecked: batch.RecentlyChecked, Unknown: batch.Unknown,
	}
}

func (m *Manager) rendererWatchBase(auth *cliproxyauth.Auth) (rendererShellBaseProperties, *accountWorker, *accountWorker, error) {
	if m == nil || !m.Enabled() {
		return rendererShellBaseProperties{}, nil, nil, nil
	}
	if m.bundle.DesktopVersion != "1.40609.0.0" {
		return rendererShellBaseProperties{}, nil, nil, fmt.Errorf("unsupported Desktop renderer watch contract for version %q", m.bundle.DesktopVersion)
	}
	segment, desktop := m.rendererWorkers(auth)
	if segment == nil {
		return rendererShellBaseProperties{}, nil, desktop, nil
	}
	identity := m.rendererSessionIdentity(segment.binding, auth)
	return rendererShellBaseProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID,
		BillingType: identity.BillingType, Surface: rendererShellSurface,
		DeploymentMode: rendererShellDeploymentMode, AppVersion: identity.AppVersion, Version: 1,
	}, segment, desktop, nil
}

// RecordDesktopSessionHeartbeatBatch emits one v140609 tick after the server
// has read the owner-scoped registry and classified every record.
func (m *Manager) RecordDesktopSessionHeartbeatBatch(auth *cliproxyauth.Auth, batch SessionHeartbeatBatch) error {
	if !batch.valid() {
		return fmt.Errorf("invalid Desktop session heartbeat batch")
	}
	base, segment, desktop, err := m.rendererWatchBase(auth)
	if err != nil || segment == nil {
		return err
	}
	properties, err := json.Marshal(buildRendererSessionsHeartbeatCheckBatch(base, batch))
	if err != nil {
		return err
	}
	facts := LocalSessionFacts{SDKSessionID: m.appSessionID}
	return m.localSessionRendererTrack(segment, desktop, facts, FactRendererSessionsHeartbeatCheckBatch, rendererCopyPathShell, properties, true)
}

// ObserveSessionsWatchRetry is called only after the v140609 startup watch
// request returned a real 502. The detail and watcher set belong to the native
// contract rather than to a browser-supplied payload.
func (m *Manager) ObserveSessionsWatchRetry(auth *cliproxyauth.Auth, status int) error {
	if status != 502 {
		return fmt.Errorf("unsupported Desktop sessions/watch retry status %d", status)
	}
	base, segment, desktop, err := m.rendererWatchBase(auth)
	if err != nil || segment == nil {
		return err
	}
	properties, err := json.Marshal(rendererSessionsWatchRetryLoopProperties{
		rendererShellBaseProperties: base,
		HTTPStatus:                  status, Detail: fmt.Sprintf("sessions/watch %d ", status), WatchTags: rendererSessionsWatchRetryTags,
	})
	if err != nil {
		return err
	}
	facts := LocalSessionFacts{SDKSessionID: m.appSessionID}
	return m.localSessionRendererTrack(segment, desktop, facts, FactRendererSessionsWatchRetryLoop, rendererCopyPathShell, properties, true)
}
