package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUpdateCheckProjectsApplicationFactsWithoutRequestOrSession(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	authA := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	authB := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	if err := manager.ObserveUpdateCheck(authB, FactUpdateNotAvailable); err == nil {
		t.Fatal("orphan outcome accepted")
	}
	if err := manager.ObserveUpdateCheck(authA, FactRequestStarted); err == nil {
		t.Fatal("unrelated fact accepted")
	}
	for _, auth := range []*cliproxyauth.Auth{authA, authB} {
		for _, fact := range []string{FactUpdateCheckStarted, FactUpdateCheckStarted, FactUpdateNotAvailable, FactUpdateNotAvailable} {
			if err := manager.ObserveUpdateCheck(auth, fact); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Millisecond)
		}
	}
	if len(manager.sessions) != 0 {
		t.Fatal("application event fabricated a user session")
	}
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	requests := doer.Requests()
	if len(requests) != 2 {
		t.Fatalf("isolated renderer batches=%d want 2", len(requests))
	}
	seen := map[string]bool{}
	for _, request := range requests {
		if !strings.HasPrefix(request.URL, "https://claude.ai/") {
			t.Fatalf("wrong endpoint: %s", request.URL)
		}
		// Use explicit tags for wire event names; all identity values are synthetic.
		var wire struct {
			Events []struct {
				EventData struct {
					Name      string `json:"event_name"`
					Timestamp string `json:"timestamp"`
					Metadata  string `json:"metadata"`
					Auth      struct {
						AccountUUID string `json:"account_uuid"`
					} `json:"auth"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.Events) != 2 {
			t.Fatalf("events=%d want 2", len(wire.Events))
		}
		for i, event := range wire.Events {
			if want := []string{"desktop_update_check_started", "desktop_update_not_available"}[i]; event.EventData.Name != want {
				t.Fatalf("event=%s want %s", event.EventData.Name, want)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(event.EventData.Metadata), &metadata); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]any{"current_version": "1.40609.0", "app_version": "1.40609.0", "update_channel": "production", "is_manual": false, "app_session_id": manager.appSessionID} {
				if metadata[key] != want {
					t.Fatalf("%s=%v want %v", key, metadata[key], want)
				}
			}
			for _, key := range []string{"renderer_surface", "backend_kind", "is_ssh", "session_id", "cli_session_id", "user_message_uuid", "model", "process_main_rss_bytes", "window_count"} {
				if _, ok := metadata[key]; ok {
					t.Fatalf("fabricated application metadata %s", key)
				}
			}
			if event.EventData.Auth.AccountUUID != wire.Events[0].EventData.Auth.AccountUUID {
				t.Fatal("batch crossed accounts")
			}
		}
		seen[wire.Events[0].EventData.Auth.AccountUUID] = true
	}
	if !seen[testAccountA] || !seen[testAccountB] {
		t.Fatalf("account batches=%v", seen)
	}
	manager.Quarantine()
	if err := manager.ObserveUpdateCheck(authA, FactUpdateCheckStarted); err == nil {
		t.Fatal("stopped runtime accepted a check")
	}
}
