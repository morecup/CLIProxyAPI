package telemetry

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	segmentRole            = "segment"
	datadogLogsRole        = "datadog-logs"
	datadogLogsBrowserRole = "datadog-logs-browser"
	datadogRUMRole         = "datadog-rum"
	sentryRole             = "sentry"
)

const (
	FactDesktopSessionListLoaded   = "desktop_session_list_loaded"
	FactRendererSessionStopped     = "renderer_session_stopped"
	FactLocalSessionCreated        = "local_session_created"
	FactLocalSessionStartTiming    = "local_session_start_timing"
	FactLocalSessionFirstText      = "local_session_first_text"
	FactLocalInputReady            = "local_input_ready"
	FactLocalSwitchInitiated       = "local_switch_initiated"
	FactLocalSwitchTiming          = "local_switch_timing"
	FactLocalSidebarSessionOpen    = "local_sidebar_session_opened"
	FactLocalTranscriptSettled     = "local_transcript_open_settled"
	FactLocalPendingTurnStuck      = "local_pending_turn_stuck_idle"
	FactLocalWatchDemandSuppressed = "local_sessions_watch_demand_suppressed"
	FactLocalWatchDemandRestored   = "local_sessions_watch_demand_restored"
)

func init() {
	registerExecutableEvents("desktop-event-logging", map[string]string{
		FactDesktopSessionListLoaded:   "desktop_ccd_session_list_loaded",
		FactRendererSessionStopped:     "claudeai.code.session.stopped",
		FactLocalSessionCreated:        "claudeai.desktop.code.landing.session_created",
		FactLocalSessionStartTiming:    "desktop_ccd_session_start_timing",
		FactLocalSessionFirstText:      "claudeai.code.session.first_text",
		FactLocalInputReady:            "claudeai.page.new_chat_input_ready",
		FactLocalSwitchInitiated:       "desktop_ccd_session_switch_initiated",
		FactLocalSwitchTiming:          "desktop_ccd_session_switch_timing",
		FactLocalSidebarSessionOpen:    "claudeai.code.sidebar.session_opened",
		FactLocalTranscriptSettled:     "claudeai.epitaxy.transcript.open_settled",
		FactLocalPendingTurnStuck:      "claudeai.epitaxy.session.pending_turn_stuck_idle",
		FactLocalWatchDemandSuppressed: "claudeai.television.sessions_watch.demand_suppressed",
		FactLocalWatchDemandRestored:   "claudeai.television.sessions_watch.demand_restored",
	})
	registerExecutableEvents(segmentRole, map[string]string{
		FactRendererSessionStopped:     "claudeai.code.session.stopped",
		FactLocalSessionCreated:        "claudeai.desktop.code.landing.session_created",
		FactLocalSessionFirstText:      "claudeai.code.session.first_text",
		FactLocalInputReady:            "claudeai.page.new_chat_input_ready",
		FactLocalSidebarSessionOpen:    "claudeai.code.sidebar.session_opened",
		FactLocalTranscriptSettled:     "claudeai.epitaxy.transcript.open_settled",
		FactLocalPendingTurnStuck:      "claudeai.epitaxy.session.pending_turn_stuck_idle",
		FactLocalWatchDemandSuppressed: "claudeai.television.sessions_watch.demand_suppressed",
		FactLocalWatchDemandRestored:   "claudeai.television.sessions_watch.demand_restored",
	})
}

// RecordDesktopSessionListLoaded observes a successful, owner-scoped management
// list. It does not provision an account, infer renderer boot, or count another
// owner's records. Cache state and elapsed time come from the registry read.
func (m *Manager) RecordDesktopSessionListLoaded(authID string, elapsed time.Duration, sessionCount int, cached bool) {
	if m == nil || !m.Enabled() || authID == "" || elapsed < 0 || sessionCount < 0 {
		return
	}
	m.mu.RLock()
	var worker *accountWorker
	for _, candidate := range m.workers {
		if candidate.binding.AuthID == authID && candidate.profile.endpointRole == m.profile.EndpointRole {
			worker = candidate
			break
		}
	}
	m.mu.RUnlock()
	if worker == nil {
		return
	}
	metadata := map[string]any{
		"attempt_id": uuid.NewString(), "list_source": "local", "entry_point": "management",
		"attempt_index": 1, "duration_ms": elapsed.Milliseconds(), "session_count": sessionCount,
		"skipped_count": 0, "cached": cached, "store_state": "loaded",
	}
	if err := worker.enqueueProjected(context.Background(), FactDesktopSessionListLoaded, "", "", metadata); err != nil {
		worker.recordQueueFailure(err)
		log.WithError(err).Warn("claude desktop telemetry: session list observation was not persisted")
	}
}

type rendererSessionStoppedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	IsLocal          bool   `json:"is_local"`
	Discarded        bool   `json:"discarded"`
	RendererSurface  string `json:"renderer_surface"`
	IsMobileWeb      bool   `json:"is_mobile_web"`
}

// emitRetainedRendererSessionStopped runs only after an explicit stop commits.
// The registry keeps the local record, so discarded is false. Backgrounding,
// disconnect, app exit, failed preparation and repeated stops do not call it.
func (m *Manager) emitRetainedRendererSessionStopped(worker *accountWorker, facts RequestFacts) error {
	auth := worker.authSnapshot()
	segmentWorker, desktopWorker := m.rendererWorkers(auth)
	if segmentWorker == nil || desktopWorker == nil {
		return nil
	}
	if m.auxiliaryProfiles[segmentRole].Events[FactRendererSessionStopped].EventName == "" || m.profile.Events[FactRendererSessionStopped].EventName == "" {
		return nil
	}
	identity := m.rendererSessionIdentity(worker.binding, auth)
	properties, err := json.Marshal(rendererSessionStoppedProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID,
		BillingType: identity.BillingType, Surface: rendererSessionSurface,
		DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Version: 1, SessionID: facts.DesktopSessionID, IsLocal: true, Discarded: false,
		RendererSurface: rendererSessionSurfaceName, IsMobileWeb: false,
	})
	if err != nil {
		return err
	}
	payload, name, errProject := m.projectRendererCall(segmentWorker, rendererCallTrack, FactRendererSessionStopped, properties)
	if err := m.enqueueAuxiliary(context.Background(), segmentWorker, FactRendererSessionStopped, name, facts, payload, errProject); err != nil {
		return err
	}
	// A true dual-fire copy requires the Segment item to have been persisted.
	copyPayload, copyName, errCopy := m.projectDesktopRendererDualFireAt(desktopWorker, FactRendererSessionStopped, payload, rendererCopyPathSession)
	if errCopy != nil {
		desktopWorker.recordQueueFailure(errCopy)
		return errCopy
	}
	if err := desktopWorker.enqueue(Envelope{
		Version: 1, EventUUID: uuid.NewString(), EndpointRole: m.profile.EndpointRole,
		CatalogFact: FactRendererSessionStopped, CatalogEvent: copyName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: desktopWorker.binding,
		SessionID: facts.SessionID, ClientRequestID: facts.ClientRequestID, Payload: copyPayload,
	}); err != nil {
		desktopWorker.recordQueueFailure(err)
		return err
	}
	return nil
}

