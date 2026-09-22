package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Renderer session-open analytics (lane W6 renderer-session).
//
// When the claude.ai renderer hosted in the Desktop webview opens a freshly
// created local session it navigates to `/epitaxy/<sessionId>` and reports, in
// this order, the Segment `page` call named `/epitaxy/:redacted`, then the
// `claudeai.epitaxy.session.opened` and `claudeai.epitaxy.session.meta_resolved`
// tracks (each also copied into the Desktop event-logging batch with the
// dual-fire suffix and path `/epitaxy/$sessionId`). The names are absent from
// the local Electron chunks (remote renderer), so key order and constants are
// pinned on SHA-256 verified captured request bodies in
// testdata/desktop-telemetry-renderer-session-native.json (see
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-renderer-session-source.mjs).
//
// Gateway moment: the first turn of a session (store.go ensureSessionInitialized
// through the desktop session-start hook registry), which is where the
// emulated renderer opens the session before desktop_ccd_session_initialized.
const (
	FactRendererSessionPage         = "renderer_session_page"
	FactRendererSessionOpened       = "renderer_session_opened"
	FactRendererSessionMetaResolved = "renderer_session_meta_resolved"
	// FactRendererShellPage is the Segment `page` call named `/epitaxy` the
	// renderer shell reports once per application activation (32/32 constant).
	FactRendererShellPage = "renderer_shell_page"
	// FactRendererPageViewed is the Desktop event-logging `page_viewed` copy
	// of a renderer page call. It is not a same-fact twin: its property order
	// differs from the Segment page properties (103/104 captured occurrences).
	FactRendererPageViewed = "renderer_page_viewed"
	// FactRendererPermissionModeChanged is claudeai.code.permission_mode.changed
	// (7/7 captured occurrences: surface ccd, change_method mode_dropdown).
	FactRendererPermissionModeChanged = "renderer_permission_mode_changed"
)

const (
	rendererSessionPageName    = "/epitaxy/:redacted"
	rendererSessionPageURL     = "https://claude.ai" + rendererSessionPageName
	rendererShellPageName      = "/epitaxy"
	rendererShellPageURL       = "https://claude.ai" + rendererShellPageName
	rendererSessionSurface     = "claude-ai"
	rendererSessionDeployment  = "1p"
	rendererSessionType        = "local"
	rendererSessionSurfaceName = "epitaxy"
	rendererSessionVersion     = 1
	rendererPermissionSurface  = "ccd"
	rendererPermissionMethod   = "mode_dropdown"
)

func init() {
	registerExecutableEvents(segmentRole, map[string]string{
		FactRendererSessionPage:           rendererSessionPageName,
		FactRendererShellPage:             rendererShellPageName,
		FactRendererSessionOpened:         "claudeai.epitaxy.session.opened",
		FactRendererSessionMetaResolved:   "claudeai.epitaxy.session.meta_resolved",
		FactRendererPermissionModeChanged: "claudeai.code.permission_mode.changed",
	})
	registerExecutableEvents("desktop-event-logging", map[string]string{
		FactRendererSessionOpened:         "claudeai.epitaxy.session.opened",
		FactRendererSessionMetaResolved:   "claudeai.epitaxy.session.meta_resolved",
		FactRendererPageViewed:            "page_viewed",
		FactRendererPermissionModeChanged: "claudeai.code.permission_mode.changed",
	})
	registerDesktopSessionStartHook(emitRendererSessionOpened)
	registerRendererActivationHook(emitRendererShellActivation)
	registerRendererRequestHook(emitRendererPermissionModeChanged)
}

// Renderer request hooks run for every main-role prompt-starting request after
// the session was initialized (manager.go BeginRequest, after
// ensureSessionInitialized). They let topic files compare consecutive requests
// of one session. Insertion: `m.runRendererRequestHooks(ctx, span)`.
type rendererRequestHook func(ctx context.Context, m *Manager, span *RequestSpan)

var rendererRequestHooks []rendererRequestHook

func registerRendererRequestHook(hook rendererRequestHook) {
	if hook != nil {
		rendererRequestHooks = append(rendererRequestHooks, hook)
	}
}

// runRendererRequestHooks is the single insertion point for BeginRequest.
// Hook failures never block the request.
func (m *Manager) runRendererRequestHooks(ctx context.Context, span *RequestSpan) {
	if m == nil || span == nil || span.worker == nil {
		return
	}
	for _, hook := range rendererRequestHooks {
		hook(ctx, m, span)
	}
}

