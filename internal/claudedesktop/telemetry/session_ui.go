package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// LocalSessionFacts combines actual durable record/request facts with explicit
// browser observations. Missing native-only phases stay absent, never fake zero.
type LocalSessionFacts struct {
	SessionID, SDKSessionID, QueryID, Model, PromptID, AssistantID, RequestID, SwitchID string
	MessageLength                                                                       int
	HasRepo                                                                             *bool
	PreflightMS, QueryMS, InitMS                                                        int64
	CreatedAt, RequestStartedAt, FirstAssistantAt                                       time.Time
	IsFirstTurn                                                                         bool
	RequestCount, ToolCount                                                             int
	SessionCount, RunningCount, EntryCount                                              int
	IsRunning                                                                           bool
	Metrics                                                                             map[string]float64
	WasHidden, CacheHit                                                                 bool
	WatchTag                                                                            string
	SuppressedDurationMS                                                                int64
}

// The three properties below retain the renderer field order captured for the
// matching interaction classes. Values that the management UI does not model
// (pinning and unread state) are emitted only as their real local defaults.
type localSidebarSessionOpenedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	IsPinned         bool   `json:"is_pinned"`
	Section          string `json:"section"`
	SessionType      string `json:"session_type"`
	Via              string `json:"via"`
	IsRunning        bool   `json:"is_running"`
	IsUnread         bool   `json:"is_unread"`
	GroupBy          string `json:"group_by"`
	Sidebar          string `json:"sidebar"`
}

type localPendingTurnStuckProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	SessionType      string `json:"session_type"`
	PendingAgeMS     int64  `json:"pending_age_ms"`
	MetaIsRunning    bool   `json:"meta_is_running"`
	RendererSurface  string `json:"renderer_surface"`
}

type localTranscriptOpenSettledProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	SessionType      string `json:"session_type"`
	OpenKind         string `json:"open_kind"`
	Origin           string `json:"origin"`
	SettleReason     string `json:"settle_reason"`
	SettledMS        int64  `json:"settled_ms"`
	FirstRowsMS      int64  `json:"first_rows_ms"`
	SkeletonMS       int64  `json:"skeleton_ms"`
	CachedMessages   bool   `json:"cached_messages"`
	EntryCount       int    `json:"entry_count"`
	WasHidden        bool   `json:"was_hidden"`
	RendererSurface  string `json:"renderer_surface"`
}

// The host power-state booleans are deliberately pointers. The browser can
// measure visibility, but it cannot establish a machine suspend/resume; those
// keys therefore remain absent until an independently measured host signal is
// available.
type localSessionWatchDemandSuppressedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	WatchTags        string `json:"watch_tags"`
	Platform         string `json:"platform"`
	HostSuspended    *bool  `json:"host_suspended,omitempty"`
}

type localSessionWatchDemandRestoredProperties struct {
	AccountUUID          string `json:"account_uuid"`
	OrganizationUUID     string `json:"organization_uuid"`
	BillingType          string `json:"billing_type"`
	Surface              string `json:"surface"`
	DeploymentMode       string `json:"deployment_mode"`
	AppVersion           string `json:"app_version"`
	Version              int    `json:"version"`
	WatchTags            string `json:"watch_tags"`
	Platform             string `json:"platform"`
	SuppressedDurationMS int64  `json:"suppressed_duration_ms"`
	Trigger              string `json:"trigger"`
	HostSleptInGap       *bool  `json:"host_slept_in_gap,omitempty"`
}

type localSessionWatchContract struct {
	tags     []string
	dualFire bool
}

var v140609LocalSessionWatchContract = localSessionWatchContract{
	tags:     []string{"cowork-remote", "sidebar", "idle-notifications"},
	dualFire: true,
}

// localSessionWatchContractForBundle keeps watcher fan-out and delivery shape
// version-scoped. Desktop 1.40609.0.0 captured three dual-fired watchers,
// while later Desktop releases must be measured and added explicitly instead
// of silently inheriting this contract.
func (m *Manager) localSessionWatchContractForBundle() (localSessionWatchContract, error) {
	if m == nil || !m.Enabled() {
		return localSessionWatchContract{}, nil
	}
	if m.bundle.DesktopVersion != "1.40609.0.0" {
		return localSessionWatchContract{}, fmt.Errorf("unsupported Desktop session watch contract for version %q", m.bundle.DesktopVersion)
	}
	return v140609LocalSessionWatchContract, nil
}

func isLocalSessionWatchTag(tags []string, tag string) bool {
	for _, candidate := range tags {
		if tag == candidate {
			return true
		}
	}
	return false
}