type encodedDeliveryBatch struct {
	body            []byte
	contentEncoding string
	query           url.Values
}

func (m *Manager) emitAuxiliaryRequestStarted(ctx context.Context, span *RequestSpan) {
	if span == nil || span.facts.Role != claudeprofile.RoleMain {
		return
	}
	if span.facts.Prompt != nil && !span.facts.Prompt.Identity().StartsPrompt {
		return
	}
	if worker := span.auxiliaryWorkers[segmentRole]; worker != nil {
		payload, eventName, errProject := m.projectSegment(worker, FactRequestStarted, span, 0)
		m.enqueueAuxiliary(ctx, worker, FactRequestStarted, eventName, span.facts, payload, errProject)
		m.emitDesktopRendererDualFire(ctx, span, FactRendererMessageSubmitted, payload, errProject)
	}
	if worker := span.auxiliaryWorkers[datadogRUMRole]; worker != nil {
		payload, eventName, errProject := m.projectRUM(worker, FactRequestStarted, span.facts, 0, "", "")
		m.enqueueAuxiliary(ctx, worker, FactRequestStarted, eventName, span.facts, payload, errProject)
	}
}

func (m *Manager) observeAuxiliaryFirstByte(span *RequestSpan) {
	if span == nil || span.facts.Role != claudeprofile.RoleMain || span.firstByte.IsZero() {
		return
	}
	duration := span.firstByte.Sub(span.facts.StartedAt)
	if duration < 0 {
		duration = 0
	}
	if worker := span.auxiliaryWorkers[segmentRole]; worker != nil {
		payload, eventName, errProject := m.projectSegment(worker, FactFirstByte, span, duration)
		m.enqueueAuxiliary(context.Background(), worker, FactFirstByte, eventName, span.facts, payload, errProject)
		m.emitDesktopRendererDualFire(context.Background(), span, FactRendererSessionTTFT, payload, errProject)
	}
	if worker := span.auxiliaryWorkers[datadogRUMRole]; worker != nil {
		payload, eventName, errProject := m.projectRUM(worker, FactFirstByte, span.facts, duration, "", "")
		m.enqueueAuxiliary(context.Background(), worker, FactFirstByte, eventName, span.facts, payload, errProject)
	}
}

func (m *Manager) finishAuxiliaryRequest(ctx context.Context, span *RequestSpan, category, errorClass string, duration time.Duration) {
	if span == nil || category == "" && errorClass == "" {
		return
	}
	if worker := span.auxiliaryWorkers[datadogRUMRole]; worker != nil && span.facts.Role == claudeprofile.RoleMain {
		payload, eventName, errProject := m.projectRUM(worker, FactRequestFailed, span.facts, duration, category, errorClass)
		m.enqueueAuxiliary(ctx, worker, FactRequestFailed, eventName, span.facts, payload, errProject)
	}
	if worker := span.auxiliaryWorkers[sentryRole]; worker != nil {
		payload, eventName, errProject := m.projectSentryError(worker, span.facts, duration, category, errorClass)
		m.enqueueAuxiliary(ctx, worker, FactRequestFailed, eventName, span.facts, payload, errProject)
	}
}

func (m *Manager) enqueueAuxiliary(ctx context.Context, worker *accountWorker, fact, eventName string, facts RequestFacts, payload []byte, errProject error) error {
	_ = ctx
	if worker == nil {
		return nil
	}
	if errProject != nil {
		worker.recordQueueFailure(errProject)
		log.WithError(errProject).WithField("endpoint_role", worker.profile.endpointRole).Warn("claude desktop auxiliary telemetry: event projection failed")
		return errProject
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version:         1,
		EventUUID:       uuid.New().String(),
		EndpointRole:    worker.profile.endpointRole,
		CatalogFact:     fact,
		CatalogEvent:    eventName,
		OccurredAt:      m.now().UTC().Format(time.RFC3339Nano),
		Binding:         worker.binding,
		SessionID:       facts.SessionID,
		ClientRequestID: facts.ClientRequestID,
		Payload:         payload,
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).WithField("endpoint_role", worker.profile.endpointRole).Warn("claude desktop auxiliary telemetry: event was not persisted")
		return errEnqueue
	}
	return nil
}

