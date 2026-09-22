package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type capturedLocalSessionUIEvent struct {
	properties string
	eventType  string
}

func TestDesktopLocalSessionUIRendererContracts(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()

	facts := LocalSessionFacts{
		SessionID:    "local_11111111-1111-4111-8111-111111111111",
		SDKSessionID: "22222222-2222-4222-8222-222222222222",
		QueryID:      "33333333-3333-4333-8333-333333333333",
		IsRunning:    true,
	}
	if err := manager.RecordDesktopSessionUI(auth, "sidebar_session_opened", facts); err != nil {
		t.Fatal(err)
	}
	settled := facts
	settled.EntryCount = 7
	settled.WasHidden = true
	settled.Metrics = map[string]float64{"settled_ms": 43, "first_rows_ms": 41, "skeleton_ms": 7}
	if err := manager.RecordDesktopSessionUI(auth, "transcript_open_settled", settled); err != nil {
		t.Fatal(err)
	}
	pending := facts
	pending.Metrics = map[string]float64{"pending_age_ms": 31004}
	if err := manager.RecordDesktopSessionUI(auth, "pending_turn_stuck_idle", pending); err != nil {
		t.Fatal(err)
	}
	watch := facts
	if err := manager.RecordDesktopSessionWatchDemand(auth, "sessions_watch_demand_suppressed", watch); err != nil {
		t.Fatal(err)
	}
	watch.SuppressedDurationMS = 2345
	if err := manager.RecordDesktopSessionWatchDemand(auth, "sessions_watch_demand_restored", watch); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	segment, desktop := captureLocalSessionUIEvents(t, doer.Requests())
	wantSegment := map[string]string{
		"claudeai.code.sidebar.session_opened":             `{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"is_pinned":false,"section":"recents","session_type":"local","via":"click","is_running":true,"is_unread":false,"group_by":"project","sidebar":"unified"}`,
		"claudeai.epitaxy.transcript.open_settled":         `{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"session_id":"local_11111111-1111-4111-8111-111111111111","session_type":"local","open_kind":"existing","origin":"sidebar","settle_reason":"visible_rows","settled_ms":43,"first_rows_ms":41,"skeleton_ms":7,"cached_messages":false,"entry_count":7,"was_hidden":true,"renderer_surface":"epitaxy"}`,
		"claudeai.epitaxy.session.pending_turn_stuck_idle": `{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"session_id":"local_11111111-1111-4111-8111-111111111111","session_type":"local","pending_age_ms":31004,"meta_is_running":true,"renderer_surface":"epitaxy"}`,
	}
	for event, want := range wantSegment {
		items := segment[event]
		if len(items) != 1 || items[0].eventType != string(rendererCallTrack) || items[0].properties != want {
			t.Fatalf("Segment %s = %+v, want one ordered track with %s", event, items, want)
		}
	}
	if copies := desktop["claudeai.code.sidebar.session_opened"]; len(copies) != 1 || copies[0].eventType != desktopRendererEventType {
		t.Fatalf("sidebar click Desktop copy = %+v, want one ProductAnalyticsEvent", copies)
	} else {
		prefix := strings.TrimSuffix(wantSegment["claudeai.code.sidebar.session_opened"], "}") + `,"_dual_fire":true,"anonymous_id":"`
		const suffix = `","service_name":"claude_ai","path":"/epitaxy"}`
		if !strings.HasPrefix(copies[0].properties, prefix) || !strings.HasSuffix(copies[0].properties, suffix) {
			t.Fatalf("sidebar Desktop copy lost ordered properties or shell path: %s", copies[0].properties)
		}
	}
	for _, event := range []string{
		"claudeai.epitaxy.transcript.open_settled",
		"claudeai.epitaxy.session.pending_turn_stuck_idle",
	} {
		copies := desktop[event]
		if len(copies) != 1 || copies[0].eventType != desktopRendererEventType {
			t.Fatalf("Desktop copies for %s = %+v, want one ProductAnalyticsEvent", event, copies)
		}
		prefix := strings.TrimSuffix(wantSegment[event], "}") + `,"_dual_fire":true,"anonymous_id":"`
		const suffix = `","service_name":"claude_ai","path":"/epitaxy/$sessionId"}`
		if !strings.HasPrefix(copies[0].properties, prefix) || !strings.HasSuffix(copies[0].properties, suffix) {
			t.Fatalf("Desktop copy for %s lost ordered Segment properties or dual-fire suffix: %s", event, copies[0].properties)
		}
	}

	watchProperties := map[string][]string{
		"claudeai.television.sessions_watch.demand_suppressed": {
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"cowork-remote","platform":"desktop"}`,
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"sidebar","platform":"desktop"}`,
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"idle-notifications","platform":"desktop"}`,
		},
		"claudeai.television.sessions_watch.demand_restored": {
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"cowork-remote","platform":"desktop","suppressed_duration_ms":2345,"trigger":"focus"}`,
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"sidebar","platform":"desktop","suppressed_duration_ms":2345,"trigger":"focus"}`,
			`{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"33333333-3333-4333-8333-333333333333","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"watch_tags":"idle-notifications","platform":"desktop","suppressed_duration_ms":2345,"trigger":"focus"}`,
		},
	}
	for event, expected := range watchProperties {
		segmentItems := segment[event]
		desktopItems := desktop[event]
		if len(segmentItems) != len(expected) || len(desktopItems) != len(expected) {
			t.Fatalf("watch demand %s deliveries = Segment %d Desktop %d, want %d each", event, len(segmentItems), len(desktopItems), len(expected))
		}
		for index, want := range expected {
			if segmentItems[index].eventType != string(rendererCallTrack) || segmentItems[index].properties != want {
				t.Fatalf("Segment watch demand %s[%d] = %+v, want %s", event, index, segmentItems[index], want)
			}
			prefix := strings.TrimSuffix(want, "}") + `,"_dual_fire":true,"anonymous_id":"`
			const suffix = `","service_name":"claude_ai","path":"/epitaxy"}`
			if desktopItems[index].eventType != desktopRendererEventType || !strings.HasPrefix(desktopItems[index].properties, prefix) || !strings.HasSuffix(desktopItems[index].properties, suffix) {
				t.Fatalf("Desktop watch demand %s[%d] lost ordered properties or shell path: %s", event, index, desktopItems[index].properties)
			}
		}
	}
}