func localSessionRequestFacts(facts LocalSessionFacts) RequestFacts {
	return RequestFacts{DesktopSessionID: facts.SessionID, SessionID: facts.SDKSessionID, QueryID: facts.QueryID, PromptID: facts.PromptID}
}

// localSessionRendererTrack projects an already ordered renderer property
// object. Segment persistence happens before a Desktop dual-fire copy, matching
// the renderer delivery contract. A Segment-only event never creates a Desktop
// event just because both workers happen to be available.
func (m *Manager) localSessionRendererTrack(segment, desktop *accountWorker, facts LocalSessionFacts, fact, path string, properties json.RawMessage, dualFire bool) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if segment == nil {
		return nil
	}
	payload, name, errProject := m.projectRendererCall(segment, rendererCallTrack, fact, properties)
	request := localSessionRequestFacts(facts)
	if err := m.enqueueAuxiliary(context.Background(), segment, fact, name, request, payload, errProject); err != nil {
		return err
	}
	if !dualFire || desktop == nil {
		return nil
	}
	copyPayload, copyName, err := m.projectDesktopRendererDualFireAt(desktop, fact, payload, path)
	if err != nil {
		desktop.recordQueueFailure(err)
		return err
	}
	err = desktop.enqueue(Envelope{Version: 1, EventUUID: uuid.NewString(), EndpointRole: m.profile.EndpointRole,
		CatalogFact: fact, CatalogEvent: copyName, OccurredAt: m.now().UTC().Format(time.RFC3339Nano),
		Binding: desktop.binding, SessionID: facts.SDKSessionID, Payload: copyPayload})
	if err != nil {
		desktop.recordQueueFailure(err)
	}
	return err
}

func (m *Manager) localSessionProperties(auth *cliproxyauth.Auth) (map[string]any, *accountWorker, *accountWorker) {
	segment, desktop := m.rendererWorkers(auth)
	if segment == nil || desktop == nil {
		return nil, segment, desktop
	}
	identity := m.rendererSessionIdentity(desktop.binding, auth)
	return map[string]any{"account_uuid": identity.AccountUUID, "organization_uuid": identity.OrganizationUUID,
		"billing_type": identity.BillingType, "surface": rendererSessionSurface, "deployment_mode": rendererSessionDeployment,
		"app_version": identity.AppVersion}, segment, desktop
}

func (m *Manager) localSessionTrack(auth *cliproxyauth.Auth, facts LocalSessionFacts, fact, path string, extra map[string]any) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	props, segment, desktop := m.localSessionProperties(auth)
	if props == nil {
		return nil
	}
	for key, value := range extra {
		props[key] = value
	}
	properties, err := json.Marshal(props)
	if err != nil {
		return err
	}
	payload, name, errProject := m.projectRendererCall(segment, rendererCallTrack, fact, properties)
	request := localSessionRequestFacts(facts)
	if err := m.enqueueAuxiliary(context.Background(), segment, fact, name, request, payload, errProject); err != nil {
		return err
	}
	copyPayload, copyName, err := m.projectDesktopRendererDualFireAt(desktop, fact, payload, path)
	if err != nil {
		desktop.recordQueueFailure(err)
		return err
	}
	err = desktop.enqueue(Envelope{Version: 1, EventUUID: uuid.NewString(), EndpointRole: m.profile.EndpointRole,
		CatalogFact: fact, CatalogEvent: copyName, OccurredAt: m.now().UTC().Format(time.RFC3339Nano),
		Binding: desktop.binding, SessionID: facts.SDKSessionID, Payload: copyPayload})
	if err != nil {
		desktop.recordQueueFailure(err)
	}
	return err
}