func (m *Manager) projectSegment(worker *accountWorker, fact string, span *RequestSpan, duration time.Duration) ([]byte, string, error) {
	if span == nil {
		return nil, "", fmt.Errorf("Segment request span is unavailable")
	}
	facts := span.facts
	profile, ok := m.auxiliaryProfiles[segmentRole]
	if !ok {
		return nil, "", fmt.Errorf("Segment telemetry profile is unavailable")
	}
	event, ok := profile.Events[fact]
	if !ok || strings.TrimSpace(event.EventName) == "" {
		return nil, "", fmt.Errorf("Segment telemetry fact %q is not mapped", fact)
	}
	auth := worker.authSnapshot()
	base := segmentRendererBaseProperties{
		AccountUUID:      worker.binding.AccountUUID,
		OrganizationUUID: worker.binding.OrganizationUUID,
		BillingType:      subscriptionType(auth),
		Surface:          "claude-ai",
		DeploymentMode:   "1p",
		AppVersion:       strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		Version:          1,
		SessionID:        rendererRecordID(facts),
	}
	var properties any
	if fact == FactRequestStarted {
		properties = segmentMessageSubmittedProperties{
			segmentRendererBaseProperties: base,
			MessageLength:                 span.inputTextCharLength,
			ImageCount:                    span.imageBlockCount,
			IsLocal:                       true,
			PostBandSwitch:                false,
			TranscriptMode:                "local",
			SessionOrigin:                 "desktop",
			IsAgentOwned:                  false,
			ProjectEnabled:                true,
			RendererSurface:               "epitaxy",
		}
	} else {
		properties = segmentSessionTTFTProperties{
			segmentRendererBaseProperties: base,
			TTFTMS:                        duration.Milliseconds(),
			IsNewSession:                  false,
			SessionType:                   "local",
			FetchToFirstByteMS:            duration.Milliseconds(),
			RequestingToFirstByteMS:       duration.Milliseconds(),
			UserMessageUUID:               facts.PromptID,
			TTFTFirstFrameMS:              duration.Milliseconds(),
			TTFTFirstPaintMS:              duration.Milliseconds(),
			FirstPaintKind:                "text",
			FirstFrameKind:                "content",
			FirstContentType:              "text",
			CLISessionID:                  facts.SessionID,
			RendererSurface:               "epitaxy",
		}
	}
	encodedProperties, errProperties := json.Marshal(properties)
	if errProperties != nil {
		return nil, "", errProperties
	}
	encoded, errMarshal := json.Marshal(segmentCallItem{
		Timestamp:    m.now().UTC().Format(time.RFC3339Nano),
		Integrations: segmentIntegrations(),
		Event:        event.EventName,
		Type:         "track",
		Properties:   encodedProperties,
		Context:      m.segmentContext(auth, worker.binding),
		MessageID:    "ajs-next-" + uuid.New().String(),
		UserID:       worker.binding.AccountUUID,
		AnonymousID:  worker.binding.RuntimeUUID,
		Metadata:     segmentMetadata{Bundled: "[]"},
	})
	return encoded, event.EventName, errMarshal
}

// segmentRendererBaseProperties is the captured leading property order shared
// by the renderer's Segment tracks: account_uuid, organization_uuid,
// billing_type, surface, deployment_mode, app_version, version, session_id.
type segmentRendererBaseProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
}

// segmentMessageSubmittedProperties follows the captured
// claudeai.code.message.submitted property order for the keys the gateway
// derives (is_mobile_web is not emitted: no pinned derivation yet).
type segmentMessageSubmittedProperties struct {
	segmentRendererBaseProperties
	MessageLength   int    `json:"message_length"`
	ImageCount      int    `json:"image_count"`
	IsLocal         bool   `json:"is_local"`
	PostBandSwitch  bool   `json:"post_band_switch"`
	TranscriptMode  string `json:"transcript_mode"`
	SessionOrigin   string `json:"session_origin"`
	IsAgentOwned    bool   `json:"is_agent_owned"`
	ProjectEnabled  bool   `json:"project_enabled"`
	RendererSurface string `json:"renderer_surface"`
}

// segmentSessionTTFTProperties follows the captured claudeai.code.session.ttft
// property order for the keys the gateway derives; renderer_surface is the
// known pre-existing deviation (absent from the captured ttft schema) and is
// kept last so it never displaces a captured key.
type segmentSessionTTFTProperties struct {
	segmentRendererBaseProperties
	TTFTMS                  int64  `json:"ttft_ms"`
	IsNewSession            bool   `json:"is_new_session"`
	SessionType             string `json:"session_type"`
	FetchToFirstByteMS      int64  `json:"fetch_to_first_byte_ms"`
	RequestingToFirstByteMS int64  `json:"requesting_to_first_byte_ms"`
	UserMessageUUID         string `json:"user_message_uuid"`
	TTFTFirstFrameMS        int64  `json:"ttft_first_frame_ms"`
	TTFTFirstPaintMS        int64  `json:"ttft_first_paint_ms"`
	FirstPaintKind          string `json:"first_paint_kind"`
	FirstFrameKind          string `json:"first_frame_kind"`
	FirstContentType        string `json:"first_content_type"`
	CLISessionID            string `json:"cli_session_id"`
	RendererSurface         string `json:"renderer_surface"`
}

func (m *Manager) ensureSegmentIdentified(worker *accountWorker) error {
	if worker == nil || !m.beginActivationEvent(segmentRole) {
		return nil
	}
	profile, ok := m.auxiliaryProfiles[segmentRole]
	event, mapped := profile.Events[FactRuntimeStarted]
	if !ok || !mapped || strings.TrimSpace(event.EventName) != "identify" {
		m.clearActivationEvent(segmentRole)
		return fmt.Errorf("Segment runtime identify event is not mapped")
	}
	auth := worker.authSnapshot()
	contextFields := m.segmentContext(auth, worker.binding)
	traits, _ := contextFields["traits"].(map[string]any)
	payload := map[string]any{
		"timestamp":    m.now().UTC().Format(time.RFC3339Nano),
		"integrations": segmentIntegrations(),
		"type":         event.EventName,
		"traits":       traits,
		"context":      contextFields,
		"messageId":    "ajs-next-" + uuid.New().String(),
		"userId":       worker.binding.AccountUUID,
		"anonymousId":  worker.binding.RuntimeUUID,
		"_metadata": map[string]any{
			"bundled": "[]", "unbundled": nil, "bundledIds": nil,
		},
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		m.clearActivationEvent(segmentRole)
		return errMarshal
	}
	eventID := uuid.New().String()
	if errEnqueue := worker.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: segmentRole, CatalogFact: FactRuntimeStarted, CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: m.appSessionID,
		ClientRequestID: eventID, Payload: encoded,
	}); errEnqueue != nil {
		m.clearActivationEvent(segmentRole)
		return errEnqueue
	}
	m.emitDesktopRendererIdentify(worker, encoded)
	m.runRendererActivationHooks(context.Background(), worker)
	select {
	case worker.wake <- struct{}{}:
	default:
	}
	return nil
}

func segmentIntegrations() map[string]bool {
	return map[string]bool{
		"All": true, "Segment.io": true, "Actions Amplitude": false, "Amplitude (Actions)": false,
		"Webhook": false, "Webhooks (Actions)": false, "Iterable": false, "Iterable (Actions)": false,
	}
}

