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

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestStartupCoreMetadataMatchesNativeGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-startup-core-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Executed struct {
			ShellAllowRulesCases []struct {
				Label string `json:"label"`
				Event struct {
					Name     string          `json:"name"`
					Metadata json.RawMessage `json:"metadata"`
				} `json:"event"`
			} `json:"shell_allow_rules_cases"`
			ShellSetCwdEvent struct {
				Name     string          `json:"name"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"shell_set_cwd_event"`
		} `json:"executed"`
		StartupOrder []struct {
			Event string `json:"event"`
			Order int    `json:"order"`
		} `json:"startup_order"`
		CLIFlags struct {
			Verdict          string   `json:"verdict"`
			SessionDependent []string `json:"session_dependent"`
		} `json:"cli_flags"`
		ConcurrentSessions struct {
			Verdict string `json:"verdict"`
		} `json:"concurrent_sessions"`
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
	// Native builders ran without the logger prefix; the gateway adds
	// subscription_type in front, so compare with an empty subscription.
	setCwd, err := json.Marshal(shellSetCwdMetadata(""))
	if err != nil {
		t.Fatal(err)
	}
	if golden.Executed.ShellSetCwdEvent.Name != "tengu_shell_set_cwd" || string(setCwd) != compact(golden.Executed.ShellSetCwdEvent.Metadata) {
		t.Fatalf("shell_set_cwd metadata = %s, native %s", setCwd, golden.Executed.ShellSetCwdEvent.Metadata)
	}
	found := false
	for _, c := range golden.Executed.ShellAllowRulesCases {
		if c.Label != "empty_context" {
			continue
		}
		found = true
		allow, err := json.Marshal(shellAllowRulesAtInitMetadata(""))
		if err != nil {
			t.Fatal(err)
		}
		if c.Event.Name != "tengu_shell_allow_rules_at_init" || string(allow) != compact(c.Event.Metadata) {
			t.Fatalf("shell_allow_rules_at_init metadata = %s, native %s", allow, c.Event.Metadata)
		}
	}
	if !found {
		t.Fatal("golden lacks the empty_context allow-rule case")
	}
	orders := map[string]int{}
	for _, entry := range golden.StartupOrder {
		orders[entry.Event] = entry.Order
	}
	if orders["tengu_shell_set_cwd"] != sdkStartupOrderShellSetCwd || orders["tengu_shell_allow_rules_at_init"] != sdkStartupOrderShellAllowRulesAtInit {
		t.Fatalf("startup orders %v do not match the registered orders", orders)
	}
	if !(orders["tengu_shell_set_cwd"] < orders["tengu_started"] && orders["tengu_init"] < orders["tengu_shell_allow_rules_at_init"] && orders["tengu_shell_allow_rules_at_init"] < orders["tengu_sdk_init_handshake"]) {
		t.Fatalf("golden startup order %v contradicts the pinned placement", orders)
	}
	// tengu_cli_flags stays a boundary: the golden records why, and no topic
	// file may register it as a startup event. tengu_concurrent_sessions is
	// executable since lane W2 (sdk_startup_session.go counts the live
	// emulated SDK processes of this gateway host); this golden keeps the
	// earlier per-machine-registry verdict as history.
	if !strings.HasPrefix(golden.CLIFlags.Verdict, "boundary") || len(golden.CLIFlags.SessionDependent) == 0 {
		t.Fatalf("golden verdict changed: cli_flags=%q", golden.CLIFlags.Verdict)
	}
	for _, event := range sdkStartupEvents {
		if event.Fact == "cli_flags" {
			t.Fatalf("%s is registered as a startup event but its native value depends on state the gateway cannot observe", event.Fact)
		}
	}
	withSubscription, _ := json.Marshal(shellSetCwdMetadata("pro"))
	if string(withSubscription) != `{"subscription_type":"pro","success":true}` {
		t.Fatalf("prefixed shell_set_cwd metadata = %s", withSubscription)
	}
	withSubscription, _ = json.Marshal(shellAllowRulesAtInitMetadata("pro"))
	if string(withSubscription) != `{"subscription_type":"pro","total_shell_allow_rules":0}` {
		t.Fatalf("prefixed shell_allow_rules_at_init metadata = %s", withSubscription)
	}
}

func TestStartupCoreEventsFireFromRuntimeStart(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKShellSetCwd] = claudeprofile.TelemetryEventProfile{EventName: "tengu_shell_set_cwd"}
		bundle.SDKTelemetry.Events[FactSDKShellAllowRulesAtInit] = claudeprofile.TelemetryEventProfile{EventName: "tengu_shell_allow_rules_at_init"}
		// Startup events registered by other topic files fire on the same
		// session start; map them so the shared activation does not fail on
		// an unmapped fact (their payloads are not asserted here).
		for _, event := range sdkStartupEvents {
			if _, mapped := bundle.SDKTelemetry.Events[event.Fact]; !mapped {
				bundle.SDKTelemetry.Events[event.Fact] = claudeprofile.TelemetryEventProfile{EventName: "tengu_" + event.Fact}
			}
		}
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	// Activate emulates the SDK session start (ensureSDKRuntimeStarted).
	if err := m.Activate(auth); err != nil {
		t.Fatal(err)
	}
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleMain, SessionID: uuid.NewString(), PromptID: uuid.NewString(), Model: "claude-opus-5"})
	if !span.Active() {
		t.Fatal("main span inactive")
	}
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	payloads := map[string]string{}
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		_ = json.Unmarshal(request.Body, &batch)
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
			payloads[data.EventName] = string(raw)
		}
	}
	// Other startup topics register their own events; only the relative
	// order of the core events and this lane's two events is asserted.
	position := map[string]int{}
	for index, name := range names {
		if _, seen := position[name]; !seen {
			position[name] = index
		}
	}
	for _, name := range []string{"tengu_shell_set_cwd", "tengu_started", "tengu_init", "tengu_shell_allow_rules_at_init", "tengu_sdk_init_handshake"} {
		if _, ok := position[name]; !ok {
			t.Fatalf("%s missing from the startup batch: %s", name, strings.Join(names, ","))
		}
	}
	if !(position["tengu_shell_set_cwd"] < position["tengu_started"] && position["tengu_init"] < position["tengu_shell_allow_rules_at_init"] && position["tengu_shell_allow_rules_at_init"] < position["tengu_sdk_init_handshake"]) {
		t.Fatalf("startup order = %s", strings.Join(names, ","))
	}
	if payloads["tengu_shell_set_cwd"] != `{"subscription_type":"pro","success":true}` {
		t.Fatalf("shell_set_cwd payload = %s", payloads["tengu_shell_set_cwd"])
	}
	if payloads["tengu_shell_allow_rules_at_init"] != `{"subscription_type":"pro","total_shell_allow_rules":0}` {
		t.Fatalf("shell_allow_rules_at_init payload = %s", payloads["tengu_shell_allow_rules_at_init"])
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" {
			executable[pair.EventName] = pair.Executable
		}
	}
	for _, name := range []string{"tengu_shell_set_cwd", "tengu_shell_allow_rules_at_init"} {
		if !executable[name] {
			t.Fatalf("%s is emitted but not counted as executable: %+v", name, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("startup-core coverage: names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