// desktopRendererPageViewedProperties keeps the captured `page_viewed` key
// order of the Desktop copy (103/104 occurrences; the remaining one is the
// unauthenticated /login page without account keys).
type desktopRendererPageViewedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	DualFire         bool   `json:"_dual_fire"`
	URL              string `json:"url"`
	Path             string `json:"path"`
	PageName         string `json:"page_name"`
	AnonymousID      string `json:"anonymous_id"`
	ServiceName      string `json:"service_name"`
	CanonicalPath    string `json:"canonical_path"`
	CanonicalURL     string `json:"canonical_url"`
}

// rendererPageRoute names the route markers the Desktop `page_viewed` copy
// carries for one renderer page. The session route keeps the literal
// `$sessionId` template in url/path/page_name (46/46) and wildcards the
// canonical markers (`/epitaxy/*`); the shell route repeats `/epitaxy` (54/54).
type rendererPageRoute struct {
	URL, Path, PageName, CanonicalPath, CanonicalURL string
}

var (
	rendererShellPageRoute   = rendererPageRoute{URL: rendererShellPageURL, Path: rendererShellPageName, PageName: rendererShellPageName, CanonicalPath: rendererShellPageName, CanonicalURL: rendererShellPageURL}
	rendererSessionPageRoute = rendererPageRoute{URL: "https://claude.ai" + rendererCopyPathSession, Path: rendererCopyPathSession, PageName: rendererCopyPathSession, CanonicalPath: "/epitaxy/*", CanonicalURL: "https://claude.ai/epitaxy/*"}
)

func buildRendererShellPage(identity rendererSessionIdentity) rendererSessionPageProperties {
	return rendererSessionPageProperties{
		Path: rendererShellPageName, Referrer: "", Search: "", URL: rendererShellPageURL,
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Name: rendererShellPageName, CanonicalPath: rendererShellPageName, CanonicalURL: rendererShellPageURL,
	}
}

func buildDesktopRendererPageViewed(identity rendererSessionIdentity, anonymousID string, route rendererPageRoute) desktopRendererPageViewedProperties {
	return desktopRendererPageViewedProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		DualFire: true, URL: route.URL, Path: route.Path, PageName: route.PageName, AnonymousID: anonymousID,
		ServiceName: desktopRendererServiceName, CanonicalPath: route.CanonicalPath, CanonicalURL: route.CanonicalURL,
	}
}

// emitRendererPageWithViewedCopy delivers a Segment `page` call and, only when
// that call was projected and persisted, the Desktop `page_viewed` copy for the
// same route (the captured copies without a Segment twin carry _dual_fire
// false and are not reproduced).
func (m *Manager) emitRendererPageWithViewedCopy(ctx context.Context, segmentWorker, desktopWorker *accountWorker, facts RequestFacts, pageFact string, properties json.RawMessage, route rendererPageRoute) {
	if m == nil || segmentWorker == nil {
		return
	}
	if profile, ok := m.auxiliaryProfiles[segmentRole]; !ok || strings.TrimSpace(profile.Events[pageFact].EventName) == "" {
		return
	}
	payload, eventName, errProject := m.projectRendererCall(segmentWorker, rendererCallPage, pageFact, properties)
	m.enqueueAuxiliary(ctx, segmentWorker, pageFact, eventName, facts, payload, errProject)
	if errProject != nil || desktopWorker == nil {
		return
	}
	event, mapped := m.profile.Events[FactRendererPageViewed]
	if !mapped || strings.TrimSpace(event.EventName) == "" {
		return
	}
	identity := m.rendererSessionIdentity(desktopWorker.binding, desktopWorker.authSnapshot())
	viewed, errViewed := json.Marshal(buildDesktopRendererPageViewed(identity, gjson.GetBytes(payload, "anonymousId").String(), route))
	if errViewed != nil {
		desktopWorker.recordQueueFailure(errViewed)
		return
	}
	eventID := uuid.New().String()
	encoded, errMarshal := json.Marshal(desktopRendererEvent{
		EventType: desktopRendererEventType,
		EventData: desktopRendererEventData{
			EventName: event.EventName, EventID: eventID,
			EventTimestamp:   m.now().UTC().Format("2006-01-02T15:04:05.000Z"),
			AccountUUID:      desktopWorker.binding.AccountUUID,
			OrganizationUUID: desktopWorker.binding.OrganizationUUID,
			Properties:       string(viewed),
		},
	})
	if errMarshal != nil {
		desktopWorker.recordQueueFailure(errMarshal)
		return
	}
	if errEnqueue := desktopWorker.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: m.profile.EndpointRole, CatalogFact: FactRendererPageViewed, CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: desktopWorker.binding, SessionID: facts.SessionID,
		ClientRequestID: facts.ClientRequestID, Payload: encoded,
	}); errEnqueue != nil {
		desktopWorker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: page_viewed copy was not persisted")
		return
	}
	select {
	case desktopWorker.wake <- struct{}{}:
	default:
	}
}