func (m *Manager) segmentContext(auth *cliproxyauth.Auth, binding Binding) map[string]any {
	email := authMetadataString(auth, "email")
	subscription := subscriptionType(auth)
	return map[string]any{
		"traits": map[string]any{
			"deployment_mode": "1p", "app_version": strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
			"country": authMetadataStringDefault(auth, "country", "unknown"), "email": email,
			"is_personal_email":  authMetadataStringDefault(auth, "is_personal_email", "unknown"),
			"account_created_at": authMetadataInt64(auth, "account_created_at"),
			"account_uuid":       binding.AccountUUID, "organization_uuid": binding.OrganizationUUID,
			"billing_type": subscription, "org_type": authMetadataStringDefault(auth, "org_type", "unknown"),
			"subscription_level": authMetadataStringDefault(auth, "subscription_level", subscription),
			"subscription_plan":  authMetadataStringDefault(auth, "subscription_plan", subscription),
		},
		"page": map[string]any{"path": "/epitaxy/:redacted", "referrer": "", "search": "", "url": "https://claude.ai/epitaxy/:redacted"},
		"userAgentData": map[string]any{
			"brands": []map[string]string{{"brand": "Not/A)Brand", "version": "99"}, {"brand": "Chromium", "version": "148"}},
			"mobile": false, "platform": "Windows",
		},
		"locale": "en-US", "library": map[string]string{"name": "analytics.js", "version": "npm:next-1.69.0"},
		"timezone": "America/Los_Angeles", "ip": "0.0.0.0",
		"consent": map[string]any{"categoryPreferences": map[string]bool{"marketing": true, "analytics": true, "necessary": true}},
	}
}

func (m *Manager) projectRUM(worker *accountWorker, fact string, facts RequestFacts, duration time.Duration, category, errorClass string) ([]byte, string, error) {
	profile, ok := m.auxiliaryProfiles[datadogRUMRole]
	if !ok {
		return nil, "", fmt.Errorf("Datadog RUM telemetry profile is unavailable")
	}
	event, ok := profile.Events[fact]
	if !ok || strings.TrimSpace(event.EventName) == "" {
		return nil, "", fmt.Errorf("Datadog RUM telemetry fact %q is not mapped", fact)
	}
	materials, errMaterials := runtimeMaterialsForDelivery(worker.authSnapshot(), worker.profile)
	if errMaterials != nil {
		return nil, "", errMaterials
	}
	now := m.now().UTC()
	viewID := deterministicUUID("rum-view", worker.binding.RuntimeUUID, m.appSessionID, rendererRecordKey(facts))
	view := map[string]any{
		"id": viewID, "name": "/epitaxy/:redacted", "url": "https://claude.ai/epitaxy/:redacted", "referrer": "",
		"time_spent": maxInt64(0, duration.Nanoseconds()),
	}
	if fact == FactSessionInitialized || fact == FactSessionStopped {
		view["is_active"] = fact == FactSessionInitialized
		if fact == FactSessionInitialized {
			view["loading_type"] = "initial_load"
		}
	}
	payload := map[string]any{
		"date": now.UnixMilli(), "type": event.EventName, "service": "claude-web", "source": "browser",
		"version":     strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		"application": map[string]any{"id": materials["application_id"]},
		"session":     map[string]any{"id": deterministicUUID("rum-session", worker.binding.RuntimeUUID, m.appSessionID), "type": "user", "sampled_for_replay": false},
		"view":        view,
		"usr":         map[string]any{"id": worker.binding.AccountUUID},
		"device":      map[string]any{"type": "desktop", "name": "Windows"},
		"context": map[string]any{
			"deployment_mode": "1p", "renderer_surface": "epitaxy", "organization_uuid": worker.binding.OrganizationUUID,
			"session_id": rendererRecordID(facts), "client_request_id": facts.ClientRequestID,
		},
	}
	switch fact {
	case FactRequestStarted, FactFirstByte:
		payload["action"] = map[string]any{
			"id": uuid.New().String(), "type": "custom", "name": profileEventName(profile, fact),
			"target": map[string]any{"name": profileEventName(profile, fact)}, "loading_time": maxInt64(0, duration.Nanoseconds()),
		}
	case FactRequestFailed:
		payload["error"] = map[string]any{
			"id": uuid.New().String(), "source": "custom", "type": normalizeErrorCategory(errorClass),
			"message": "Claude Desktop upstream request failed", "handling": "handled",
			"handling_stack": "generic", "stack": "",
		}
		payload["context"].(map[string]any)["error_category"] = normalizeErrorCategory(category)
	}
	encoded, errMarshal := json.Marshal(payload)
	return encoded, event.EventName, errMarshal
}

func (w *accountWorker) ensureRUMSessionInitialized(ctx context.Context, facts RequestFacts) (bool, error) {
	if w == nil || w.profile.endpointRole != datadogRUMRole {
		return false, nil
	}
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	digest := sha256String(w.manager.appSessionID + "\x00" + rendererRecordKey(facts))
	markerPath := filepath.Join(w.sessionDir, digest+".rum-view-initialized")
	if _, errStat := os.Stat(markerPath); errStat == nil {
		return false, nil
	} else if !os.IsNotExist(errStat) {
		return false, errStat
	}
	payload, eventName, errProject := w.manager.projectRUM(w, FactSessionInitialized, facts, 0, "", "")
	if errProject != nil {
		return false, errProject
	}
	if errEnqueue := w.enqueue(Envelope{
		Version: 1, EventUUID: uuid.New().String(), EndpointRole: datadogRUMRole, CatalogFact: FactSessionInitialized, CatalogEvent: eventName,
		OccurredAt: w.manager.now().UTC().Format(time.RFC3339Nano), Binding: w.binding, SessionID: facts.SessionID,
		ClientRequestID: facts.ClientRequestID, Payload: payload,
	}); errEnqueue != nil {
		return false, errEnqueue
	}
	marker, errOpen := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		if os.IsExist(errOpen) {
			return false, nil
		}
		return true, errOpen
	}
	if _, errWrite := marker.WriteString(w.manager.now().UTC().Format(time.RFC3339Nano)); errWrite != nil {
		_ = marker.Close()
		_ = os.Remove(markerPath)
		return true, errWrite
	}
	if errSync := marker.Sync(); errSync != nil {
		_ = marker.Close()
		_ = os.Remove(markerPath)
		return true, errSync
	}
	if errClose := marker.Close(); errClose != nil {
		_ = os.Remove(markerPath)
		return true, errClose
	}
	return true, nil
}

