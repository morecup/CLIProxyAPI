package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Renderer shell-state analytics (lane W7 renderer-inventory).
//
// When the claude.ai renderer shell loads inside the Desktop webview it reports
// a fixed set of shell-state facts right after the Segment identify and before
// any code session is initialised. The event names are absent from the local
// Electron chunks (remote renderer), so the contract is pinned on SHA-256
// verified captured request bodies (see
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-renderer-inventory-source.mjs
// and testdata/desktop-telemetry-renderer-inventory-native.json). Only the
// three events whose values were constant across every captured occurrence are
// emitted; the MCP inventory events and the UI-interaction events stay
// boundaries (recorded inside the golden with the blocking field).
//
// Gateway moment: the first Desktop session of an account inside one
// application activation (desktop session-start hook, before the first
// desktop_ccd_session_initialized), which is where the emulated shell load
// precedes the first session exactly like the captured bootstrap batches.
const (
	// FactRendererDesktopSidebarStateSet mirrors `claudeai.desktop.sidebar.state_set`.
	FactRendererDesktopSidebarStateSet = "renderer_desktop_sidebar_state_set"
	// FactRendererChatFontActive mirrors `claudeai.settings.chat_font.active`.
	FactRendererChatFontActive = "renderer_settings_chat_font_active"
	// FactRendererSidePaneLayoutChanged mirrors `claudeai.epitaxy.side_pane.layout_changed`.
	FactRendererSidePaneLayoutChanged = "renderer_side_pane_layout_changed"
)

const (
	rendererShellSurface          = "claude-ai"
	rendererShellDeploymentMode   = "1p"
	rendererShellRendererSurface  = "epitaxy"
	rendererShellSidebarSource    = "initial_load"
	rendererShellOtherTabActivity = "none"
	rendererShellChatFontDefault  = "default"
)

var rendererShellStateEventNames = map[string]string{
	FactRendererDesktopSidebarStateSet: "claudeai.desktop.sidebar.state_set",
	FactRendererChatFontActive:         "claudeai.settings.chat_font.active",
	FactRendererSidePaneLayoutChanged:  "claudeai.epitaxy.side_pane.layout_changed",
}

func init() {
	registerExecutableEvents(segmentRole, rendererShellStateEventNames)
	registerExecutableEvents("desktop-event-logging", rendererShellStateEventNames)
	registerDesktopSessionStartHook(emitRendererShellStateOnFirstSession)
}

// rendererShellBaseProperties is the captured renderer property prefix shared
// by every shell-state event (native key order).
type rendererShellBaseProperties struct {
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	BillingType      string `json:"billing_type"`
	Surface          string `json:"surface"`
	DeploymentMode   string `json:"deployment_mode"`
	AppVersion       string `json:"app_version"`
	Version          int    `json:"version"`
}

// rendererDesktopSidebarStateSetProperties keeps the captured key order of
// claudeai.desktop.sidebar.state_set (25 occurrences, all constant).
type rendererDesktopSidebarStateSetProperties struct {
	rendererShellBaseProperties
	IsExpanded       bool   `json:"is_expanded"`
	Source           string `json:"source"`
	OtherTabActivity string `json:"other_tab_activity"`
}

// rendererChatFontActiveProperties keeps the captured key order of
// claudeai.settings.chat_font.active (25 occurrences, all constant).
type rendererChatFontActiveProperties struct {
	rendererShellBaseProperties
	Font string `json:"font"`
}

// rendererSidePaneLayoutChangedProperties keeps the captured key order of
// claudeai.epitaxy.side_pane.layout_changed (37 occurrences, all constant).
type rendererSidePaneLayoutChangedProperties struct {
	rendererShellBaseProperties
	OpenTileCount   int    `json:"open_tile_count"`
	RendererSurface string `json:"renderer_surface"`
}

// rendererShellStateEvent is one shell-load event in captured emission order.
type rendererShellStateEvent struct {
	Fact       string
	Properties json.RawMessage
}

// buildRendererShellStateEvents returns the shell-load events in the captured
// order (sidebar state, chat font, side pane layout). appVersion is the
// renderer app_version marker ("1.40609.0"), billingType the account
// subscription type.
func buildRendererShellStateEvents(accountUUID, organizationUUID, billingType, appVersion string) ([]rendererShellStateEvent, error) {
	base := func(version int) rendererShellBaseProperties {
		return rendererShellBaseProperties{
			AccountUUID:      accountUUID,
			OrganizationUUID: organizationUUID,
			BillingType:      billingType,
			Surface:          rendererShellSurface,
			DeploymentMode:   rendererShellDeploymentMode,
			AppVersion:       appVersion,
			Version:          version,
		}
	}
	ordered := []struct {
		fact  string
		value any
	}{
		{FactRendererDesktopSidebarStateSet, rendererDesktopSidebarStateSetProperties{
			rendererShellBaseProperties: base(1),
			IsExpanded:                  true,
			Source:                      rendererShellSidebarSource,
			OtherTabActivity:            rendererShellOtherTabActivity,
		}},
		{FactRendererChatFontActive, rendererChatFontActiveProperties{
			rendererShellBaseProperties: base(2),
			Font:                        rendererShellChatFontDefault,
		}},
		{FactRendererSidePaneLayoutChanged, rendererSidePaneLayoutChangedProperties{
			rendererShellBaseProperties: base(1),
			OpenTileCount:               0,
			RendererSurface:             rendererShellRendererSurface,
		}},
	}
	events := make([]rendererShellStateEvent, 0, len(ordered))
	for _, entry := range ordered {
		encoded, errMarshal := json.Marshal(entry.value)
		if errMarshal != nil {
			return nil, errMarshal
		}
		events = append(events, rendererShellStateEvent{Fact: entry.fact, Properties: encoded})
	}
	return events, nil
}

// rendererShellStateEmitted remembers which account already reported its
// shell load inside an application activation (keyed by the manager's
// activation id and the account), so later sessions of the same account do
// not repeat the shell-load events.
var rendererShellStateEmitted sync.Map

// emitRendererShellStateOnFirstSession is the desktop session-start hook: the
// first session of an account per activation reports the renderer shell state
// (Segment track + Desktop ProductAnalyticsEvent copy for every mapped fact).
func emitRendererShellStateOnFirstSession(ctx context.Context, w *accountWorker, facts RequestFacts) {
	if w == nil || w.manager == nil {
		return
	}
	m := w.manager
	profile, ok := m.auxiliaryProfiles[segmentRole]
	if !ok {
		return
	}
	mapped := false
	for fact := range rendererShellStateEventNames {
		if strings.TrimSpace(profile.Events[fact].EventName) != "" {
			mapped = true
			break
		}
	}
	if !mapped {
		return
	}
	key := m.appSessionID + "\x00" + w.binding.AccountUUID
	if _, seen := rendererShellStateEmitted.LoadOrStore(key, struct{}{}); seen {
		return
	}
	auth := w.authSnapshot()
	events, errBuild := buildRendererShellStateEvents(w.binding.AccountUUID, w.binding.OrganizationUUID, subscriptionType(auth), strings.TrimSuffix(m.bundle.DesktopVersion, ".0"))
	if errBuild != nil {
		log.WithError(errBuild).Warn("claude desktop renderer telemetry: shell-state properties were not encoded")
		return
	}
	for _, event := range events {
		m.emitRendererTrackForAccount(ctx, auth, facts, event.Fact, event.Properties)
	}
}