// emitRendererShellActivation is the renderer activation hook: the shell page
// call `/epitaxy` (and its page_viewed copy) right after the Segment identify
// of an application activation.
func emitRendererShellActivation(ctx context.Context, m *Manager, segmentWorker *accountWorker) {
	if m == nil || segmentWorker == nil {
		return
	}
	auth := segmentWorker.authSnapshot()
	properties, errMarshal := json.Marshal(buildRendererShellPage(m.rendererSessionIdentity(segmentWorker.binding, auth)))
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("claude desktop renderer telemetry: shell page properties were not encoded")
		return
	}
	desktopWorker, errDesktop := m.workerFor(auth)
	if errDesktop != nil {
		log.WithError(errDesktop).Debug("claude desktop renderer telemetry: Desktop runtime unavailable for page_viewed")
		desktopWorker = nil
	}
	m.emitRendererPageWithViewedCopy(ctx, segmentWorker, desktopWorker, m.rendererActivationFacts(), FactRendererShellPage, properties, rendererShellPageRoute)
}

// rendererPermissionModeChangedProperties keeps the captured key order of
// claudeai.code.permission_mode.changed (3 Segment + 4 Desktop occurrences,
// one order).
type rendererPermissionModeChangedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	PreviousMode     string `json:"previous_mode"`
	CurrentMode      string `json:"current_mode"`
	ChangeMethod     string `json:"change_method"`
	RendererSurface  string `json:"renderer_surface"`
}

func buildRendererPermissionModeChanged(identity rendererSessionIdentity, previousMode, currentMode string) rendererPermissionModeChangedProperties {
	return rendererPermissionModeChangedProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererPermissionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Version: rendererSessionVersion, PreviousMode: previousMode, CurrentMode: currentMode,
		ChangeMethod: rendererPermissionMethod, RendererSurface: rendererSessionSurfaceName,
	}
}

// rendererPermissionModes remembers the permission mode of the last
// prompt-starting request per (manager, account, session record).
var rendererPermissionModes struct {
	sync.Mutex
	modes map[string]string
}

// observeRendererPermissionMode records the request's mode and returns the
// previous mode when the session already had one that differs.
func observeRendererPermissionMode(key, mode string) (previous string, changed bool) {
	rendererPermissionModes.Lock()
	defer rendererPermissionModes.Unlock()
	if rendererPermissionModes.modes == nil {
		rendererPermissionModes.modes = make(map[string]string)
	}
	previous, seen := rendererPermissionModes.modes[key]
	rendererPermissionModes.modes[key] = mode
	return previous, seen && previous != mode
}

// emitRendererPermissionModeChanged is the renderer request hook: when a
// later prompt of a session arrives with a different permission mode than the
// previous prompt, the renderer's mode dropdown was used between the two.
func emitRendererPermissionModeChanged(ctx context.Context, m *Manager, span *RequestSpan) {
	if m == nil || span == nil || span.worker == nil || span.facts.Role != claudeprofile.RoleMain {
		return
	}
	if span.facts.Prompt != nil && !span.facts.Prompt.Identity().StartsPrompt {
		return
	}
	mode := defaultString(span.facts.PermissionMode, "default")
	key := fmt.Sprintf("%p\x00%s\x00%s", m, span.worker.binding.AccountUUID, rendererRecordKey(span.facts))
	previous, changed := observeRendererPermissionMode(key, mode)
	if !changed {
		return
	}
	identity := m.rendererSessionIdentity(span.worker.binding, span.worker.authSnapshot())
	properties, errMarshal := json.Marshal(buildRendererPermissionModeChanged(identity, previous, mode))
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("claude desktop renderer telemetry: permission mode properties were not encoded")
		return
	}
	m.emitRendererTrackAt(ctx, span, FactRendererPermissionModeChanged, properties, rendererCopyPathShell)
}