func profileEventName(profile claudeprofile.AuxiliaryTelemetryProfile, fact string) string {
	switch fact {
	case FactRequestStarted:
		return "claudeai.code.message.submitted"
	case FactFirstByte:
		return "claudeai.code.session.ttft"
	default:
		return profile.Events[fact].EventName
	}
}

func (m *Manager) projectSentryError(worker *accountWorker, facts RequestFacts, duration time.Duration, category, errorClass string) ([]byte, string, error) {
	profile, ok := m.auxiliaryProfiles[sentryRole]
	if !ok {
		return nil, "", fmt.Errorf("Sentry telemetry profile is unavailable")
	}
	event, ok := profile.Events[FactRequestFailed]
	if !ok || event.EventName != "event" {
		return nil, "", fmt.Errorf("Sentry request failure event is not mapped")
	}
	now := m.now().UTC()
	payload := map[string]any{
		"event_id": strings.ReplaceAll(uuid.New().String(), "-", ""), "timestamp": float64(now.UnixNano()) / float64(time.Second),
		"platform": "javascript", "level": "error", "environment": "production", "release": "Claude@" + strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		"user": map[string]any{"id": worker.binding.AccountUUID},
		"tags": map[string]any{"deployment_mode": "1p", "renderer_surface": "epitaxy", "request_role": facts.Role},
		"contexts": map[string]any{
			"trace":  map[string]any{"trace_id": randomHex(16), "span_id": randomHex(8), "op": "claude.request", "status": "internal_error"},
			"claude": map[string]any{"session_id": rendererRecordID(facts), "client_request_id": facts.ClientRequestID, "duration_ms": duration.Milliseconds()},
		},
		"exception": map[string]any{"values": []map[string]any{{
			"type": normalizeErrorCategory(errorClass), "value": "Claude Desktop upstream request failed",
			"mechanism": map[string]any{"type": "generic", "handled": true},
		}}},
		"extra": map[string]any{"error_category": normalizeErrorCategory(category), "model": facts.Model},
	}
	encoded, errMarshal := json.Marshal(payload)
	return encoded, event.EventName, errMarshal
}

func (m *Manager) projectDatadogBrowserError(worker *accountWorker, facts RequestFacts, duration time.Duration, err error, status int) ([]byte, string, error) {
	profile, ok := m.auxiliaryProfiles[datadogLogsBrowserRole]
	if !ok {
		return nil, "", fmt.Errorf("Datadog browser logs telemetry profile is unavailable")
	}
	event, ok := profile.Events[FactSDKRetry]
	if !ok || strings.TrimSpace(event.EventName) == "" {
		return nil, "", fmt.Errorf("Datadog browser logs retry fact is not mapped")
	}
	host := rendererHostSnapshotNow()
	if m.hostSnapshot != nil {
		host = m.hostSnapshot()
	}
	errorKind := safeErrorClass(err)
	message := sdkRetryError(err, status)
	payload := map[string]any{
		"ddtags": datadogTags(map[string]any{
			"entrypoint": "claude-desktop", "model": facts.Model, "platform": "win32", "version": m.bundle.CodeVersion,
		}, event.EventName),
		"service":            "claude-code-errors",
		"hostname":           "claude-code",
		"status":             "error",
		"message":            message,
		"timestamp":          m.now().UTC().Format(time.RFC3339Nano),
		"error":              map[string]any{"fingerprint": errorKind, "handling": "handled", "kind": errorKind, "message": message, "stack": ""},
		"version":            m.bundle.CodeVersion + "_win32",
		"sourcemap_group":    m.bundle.CodeVersion + "_win32",
		"env":                "production",
		"user_bucket":        stableBucket(worker.binding.AccountUUID),
		"origin":             "claude-desktop",
		"host_platform":      "win32",
		"host_os_release":    host.OSRelease,
		"host_name_redacted": "redacted",
		"entrypoint":         "claude-desktop",
		"node_version":       m.sdkEnvironment().NodeVersion,
		"bun_version":        "",
		"model":              facts.Model,
		"error_frames":       []any{},
		"feature_flags":      map[string]any{},
		"duration_ms":        maxInt64(0, duration.Milliseconds()),
	}
	encoded, errMarshal := json.Marshal(payload)
	return encoded, event.EventName, errMarshal
}

