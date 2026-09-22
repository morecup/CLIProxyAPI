package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
)

type capturedRendererWatchEvent struct {
	properties  string
	anonymousID string
}

func captureRendererWatchEvents(t *testing.T, requests []recordedRequest) (map[string]capturedRendererWatchEvent, map[string]string) {
	t.Helper()
	segment := make(map[string]capturedRendererWatchEvent)
	desktop := make(map[string]string)
	for _, request := range requests {
		if strings.HasPrefix(request.URL, "https://claude.ai/") {
			var batch struct {
				Events []struct {
					EventData struct {
						EventName  string `json:"event_name"`
						Properties string `json:"properties"`
					} `json:"event_data"`
				} `json:"events"`
			}
			if err := json.Unmarshal(request.Body, &batch); err != nil {
				t.Fatal(err)
			}
			for _, event := range batch.Events {
				desktop[event.EventData.EventName] = event.EventData.Properties
			}
			continue
		}
		var batch struct {
			Batch []struct {
				Event       string          `json:"event"`
				Properties  json.RawMessage `json:"properties"`
				AnonymousID string          `json:"anonymousId"`
			} `json:"batch"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			continue
		}
		for _, item := range batch.Batch {
			if item.Event != "" {
				segment[item.Event] = capturedRendererWatchEvent{properties: string(item.Properties), anonymousID: item.AnonymousID}
			}
		}
	}
	return segment, desktop
}

func TestRendererWatchEventsUseCapturedPropertiesAndDualFirePath(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	auth.Metadata["subscription_type"] = "stripe_subscription"
	batch := SessionHeartbeatBatch{Sent: 8, ProbeDispatched: 7, NoWorker: 1}
	if err := manager.RecordDesktopSessionHeartbeatBatch(auth, batch); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveSessionsWatchRetry(auth, 502); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	segment, desktop := captureRendererWatchEvents(t, doer.Requests())
	tests := []struct {
		name string
		keys []string
		want map[string]any
	}{
		{
			name: "claudeai.code.sessions.heartbeat_check_batch",
			keys: []string{"account_uuid", "organization_uuid", "billing_type", "surface", "deployment_mode", "app_version", "version", "trigger", "sent", "fresh", "probe_dispatched", "no_worker", "recently_checked", "unknown"},
			want: map[string]any{"trigger": "tick", "sent": float64(8), "fresh": float64(0), "probe_dispatched": float64(7), "no_worker": float64(1), "recently_checked": float64(0), "unknown": float64(0)},
		},
		{
			name: "claudeai.television.sessions_watch.retry_loop",
			keys: []string{"account_uuid", "organization_uuid", "billing_type", "surface", "deployment_mode", "app_version", "version", "http_status", "detail", "watch_tags"},
			want: map[string]any{"http_status": float64(502), "detail": "sessions/watch 502 ", "watch_tags": rendererSessionsWatchRetryTags},
		},
	}
	for _, test := range tests {
		item, ok := segment[test.name]
		if !ok {
			t.Fatalf("Segment event %q missing: %v", test.name, segment)
		}
		if got := orderedJSONKeys(t, []byte(item.properties)); strings.Join(got, ",") != strings.Join(test.keys, ",") {
			t.Fatalf("%s keys = %v, want %v", test.name, got, test.keys)
		}
		var properties map[string]any
		if err := json.Unmarshal([]byte(item.properties), &properties); err != nil {
			t.Fatal(err)
		}
		for key, want := range test.want {
			if properties[key] != want {
				t.Fatalf("%s.%s = %#v, want %#v", test.name, key, properties[key], want)
			}
		}
		if properties["account_uuid"] != testAccountA || properties["organization_uuid"] != testOrgA || properties["billing_type"] != "stripe_subscription" || properties["app_version"] != "1.40609.0" {
			t.Fatalf("%s identity = %v", test.name, properties)
		}
		wantCopy := strings.TrimSuffix(item.properties, "}") + `,"_dual_fire":true,"anonymous_id":"` + item.anonymousID + `","service_name":"claude_ai","path":"/epitaxy"}`
		if desktop[test.name] != wantCopy {
			t.Fatalf("Desktop copy of %s = %s, want %s", test.name, desktop[test.name], wantCopy)
		}
	}
}

func TestRendererWatchContractsRejectCallerCountsAndUnknownVersions(t *testing.T) {
	manager := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := manager.RecordDesktopSessionHeartbeatBatch(auth, SessionHeartbeatBatch{Sent: 8, ProbeDispatched: 8, NoWorker: 1}); err == nil {
		t.Fatal("inconsistent caller heartbeat counters were accepted")
	}
	manager.bundle.DesktopVersion = "2.2553.1"
	if err := manager.RecordDesktopSessionHeartbeatBatch(auth, SessionHeartbeatBatch{}); err == nil {
		t.Fatal("unknown Desktop version inherited heartbeat contract")
	}
	if err := manager.ObserveSessionsWatchRetry(auth, 502); err == nil {
		t.Fatal("unknown Desktop version inherited retry-loop contract")
	}
}