// rendererSessionPageProperties keeps the captured `/epitaxy/:redacted` page
// property order (25/25 occurrences; every value is constant or account bound).
type rendererSessionPageProperties struct {
	Path             string `json:"path"`
	Referrer         string `json:"referrer"`
	Search           string `json:"search"`
	URL              string `json:"url"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Name             string `json:"name"`
	CanonicalPath    string `json:"canonical_path"`
	CanonicalURL     string `json:"canonical_url"`
}

// rendererSessionOpenedProperties keeps the captured key order of
// claudeai.epitaxy.session.opened (23/24 Segment and 45/46 Desktop
// occurrences; one outlier appended is_mobile_web and is not reproduced).
type rendererSessionOpenedProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
	SessionType      string `json:"session_type"`
	RendererSurface  string `json:"renderer_surface"`
}

// rendererSessionMetaResolvedProperties keeps the captured key order of
// claudeai.epitaxy.session.meta_resolved. session_origin is null in every
// captured occurrence (46 Desktop, 25 Segment), so it is encoded as JSON null.
type rendererSessionMetaResolvedProperties struct {
	AccountUUID      string  `json:"account_uuid"`
	OrganizationUUID string  `json:"organization_uuid"`
	BillingType      string  `json:"billing_type"`
	Surface          string  `json:"surface"`
	DeploymentMode   string  `json:"deployment_mode"`
	AppVersion       string  `json:"app_version"`
	Version          int     `json:"version"`
	SessionID        string  `json:"session_id"`
	SessionType      string  `json:"session_type"`
	SessionOrigin    *string `json:"session_origin"`
	IsAgentOwned     bool    `json:"is_agent_owned"`
	ProjectEnabled   bool    `json:"project_enabled"`
	RendererSurface  string  `json:"renderer_surface"`
}

// rendererSessionIdentity is the account/app tuple shared by the three calls.
type rendererSessionIdentity struct {
	AccountUUID, OrganizationUUID, BillingType, AppVersion string
}

func (m *Manager) rendererSessionIdentity(binding Binding, auth *cliproxyauth.Auth) rendererSessionIdentity {
	appVersion := ""
	if m != nil && m.bundle != nil {
		appVersion = strings.TrimSuffix(m.bundle.DesktopVersion, ".0")
	}
	return rendererSessionIdentity{
		AccountUUID:      binding.AccountUUID,
		OrganizationUUID: binding.OrganizationUUID,
		BillingType:      subscriptionType(auth),
		AppVersion:       appVersion,
	}
}

func buildRendererSessionPage(identity rendererSessionIdentity) rendererSessionPageProperties {
	return rendererSessionPageProperties{
		Path: rendererSessionPageName, Referrer: "", Search: "", URL: rendererSessionPageURL,
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Name: rendererSessionPageName, CanonicalPath: rendererSessionPageName, CanonicalURL: rendererSessionPageURL,
	}
}

func buildRendererSessionOpened(identity rendererSessionIdentity) rendererSessionOpenedProperties {
	return rendererSessionOpenedProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Version: rendererSessionVersion, SessionType: rendererSessionType, RendererSurface: rendererSessionSurfaceName,
	}
}

func buildRendererSessionMetaResolved(identity rendererSessionIdentity, facts RequestFacts) rendererSessionMetaResolvedProperties {
	return rendererSessionMetaResolvedProperties{
		AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
		Surface: rendererSessionSurface, DeploymentMode: rendererSessionDeployment, AppVersion: identity.AppVersion,
		Version: rendererSessionVersion, SessionID: rendererRecordID(facts), SessionType: rendererSessionType,
		SessionOrigin: nil, IsAgentOwned: false, ProjectEnabled: false, RendererSurface: rendererSessionSurfaceName,
	}
}

// emitRendererSessionOpened is the desktop session-start hook: it runs on the
// Desktop worker of the account at the first turn of a session and delivers
// the page call and both tracks (Segment first, Desktop copy second) in the
// captured order. Facts the loaded profiles do not map are skipped by the
// shared renderer helper.
func emitRendererSessionOpened(ctx context.Context, w *accountWorker, facts RequestFacts) {
	if w == nil || w.manager == nil {
		return
	}
	m := w.manager
	auth := w.authSnapshot()
	identity := m.rendererSessionIdentity(w.binding, auth)
	for _, call := range []struct {
		fact  string
		page  bool
		value any
	}{
		{fact: FactRendererSessionPage, page: true, value: buildRendererSessionPage(identity)},
		{fact: FactRendererSessionOpened, value: buildRendererSessionOpened(identity)},
		{fact: FactRendererSessionMetaResolved, value: buildRendererSessionMetaResolved(identity, facts)},
	} {
		properties, errMarshal := json.Marshal(call.value)
		if errMarshal != nil {
			log.WithError(errMarshal).WithField("fact", call.fact).Warn("claude desktop renderer telemetry: session-open properties were not encoded")
			continue
		}
		if call.page {
			segmentWorker, desktopWorker := m.rendererWorkers(auth)
			m.emitRendererPageWithViewedCopy(ctx, segmentWorker, desktopWorker, facts, call.fact, properties, rendererSessionPageRoute)
			continue
		}
		m.emitRendererTrackForAccount(ctx, auth, facts, call.fact, properties)
	}
}