func (m *Manager) enqueueDatadogLog(auth *cliproxyauth.Auth, fact string, facts RequestFacts, sdkEventName string, metadata any) error {
	delivery, ok := m.auxiliaryDeliveries[datadogLogsRole]
	if !ok {
		return fmt.Errorf("Datadog logs delivery profile is unavailable")
	}
	profile := m.auxiliaryProfiles[datadogLogsRole]
	mappedEvent, mapped := profile.Events[fact]
	if !mapped {
		return nil
	}
	logEventName := strings.TrimSpace(mappedEvent.EventName)
	if logEventName == "" {
		logEventName = sdkEventName
	}
	worker, errWorker := m.workerForDelivery(auth, delivery)
	if errWorker != nil {
		return errWorker
	}
	metadataFields := make(map[string]any)
	encodedMetadata, errMetadata := json.Marshal(metadata)
	if errMetadata != nil {
		return errMetadata
	}
	_ = json.Unmarshal(encodedMetadata, &metadataFields)
	flattened := datadogMetadataFields(metadataFields)
	common := map[string]any{
		"ddsource": "nodejs", "message": logEventName, "service": "claude-code", "hostname": "claude-code", "env": "external",
		"model": datadogModelName(facts.Model), "session_id": facts.SessionID, "user_type": "external", "betas": facts.Betas,
		"entrypoint": "claude-desktop", "agent_sdk_version": m.bundle.AgentSDKVersion, "is_interactive": "false", "client_type": "claude-desktop",
		"subscription_type": subscriptionType(auth),
		"platform":          "win32", "platform_raw": "win32", "arch": "x64", "node_version": m.sdkEnvironment().NodeVersion,
		"terminal": "", "shell": "", "package_managers": "", "runtimes": "",
		"is_running_with_bun": false, "is_ci": false, "is_claubbit": false, "is_claude_code_remote": false,
		"is_local_agent_mode": true, "is_conductor": false, "is_github_action": false, "is_claude_code_action": false, "is_claude_ai_auth": true,
		"version": m.bundle.CodeVersion, "version_base": m.bundle.CodeVersion, "build_time": m.sdkEnvironment().BuildTime,
		"deployment_environment": "production", "user_bucket": stableBucket(worker.binding.AccountUUID),
	}
	if promptID := strings.TrimSpace(facts.PromptID); promptID != "" && facts.Role != claudeprofile.RoleTitle {
		common["prompt_id"] = promptID
	}
	if snapshot, okSnapshot := m.sdkProcessSnapshot(); okSnapshot {
		common["process_metrics"] = snapshot
	}
	for key, value := range common {
		flattened[key] = value
	}
	flattened["ddtags"] = datadogTags(flattened, logEventName)
	payload, errPayload := json.Marshal(flattened)
	if errPayload != nil {
		return errPayload
	}
	return worker.enqueue(Envelope{
		Version: 1, EventUUID: uuid.New().String(), EndpointRole: datadogLogsRole, CatalogFact: fact, CatalogEvent: logEventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: facts.SessionID,
		ClientRequestID: facts.ClientRequestID, Payload: payload,
	})
}

// The SDK batch and Datadog sender have distinct field contracts. The latter
// separates every ASCII capital (costUSD -> cost_u_s_d), renames the prompt
// identifier, and omits the SDK's endpoint URL.
func datadogMetadataFields(metadata map[string]any) map[string]any {
	result := make(map[string]any, len(metadata))
	for name, value := range metadata {
		if name == "baseUrl" || name == "base_url" || name == "toolUseContentLengths" || name == "tool_use_content_lengths" {
			continue
		}
		if name == "cc_prompt_id" {
			if promptID, ok := value.(string); ok && promptID != "" {
				result["prompt_id"] = promptID
			}
			continue
		}
		var key strings.Builder
		for _, character := range name {
			if character >= 'A' && character <= 'Z' {
				key.WriteByte('_')
				character += 'a' - 'A'
			}
			key.WriteRune(character)
		}
		result[key.String()] = value
	}
	return result
}

func datadogModelName(model string) string {
	// Captured Datadog dimensions omit dated model revisions, unlike the SDK
	// event payload (for example claude-haiku-4-5-20251001).
	if len(model) > 9 && model[len(model)-9] == '-' {
		for _, digit := range model[len(model)-8:] {
			if digit < '0' || digit > '9' {
				return model
			}
		}
		return model[:len(model)-9]
	}
	return model
}

func datadogTags(fields map[string]any, eventName string) string {
	tags := map[string]string{
		"event": eventName, "arch": auxiliaryStringValue(fields["arch"]), "client_type": auxiliaryStringValue(fields["client_type"]),
		"entrypoint": auxiliaryStringValue(fields["entrypoint"]), "model": auxiliaryStringValue(fields["model"]), "platform": auxiliaryStringValue(fields["platform"]),
		"subscription_type": auxiliaryStringValue(fields["subscription_type"]), "user_type": auxiliaryStringValue(fields["user_type"]),
		"version": auxiliaryStringValue(fields["version"]), "version_base": auxiliaryStringValue(fields["version_base"]),
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if tags[key] != "" {
			parts = append(parts, key+":"+tags[key])
		}
	}
	return strings.Join(parts, ",")
}

func (w *accountWorker) ensureAuxiliaryRuntimeStarted(ctx context.Context) error {
	if w == nil || w.profile.endpointRole != sentryRole {
		return nil
	}
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	markerPath := w.sentryRuntimeMarkerPath()
	if _, errStat := os.Stat(markerPath); errStat == nil {
		return nil
	} else if !os.IsNotExist(errStat) {
		return errStat
	}
	payload, eventName, errProject := w.manager.projectSentrySession(w, true)
	if errProject != nil {
		return errProject
	}
	eventID := uuid.New().String()
	if errEnqueue := w.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: sentryRole, CatalogFact: FactRuntimeStarted, CatalogEvent: eventName,
		OccurredAt: w.manager.now().UTC().Format(time.RFC3339Nano), Binding: w.binding, SessionID: w.manager.appSessionID,
		ClientRequestID: eventID, Payload: payload,
	}); errEnqueue != nil {
		return errEnqueue
	}
	marker, errOpen := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		if os.IsExist(errOpen) {
			return nil
		}
		return errOpen
	}
	return marker.Close()
}

func (w *accountWorker) sentryRuntimeMarkerPath() string {
	if w == nil || w.manager == nil {
		return ""
	}
	return filepath.Join(w.sessionDir, "runtime."+w.manager.appSessionID+".started")
}

func (m *Manager) projectSentrySession(worker *accountWorker, started bool) ([]byte, string, error) {
	profile := m.auxiliaryProfiles[sentryRole]
	fact := FactRuntimeStopped
	if started {
		fact = FactRuntimeStarted
	}
	event, ok := profile.Events[fact]
	if !ok || event.EventName != "session" {
		return nil, "", fmt.Errorf("Sentry runtime session fact %q is not mapped", fact)
	}
	now := m.now().UTC()
	payload := map[string]any{
		"sid": m.appSessionID, "init": started, "started": float64(m.startedAt.UnixNano()) / float64(time.Second),
		"timestamp": float64(now.UnixNano()) / float64(time.Second), "status": map[bool]string{true: "ok", false: "exited"}[started],
		"errors": 0, "did": worker.binding.AccountUUID,
		"attrs": map[string]any{"release": "Claude@" + strings.TrimSuffix(m.bundle.DesktopVersion, ".0"), "environment": "production"},
	}
	encoded, errMarshal := json.Marshal(payload)
	return encoded, event.EventName, errMarshal
}

