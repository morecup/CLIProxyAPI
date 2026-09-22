package telemetry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestStartupSessionMetadataMatchesNativeGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-startup-session-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Executed struct {
			ConcurrentSessionsCases []struct {
				Counted int             `json:"num_sessions_counted"`
				Event   json.RawMessage `json:"event"`
			} `json:"concurrent_sessions_cases"`
			TimerStartupCases []struct {
				Label string `json:"label"`
				Event struct {
					Name     string          `json:"name"`
					Metadata json.RawMessage `json:"metadata"`
				} `json:"event"`
			} `json:"timer_startup_cases"`
		} `json:"executed"`
		StartupOrder []struct {
			Event string `json:"event"`
			Order int    `json:"order"`
		} `json:"startup_order"`
		KeyOrder map[string][]string `json:"key_order"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	compact := func(raw json.RawMessage) string {
		var buffer bytes.Buffer
		if err := json.Compact(&buffer, raw); err != nil {
			t.Fatal(err)
		}
		return buffer.String()
	}
	// Native builders ran without the logger prefix; compare with an empty
	// subscription so the gateway struct order is checked byte for byte.
	if len(golden.Executed.ConcurrentSessionsCases) != 3 {
		t.Fatalf("golden concurrent cases = %d", len(golden.Executed.ConcurrentSessionsCases))
	}
	for _, c := range golden.Executed.ConcurrentSessionsCases {
		metadata, ok := concurrentSessionsMetadata("", c.Counted)
		if string(c.Event) == "null" {
			if ok {
				t.Fatalf("count %d must not emit natively-gated event", c.Counted)
			}
			continue
		}
		var event struct {
			Name     string          `json:"name"`
			Metadata json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal(c.Event, &event); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(metadata)
		if !ok || event.Name != "tengu_concurrent_sessions" || string(encoded) != compact(event.Metadata) {
			t.Fatalf("count %d metadata = %s ok=%v, native %s", c.Counted, encoded, ok, event.Metadata)
		}
	}
	found := false
	for _, c := range golden.Executed.TimerStartupCases {
		if c.Label != "fresh_no_mcp" {
			continue
		}
		found = true
		encoded, _ := json.Marshal(timerStartupMetadata("", 1235, 0, false))
		if c.Event.Name != "tengu_timer" || string(encoded) != compact(c.Event.Metadata) {
			t.Fatalf("timer metadata = %s, native %s", encoded, c.Event.Metadata)
		}
	}
	if !found {
		t.Fatal("golden lacks the fresh_no_mcp timer case")
	}
	orders := map[string]int{}
	for _, entry := range golden.StartupOrder {
		orders[entry.Event] = entry.Order
	}
	if orders["tengu_concurrent_sessions"] != sdkStartupOrderConcurrentSessions || orders["tengu_timer"] != sdkStartupOrderTimerStartup {
		t.Fatalf("startup orders %v do not match the registered orders", orders)
	}
	if !(orders["tengu_shell_allow_rules_at_init"] < orders["tengu_concurrent_sessions"] && orders["tengu_concurrent_sessions"] < orders["tengu_timer"] && orders["tengu_timer"] < orders["tengu_sdk_init_handshake"]) {
		t.Fatalf("golden startup order %v contradicts the pinned placement", orders)
	}
	if strings.Join(golden.KeyOrder["tengu_timer_startup"], ",") != "event,durationMs,mcpNonBlocking,mcpClientCount,resumed" {
		t.Fatalf("golden timer key order = %v", golden.KeyOrder["tengu_timer_startup"])
	}
	withSubscription, _ := json.Marshal(timerStartupMetadata("pro", -5, 0, false))
	if string(withSubscription) != `{"subscription_type":"pro","event":"startup","durationMs":0,"mcpNonBlocking":true,"mcpClientCount":0,"resumed":false}` {
		t.Fatalf("prefixed timer metadata = %s", withSubscription)
	}
	concurrent, _ := concurrentSessionsMetadata("pro", 2)
	withSubscription, _ = json.Marshal(concurrent)
	if string(withSubscription) != `{"subscription_type":"pro","num_sessions":2}` {
		t.Fatalf("prefixed concurrent metadata = %s", withSubscription)
	}
}

func startupSessionTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKConcurrentSessions] = claudeprofile.TelemetryEventProfile{EventName: "tengu_concurrent_sessions"}
		bundle.SDKTelemetry.Events[FactSDKTimer] = claudeprofile.TelemetryEventProfile{EventName: "tengu_timer"}
		if bundle.AuxiliaryTelemetry.DatadogLogs.Events == nil {
			bundle.AuxiliaryTelemetry.DatadogLogs.Events = map[string]claudeprofile.TelemetryEventProfile{}
		}
		bundle.AuxiliaryTelemetry.DatadogLogs.Events[FactSDKTimer] = claudeprofile.TelemetryEventProfile{EventName: "tengu_timer"}
		// Startup events registered by other topic files fire on the same
		// session start; map them so the shared activation does not fail.
		for _, event := range sdkStartupEvents {
			if _, mapped := bundle.SDKTelemetry.Events[event.Fact]; !mapped {
				bundle.SDKTelemetry.Events[event.Fact] = claudeprofile.TelemetryEventProfile{EventName: "tengu_" + event.Fact}
			}
		}
	})
}

func startupSessionBatch(t *testing.T, doer *testDoer) ([]string, map[string]string) {
	t.Helper()
	var names []string
	payloads := map[string]string{}
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			continue
		}
		for _, wrapper := range batch.Events {
			data := wrapper.EventData
			if !strings.HasPrefix(data.EventName, "tengu_") {
				continue
			}
			names = append(names, data.EventName)
			raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			if _, seen := payloads[data.EventName]; !seen {
				payloads[data.EventName] = string(raw)
			}
		}
	}
	return names, payloads
}

func TestStartupSessionEventsFireFromRuntimeStart(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	baseline := liveEmulatedSDKSessionCount()
	doerA := &testDoer{}
	first := startupSessionTestManager(t, clock, doerA)
	if got := liveEmulatedSDKSessionCount(); got != baseline+1 {
		t.Fatalf("live emulated sessions = %d, want %d", got, baseline+1)
	}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	// The emulated process has been up for 1.5 s when the session starts.
	clock.now = clock.now.Add(1500 * time.Millisecond)
	if err := first.Activate(auth); err != nil {
		t.Fatal(err)
	}
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	names, payloads := startupSessionBatch(t, doerA)
	position := map[string]int{}
	for index, name := range names {
		if _, seen := position[name]; !seen {
			position[name] = index
		}
	}
	for _, name := range []string{"tengu_started", "tengu_init", "tengu_shell_allow_rules_at_init", "tengu_timer", "tengu_sdk_init_handshake"} {
		if _, ok := position[name]; !ok {
			t.Fatalf("%s missing from the startup batch: %s", name, strings.Join(names, ","))
		}
	}
	if !(position["tengu_started"] < position["tengu_init"] && position["tengu_init"] < position["tengu_shell_allow_rules_at_init"] && position["tengu_shell_allow_rules_at_init"] < position["tengu_timer"] && position["tengu_timer"] < position["tengu_sdk_init_handshake"]) {
		t.Fatalf("startup order = %s", strings.Join(names, ","))
	}
	if payloads["tengu_timer"] != `{"subscription_type":"pro","event":"startup","durationMs":1500,"mcpNonBlocking":true,"mcpClientCount":0,"resumed":false}` {
		t.Fatalf("timer payload = %s", payloads["tengu_timer"])
	}
	if baseline == 0 {
		if _, emitted := position["tengu_concurrent_sessions"]; emitted {
			t.Fatalf("a lone emulated session must not report concurrent sessions: %s", payloads["tengu_concurrent_sessions"])
		}
	} else {
		t.Logf("other live managers present (%d); lone-session branch covered by the golden gate test", baseline)
	}

	// A second emulated process on the same host: its own startup counts
	// both live sessions, and the count is placed after the allow-rule report
	// and before the startup timer.
	doerB := &testDoer{}
	second := startupSessionTestManager(t, clock, doerB)
	live := liveEmulatedSDKSessionCount()
	if live < 2 || live != baseline+2 {
		t.Fatalf("live emulated sessions = %d, want %d", live, baseline+2)
	}
	if err := second.Activate(auth); err != nil {
		t.Fatal(err)
	}
	if err := second.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	names, payloads = startupSessionBatch(t, doerB)
	position = map[string]int{}
	for index, name := range names {
		if _, seen := position[name]; !seen {
			position[name] = index
		}
	}
	if _, ok := position["tengu_concurrent_sessions"]; !ok {
		t.Fatalf("tengu_concurrent_sessions missing from the second startup batch: %s", strings.Join(names, ","))
	}
	if !(position["tengu_shell_allow_rules_at_init"] < position["tengu_concurrent_sessions"] && position["tengu_concurrent_sessions"] < position["tengu_timer"] && position["tengu_timer"] < position["tengu_sdk_init_handshake"]) {
		t.Fatalf("second startup order = %s", strings.Join(names, ","))
	}
	var concurrent struct {
		SubscriptionType string `json:"subscription_type"`
		NumSessions      int    `json:"num_sessions"`
	}
	if err := json.Unmarshal([]byte(payloads["tengu_concurrent_sessions"]), &concurrent); err != nil {
		t.Fatal(err)
	}
	if concurrent.SubscriptionType != "pro" || concurrent.NumSessions != live {
		t.Fatalf("concurrent payload = %s, want num_sessions %d", payloads["tengu_concurrent_sessions"], live)
	}
	if !strings.HasPrefix(payloads["tengu_concurrent_sessions"], `{"subscription_type":"pro","num_sessions":`) {
		t.Fatalf("concurrent payload order = %s", payloads["tengu_concurrent_sessions"])
	}
	// Closing a manager removes its emulated process from the host count.
	second.Close()
	if got := liveEmulatedSDKSessionCount(); got != live-1 {
		t.Fatalf("live emulated sessions after close = %d, want %d", got, live-1)
	}

	coverage := first.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" || pair.EndpointRole == datadogLogsRole {
			executable[pair.EndpointRole+"/"+pair.EventName] = pair.Executable
		}
	}
	for _, key := range []string{"sdk-event-logging/tengu_concurrent_sessions", "sdk-event-logging/tengu_timer", datadogLogsRole + "/tengu_timer"} {
		if !executable[key] {
			t.Fatalf("%s is emitted but not counted as executable: %+v", key, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("startup-session coverage: names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