func TestDesktopLocalSessionWatchContractRejectsUnmeasuredDesktopVersion(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.DesktopVersion = "2.2553.1"
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()

	err := manager.RecordDesktopSessionWatchDemand(auth, "sessions_watch_demand_suppressed", LocalSessionFacts{})
	if err == nil || !strings.Contains(err.Error(), `unsupported Desktop session watch contract for version "2.2553.1"`) {
		t.Fatalf("unmeasured Desktop watcher contract was accepted: %v", err)
	}
	if requests := doer.Requests(); len(requests) != 0 {
		t.Fatalf("unmeasured Desktop watcher contract emitted %d requests", len(requests))
	}
}

func captureLocalSessionUIEvents(t *testing.T, requests []recordedRequest) (map[string][]capturedLocalSessionUIEvent, map[string][]capturedLocalSessionUIEvent) {
	t.Helper()
	segment := make(map[string][]capturedLocalSessionUIEvent)
	desktop := make(map[string][]capturedLocalSessionUIEvent)
	for _, request := range requests {
		var segmentBatch struct {
			Batch []json.RawMessage `json:"batch"`
		}
		if err := json.Unmarshal(request.Body, &segmentBatch); err == nil && len(segmentBatch.Batch) > 0 {
			for _, raw := range segmentBatch.Batch {
				var item struct {
					Event      string          `json:"event"`
					Type       string          `json:"type"`
					Properties json.RawMessage `json:"properties"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				if item.Event != "" {
					segment[item.Event] = append(segment[item.Event], capturedLocalSessionUIEvent{properties: string(item.Properties), eventType: item.Type})
				}
			}
		}

		var desktopBatch struct {
			Events []struct {
				EventType string `json:"event_type"`
				EventData struct {
					EventName  string `json:"event_name"`
					Properties string `json:"properties"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &desktopBatch); err == nil {
			for _, item := range desktopBatch.Events {
				if item.EventData.EventName != "" {
					desktop[item.EventData.EventName] = append(desktop[item.EventData.EventName], capturedLocalSessionUIEvent{properties: item.EventData.Properties, eventType: item.EventType})
				}
			}
		}
	}
	return segment, desktop
}