func (m *Manager) emitAuxiliaryRuntimeStopped() {
	if m == nil || m.freezeOnShutdown.Load() {
		return
	}
	m.mu.RLock()
	workers := make([]*accountWorker, 0)
	for _, worker := range m.workers {
		if worker.profile.endpointRole == sentryRole {
			workers = append(workers, worker)
		}
	}
	m.mu.RUnlock()
	for _, worker := range workers {
		payload, eventName, errProject := m.projectSentrySession(worker, false)
		facts := RequestFacts{SessionID: m.appSessionID, ClientRequestID: uuid.New().String()}
		m.enqueueAuxiliary(context.Background(), worker, FactRuntimeStopped, eventName, facts, payload, errProject)
		if m.freezeOnShutdown.Load() {
			return
		}
		markerPath := worker.sentryRuntimeMarkerPath()
		if markerPath != "" {
			if errRemove := os.Remove(markerPath); errRemove != nil && !os.IsNotExist(errRemove) {
				worker.recordQueueFailure(errRemove)
				log.WithError(errRemove).Warn("claude desktop Sentry telemetry: runtime marker was not cleared")
			}
		}
	}
}

func encodeDeliveryBatch(profile deliveryProfile, envelopes []Envelope, materials map[string]string, now time.Time) (encodedDeliveryBatch, error) {
	switch profile.bodyFormat {
	case "events-wrapper-json":
		var body bytes.Buffer
		body.WriteString(`{"events":[`)
		for i, envelope := range envelopes {
			if i > 0 {
				body.WriteByte(',')
			}
			if envelope.CatalogFact == factGrowthbookExposure {
				if !json.Valid(envelope.Payload) {
					return encodedDeliveryBatch{}, fmt.Errorf("invalid SDK exposure payload")
				}
				body.Write(envelope.Payload)
			} else {
				encoded, err := json.Marshal(envelope.Payload)
				if err != nil {
					return encodedDeliveryBatch{}, err
				}
				body.Write(encoded)
			}
		}
		body.WriteString(`]}`)
		return encodedDeliveryBatch{body: body.Bytes()}, nil
	case "segment-batch-json":
		// analytics.js batch order: {writeKey, batch, sentAt}; every item carries
		// its keys in call order with writeKey inserted before _metadata.
		writeKey, errWriteKey := json.Marshal(materials["write_key"])
		if errWriteKey != nil {
			return encodedDeliveryBatch{}, errWriteKey
		}
		var body bytes.Buffer
		body.WriteString(`{"writeKey":`)
		body.Write(writeKey)
		body.WriteString(`,"batch":[`)
		for i, envelope := range envelopes {
			item, errItem := segmentBatchItemWithWriteKey(envelope.Payload, writeKey)
			if errItem != nil {
				return encodedDeliveryBatch{}, errItem
			}
			if i > 0 {
				body.WriteByte(',')
			}
			body.Write(item)
		}
		body.WriteString(`],"sentAt":"` + now.UTC().Format(time.RFC3339Nano) + `"}`)
		if !json.Valid(body.Bytes()) {
			return encodedDeliveryBatch{}, fmt.Errorf("Segment batch is not valid JSON")
		}
		return encodedDeliveryBatch{body: body.Bytes()}, nil
	case "datadog-logs-json", "datadog-browser-logs-json":
		items := make([]json.RawMessage, 0, len(envelopes))
		for _, envelope := range envelopes {
			items = append(items, envelope.Payload)
		}
		encoded, errMarshal := json.Marshal(items)
		result := encodedDeliveryBatch{body: encoded}
		if profile.bodyFormat == "datadog-browser-logs-json" {
			result.query = url.Values{
				"ddsource":              {"browser"},
				"dd-api-key":            {materials["api_key"]},
				"dd-evp-origin":         {"browser"},
				"dd-evp-origin-version": {"7.6.0"},
				"dd-request-id":         {uuid.New().String()},
			}
		}
		return result, errMarshal
	case "datadog-rum-ndjson":
		var body bytes.Buffer
		for _, envelope := range envelopes {
			body.Write(envelope.Payload)
			body.WriteByte('\n')
		}
		encoded := body.Bytes()
		contentEncoding := ""
		query := url.Values{
			"ddsource": {"browser"}, "dd-api-key": {materials["client_token"]}, "dd-evp-origin-version": {"7.6.0"},
			"dd-evp-origin": {"browser"}, "dd-request-id": {uuid.New().String()}, "batch_time": {strconv.FormatInt(now.UnixMilli(), 10)}, "_dd.api": {"fetch"},
		}
		if len(encoded) >= 16*1024 {
			var compressed bytes.Buffer
			writer := zlib.NewWriter(&compressed)
			if _, errWrite := writer.Write(encoded); errWrite != nil {
				return encodedDeliveryBatch{}, errWrite
			}
			if errClose := writer.Close(); errClose != nil {
				return encodedDeliveryBatch{}, errClose
			}
			encoded = compressed.Bytes()
			contentEncoding = "deflate"
			query.Set("dd-evp-encoding", "deflate")
		}
		return encodedDeliveryBatch{body: encoded, contentEncoding: contentEncoding, query: query}, nil
	case "sentry-envelope":
		materialsKey := materials["public_key"]
		dsn := "https://" + materialsKey + "@o1158394.ingest.us.sentry.io/4507368973008896"
		eventID := strings.ReplaceAll(uuid.New().String(), "-", "")
		if len(envelopes) > 0 {
			if candidate := strings.ReplaceAll(strings.TrimSpace(envelopes[0].EventUUID), "-", ""); candidate != "" {
				eventID = candidate
			}
		}
		header, errHeader := json.Marshal(map[string]any{
			"event_id": eventID, "sent_at": now.UTC().Format(time.RFC3339Nano),
			"sdk": map[string]string{"name": "sentry.javascript.electron", "version": "7.12.0"}, "dsn": dsn,
		})
		if errHeader != nil {
			return encodedDeliveryBatch{}, errHeader
		}
		var body bytes.Buffer
		body.Write(header)
		body.WriteByte('\n')
		for _, envelope := range envelopes {
			payload, errPayload := envelopePayloadBytes(envelope)
			if errPayload != nil {
				return encodedDeliveryBatch{}, errPayload
			}
			headerFields := make(map[string]any, len(envelope.ItemHeaders)+2)
			for key, value := range envelope.ItemHeaders {
				headerFields[key] = value
			}
			headerFields["type"] = envelope.CatalogEvent
			headerFields["length"] = len(payload)
			itemHeader, errItem := json.Marshal(headerFields)
			if errItem != nil {
				return encodedDeliveryBatch{}, errItem
			}
			body.Write(itemHeader)
			body.WriteByte('\n')
			body.Write(payload)
			body.WriteByte('\n')
		}
		encoded := body.Bytes()
		contentEncoding := ""
		if len(encoded) >= 64*1024 {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, errWrite := writer.Write(encoded); errWrite != nil {
				return encodedDeliveryBatch{}, errWrite
			}
			if errClose := writer.Close(); errClose != nil {
				return encodedDeliveryBatch{}, errClose
			}
			encoded = compressed.Bytes()
			contentEncoding = "gzip"
		}
		query := url.Values{"sentry_key": {materialsKey}, "sentry_version": {"7"}, "sentry_client": {"sentry.javascript.electron/7.12.0"}}
		return encodedDeliveryBatch{body: encoded, contentEncoding: contentEncoding, query: query}, nil
	default:
		return encodedDeliveryBatch{}, fmt.Errorf("unsupported telemetry delivery body format %q", profile.bodyFormat)
	}
}