func (m *Manager) localSessionDesktop(auth *cliproxyauth.Auth, facts LocalSessionFacts, fact string, metadata map[string]any) error {
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

// RecordDesktopLocalCreated is called after CreateLocal durably commits a new
// record. Reconnects, resumes and a request's firstTurn flag never call it.
func (m *Manager) RecordDesktopLocalCreated(auth *cliproxyauth.Auth, facts LocalSessionFacts) error {
	props := map[string]any{"version": 2, "session_type": "local", "model": facts.Model,
		"environment_kind": "local", "is_scratch": false, "message_length": facts.MessageLength,
		"post_band_switch": false, "renderer_surface": rendererSessionSurfaceName}
	if facts.HasRepo != nil {
		props["has_repo"] = *facts.HasRepo
	}
	return m.localSessionTrack(auth, facts, FactLocalSessionCreated, rendererCopyPathShell, props)
}

func (m *Manager) RecordDesktopLocalStartTiming(auth *cliproxyauth.Auth, facts LocalSessionFacts) error {
	metadata := map[string]any{"session_id": facts.SessionID, "preflight_ms": facts.PreflightMS, "query_ms": facts.QueryMS,
		"init_ms": facts.InitMS, "total_to_init_ms": facts.PreflightMS + facts.InitMS, "is_first_turn": true, "is_ssh": false}
	if !facts.FirstAssistantAt.IsZero() && !facts.RequestStartedAt.IsZero() {
		metadata["first_assistant_ms"] = facts.FirstAssistantAt.Sub(facts.RequestStartedAt).Milliseconds()
		metadata["total_to_assistant_ms"] = facts.FirstAssistantAt.Sub(facts.CreatedAt).Milliseconds()
	}
	return m.localSessionDesktop(auth, facts, FactLocalSessionStartTiming, metadata)
}

func localMetric(facts LocalSessionFacts, key string) int64 {
	return int64(math.Round(facts.Metrics[key]))
}

// RecordDesktopSessionWatchDemand emits the renderer's fixed Code watch
// subscriptions. The view never chooses watch tags or a duration: duration is
// measured by the account runtime between accepted hidden and visible states.
func (m *Manager) RecordDesktopSessionWatchDemand(auth *cliproxyauth.Auth, kind string, facts LocalSessionFacts) error {
	if kind != "sessions_watch_demand_suppressed" && kind != "sessions_watch_demand_restored" {
		return fmt.Errorf("unsupported Desktop session watch observation %q", kind)
	}
	contract, err := m.localSessionWatchContractForBundle()
	if err != nil {
		return err
	}
	for _, tag := range contract.tags {
		facts.WatchTag = tag
		if err := m.RecordDesktopSessionUI(auth, kind, facts); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) RecordDesktopSessionUI(auth *cliproxyauth.Auth, kind string, facts LocalSessionFacts) error {
	switch kind {
	case "input_ready":
		props := map[string]any{"version": 54, "duration_ms": localMetric(facts, "duration_ms"),
			"input_ready_ms": localMetric(facts, "duration_ms"), "ready_commit_ms": localMetric(facts, "mount_ms"),
			"route_commit_ms": localMetric(facts, "route_ms"), "tab_age_ms": localMetric(facts, "tab_age_ms"),
			"after_paint_ms": localMetric(facts, "after_paint_ms"), "entry_pathname": "/new", "was_hidden": facts.WasHidden,
			"input_ready_source": "react_commit", "after_paint_end": "painted", "renderer_observation_source": "management_session_ui"}
		return m.localSessionTrack(auth, facts, FactLocalInputReady, "/new", props)
	case "first_text":
		props := map[string]any{"version": 1, "session_id": facts.SessionID, "session_type": "local", "is_new_session": facts.IsFirstTurn,
			"user_message_uuid": facts.PromptID, "cli_session_id": facts.SDKSessionID, "first_assistant_message_id": facts.AssistantID,
			"first_text_assistant_message_id": facts.AssistantID, "ttfvt_first_text_paint_ms": localMetric(facts, "paint_ms"),
			"first_text_wait_end": "painted", "first_text_path": "direct", "requests_before_first_text": facts.RequestCount,
			"tool_calls_before_first_text": facts.ToolCount, "first_text_receipt_ms": localMetric(facts, "receipt_ms"),
			"start_to_emit_ms": localMetric(facts, "after_paint_ms"), "model": facts.Model,
			"tab_age_ms": localMetric(facts, "tab_age_ms"), "open_local_session_count": facts.SessionCount,
			"renderer_observation_source": "management_session_ui"}
		if facts.ToolCount > 0 {
			props["first_text_path"] = "after_tool_use"
		}
		if facts.RequestID != "" {
			props["first_request_id"] = facts.RequestID
		}
		return m.localSessionTrack(auth, facts, FactLocalSessionFirstText, rendererCopyPathSession, props)
	case "switch_started", "switch_painted":
		metadata := map[string]any{"session_id": facts.SessionID, "switch_id": facts.SwitchID,
			"session_type": "local", "is_new_session": false, "entry_point": "management", "backend_kind": "local",
			"renderer_surface": rendererSessionSurfaceName, "renderer_observation_source": "management_session_ui"}
		if kind == "switch_started" {
			return m.localSessionDesktop(auth, facts, FactLocalSwitchInitiated, metadata)
		}
		for _, key := range []string{"route_ms", "transcript_ms", "parse_ms", "mount_ms", "after_paint_ms"} {
			if _, present := facts.Metrics[key]; present {
				metadata[key] = localMetric(facts, key)
			}
		}
		metadata["first_paint_ms"], metadata["active_session_count"], metadata["running_session_count"] = localMetric(facts, "paint_ms"), facts.SessionCount, facts.RunningCount
		metadata["cache_hit"], metadata["was_hidden"], metadata["transcript_result"] = facts.CacheHit, facts.WasHidden, "loaded"
		metadata["first_content_source"] = "local_conversation"
		return m.localSessionDesktop(auth, facts, FactLocalSwitchTiming, metadata)
	case "sidebar_session_opened":
		segment, desktop := m.rendererWorkers(auth)
		if segment == nil {
			return nil
		}
		identity := m.rendererSessionIdentity(segment.binding, auth)
		properties, err := json.Marshal(localSidebarSessionOpenedProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Version: rendererSessionVersion, IsPinned: false, Section: "recents", SessionType: rendererSessionType,
			Via: "click", IsRunning: facts.IsRunning, IsUnread: false, GroupBy: "project", Sidebar: "unified",
		})
		if err != nil {
			return err
		}
		return m.localSessionRendererTrack(segment, desktop, facts, FactLocalSidebarSessionOpen, rendererCopyPathShell, properties, true)
	case "transcript_open_settled":
		segment, desktop := m.rendererWorkers(auth)
		if segment == nil {
			return nil
		}
		identity := m.rendererSessionIdentity(segment.binding, auth)
		properties, err := json.Marshal(localTranscriptOpenSettledProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Version: rendererSessionVersion, SessionID: facts.SessionID, SessionType: rendererSessionType,
			OpenKind: "existing", Origin: "sidebar", SettleReason: "visible_rows",
			SettledMS: localMetric(facts, "settled_ms"), FirstRowsMS: localMetric(facts, "first_rows_ms"),
			SkeletonMS: localMetric(facts, "skeleton_ms"), CachedMessages: facts.CacheHit, EntryCount: facts.EntryCount,
			WasHidden: facts.WasHidden, RendererSurface: rendererSessionSurfaceName,
		})
		if err != nil {
			return err
		}
		return m.localSessionRendererTrack(segment, desktop, facts, FactLocalTranscriptSettled, rendererCopyPathSession, properties, true)
	case "pending_turn_stuck_idle":
		segment, desktop := m.rendererWorkers(auth)
		if segment == nil {
			return nil
		}
		identity := m.rendererSessionIdentity(segment.binding, auth)
		properties, err := json.Marshal(localPendingTurnStuckProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Version: rendererSessionVersion, SessionID: facts.SessionID, SessionType: rendererSessionType,
			PendingAgeMS: localMetric(facts, "pending_age_ms"), MetaIsRunning: facts.IsRunning, RendererSurface: rendererSessionSurfaceName,
		})
		if err != nil {
			return err
		}
		return m.localSessionRendererTrack(segment, desktop, facts, FactLocalPendingTurnStuck, rendererCopyPathSession, properties, true)
	case "sessions_watch_demand_suppressed":
		contract, err := m.localSessionWatchContractForBundle()
		if err != nil {
			return err
		}
		if len(contract.tags) == 0 {
			return nil
		}
		if !isLocalSessionWatchTag(contract.tags, facts.WatchTag) {
			return fmt.Errorf("unsupported Desktop session watch tag %q", facts.WatchTag)
		}
		segment, desktop := m.rendererWorkers(auth)
		if segment == nil {
			return nil
		}
		identity := m.rendererSessionIdentity(segment.binding, auth)
		properties, err := json.Marshal(localSessionWatchDemandSuppressedProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Version: rendererSessionVersion, WatchTags: facts.WatchTag, Platform: "desktop",
		})
		if err != nil {
			return err
		}
		return m.localSessionRendererTrack(segment, desktop, facts, FactLocalWatchDemandSuppressed, rendererCopyPathShell, properties, contract.dualFire)
	case "sessions_watch_demand_restored":
		contract, err := m.localSessionWatchContractForBundle()
		if err != nil {
			return err
		}
		if len(contract.tags) == 0 {
			return nil
		}
		if !isLocalSessionWatchTag(contract.tags, facts.WatchTag) || facts.SuppressedDurationMS < 0 {
			return fmt.Errorf("invalid Desktop session watch restoration")
		}
		segment, desktop := m.rendererWorkers(auth)
		if segment == nil {
			return nil
		}
		identity := m.rendererSessionIdentity(segment.binding, auth)
		properties, err := json.Marshal(localSessionWatchDemandRestoredProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
			Version: rendererSessionVersion, WatchTags: facts.WatchTag, Platform: "desktop",
			SuppressedDurationMS: facts.SuppressedDurationMS, Trigger: "focus",
		})
		if err != nil {
			return err
		}
		return m.localSessionRendererTrack(segment, desktop, facts, FactLocalWatchDemandRestored, rendererCopyPathShell, properties, contract.dualFire)
	}
	return nil
}