func envelopePayloadSize(envelope Envelope) int {
	if envelope.PayloadEncoding != "base64" {
		return len(envelope.Payload)
	}
	var encoded string
	if json.Unmarshal(envelope.Payload, &encoded) != nil {
		return len(envelope.Payload)
	}
	return base64.StdEncoding.DecodedLen(len(encoded))
}

func envelopePayloadBytes(envelope Envelope) ([]byte, error) {
	switch envelope.PayloadEncoding {
	case "":
		return envelope.Payload, nil
	case "base64":
		var encoded string
		if err := json.Unmarshal(envelope.Payload, &encoded); err != nil {
			return nil, fmt.Errorf("decode base64 telemetry payload wrapper: %w", err)
		}
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode base64 telemetry payload: %w", err)
		}
		return payload, nil
	default:
		return nil, fmt.Errorf("unsupported telemetry payload encoding %q", envelope.PayloadEncoding)
	}
}

// segmentBatchItemWithWriteKey inserts the per-item writeKey where
// analytics.js places it (immediately before the trailing _metadata object)
// without re-encoding the item, so the projected key order reaches the wire.
// Items without a trailing _metadata object fall back to a decoded copy.
func segmentBatchItemWithWriteKey(payload []byte, writeKey []byte) ([]byte, error) {
	var keys map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &keys); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if _, has := keys["writeKey"]; has {
		return payload, nil
	}
	const metadataMarker = `,"_metadata":`
	if _, hasMetadata := keys["_metadata"]; hasMetadata {
		index := bytes.LastIndex(payload, []byte(metadataMarker))
		if index > 0 {
			var item bytes.Buffer
			item.Write(payload[:index])
			item.WriteString(`,"writeKey":`)
			item.Write(writeKey)
			item.Write(payload[index:])
			if json.Valid(item.Bytes()) {
				return item.Bytes(), nil
			}
		}
	}
	item := make(map[string]any, len(keys)+1)
	if errUnmarshal := json.Unmarshal(payload, &item); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	item["writeKey"] = json.RawMessage(writeKey)
	return json.Marshal(item)
}

// segmentCallItem is the analytics.js call envelope in its native key order:
// timestamp, integrations, event|name, type, properties, context, messageId,
// userId, anonymousId, (writeKey inserted at delivery), _metadata. Page calls
// place `name` after `properties`.
type segmentCallItem struct {
	Timestamp    string          `json:"timestamp"`
	Integrations map[string]bool `json:"integrations"`
	Event        string          `json:"event,omitempty"`
	Type         string          `json:"type"`
	Properties   json.RawMessage `json:"properties"`
	Name         string          `json:"name,omitempty"`
	Context      map[string]any  `json:"context"`
	MessageID    string          `json:"messageId"`
	UserID       string          `json:"userId"`
	AnonymousID  string          `json:"anonymousId"`
	Metadata     segmentMetadata `json:"_metadata"`
}

type segmentMetadata struct {
	Bundled    string   `json:"bundled"`
	Unbundled  []string `json:"unbundled"`
	BundledIDs []string `json:"bundledIds"`
}

func applyAuxiliaryRequest(request *http.Request, profile deliveryProfile, materials map[string]string, encoded encodedDeliveryBatch) error {
	if request == nil {
		return fmt.Errorf("telemetry request is nil")
	}
	if len(encoded.query) > 0 {
		request.URL.RawQuery = encoded.query.Encode()
	}
	if encoded.contentEncoding != "" {
		request.Header.Set("Content-Encoding", encoded.contentEncoding)
	}
	if profile.endpointRole == datadogLogsRole {
		key := strings.TrimSpace(materials["api_key"])
		if key == "" {
			return fmt.Errorf("Datadog logs API key is unavailable")
		}
		request.Header.Set("DD-API-KEY", key)
	} else if profile.endpointRole == datadogLogsBrowserRole && strings.TrimSpace(materials["api_key"]) == "" {
		return fmt.Errorf("Datadog browser logs API key is unavailable")
	}
	return nil
}

func authMetadataString(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func authMetadataStringDefault(auth *cliproxyauth.Auth, key, fallback string) string {
	if value := authMetadataString(auth, key); value != "" {
		return value
	}
	return fallback
}

func authMetadataInt64(auth *cliproxyauth.Auth, key string) int64 {
	if auth == nil || auth.Metadata == nil {
		return 0
	}
	switch value := auth.Metadata[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func auxiliaryStringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func stableBucket(value string) int {
	digest := sha256.Sum256([]byte(value))
	return int(digest[0]) % 100
}

func deterministicUUID(namespace string, values ...string) string {
	material := append([]string{namespace}, values...)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(strings.Join(material, "\x00"))).String()
}

func randomHex(bytesLength int) string {
	value := strings.ReplaceAll(uuid.New().String(), "-", "")
	for len(value) < bytesLength*2 {
		value += strings.ReplaceAll(uuid.New().String(), "-", "")
	}
	return value[:bytesLength*2]
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
