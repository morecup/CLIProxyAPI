package telemetry

import (
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

// TestStartupConfig2MetadataMatchesGolden byte-compares the prewait metadata
// (key order and zero-inventory values) and the startup order against the
// golden produced by audit-sdk-telemetry-startup-config2-source.mjs.
func TestStartupConfig2MetadataMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-startup-config2-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		SDKSHA256    string   `json:"sdk_sha256"`
		LoggerPrefix []string `json:"logger_prefix"`
		Events       []struct {
			Fact         string   `json:"fact"`
			EventName    string   `json:"event_name"`
			MirrorRoles  []string `json:"mirror_roles"`
			Order        int      `json:"order"`
			Keys         []string `json:"keys"`
			MetadataJSON string   `json:"metadata_json"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.SDKSHA256 != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(golden.Events) != 1 {
		t.Fatalf("golden is not the reviewed SDK: %s events=%d", golden.SDKSHA256, len(golden.Events))
	}
	if strings.Join(golden.LoggerPrefix, ",") != "subscription_type" {
		t.Fatalf("logger prefix = %v", golden.LoggerPrefix)
	}
	event := golden.Events[0]
	if event.Fact != FactSDKHeadlessMCPPrewait || event.Order != sdkStartupOrderHeadlessMCPPrewait {
		t.Fatalf("golden event %s order %d, gateway %s/%d", event.Fact, event.Order, FactSDKHeadlessMCPPrewait, sdkStartupOrderHeadlessMCPPrewait)
	}
	if executableEvents["sdk-event-logging"][event.Fact] != event.EventName {
		t.Fatalf("%s registered as %q, golden %q", event.Fact, executableEvents["sdk-event-logging"][event.Fact], event.EventName)
	}
	// The golden lists the native datadog-logs mirror role; the gateway does
	// not claim it yet (see sdk_startup_config2.go).
	if strings.Join(event.MirrorRoles, ",") != "datadog-logs" {
		t.Fatalf("golden mirror roles %v", event.MirrorRoles)
	}
	emitted, err := json.Marshal(headlessMCPPrewaitMetadata("pro", sdkHandshakeMCPNonBlocking))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subscription_type":"pro",` + strings.TrimPrefix(event.MetadataJSON, "{")
	if string(emitted) != want {
		t.Fatalf("prewait metadata = %s, golden %s", emitted, want)
	}
	if strings.Join(event.Keys, ",") != "localOnly,willDeferMcp,waitForDeferrable,deadlineMs,pendingBefore,pendingWaitedBefore,toolsBefore,waitedMs,permissionPromptServerPendingBefore,permissionPromptWaitedMs,pendingAfter,pendingWaitedAfter,permissionPromptServerPendingAfter,toolsAfter,mcpNonBlocking" {
		t.Fatalf("golden keys %v", event.Keys)
	}
}

func TestStartupConfig2PrewaitFiresBeforeHandshake(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKHeadlessMCPPrewait] = claudeprofile.TelemetryEventProfile{EventName: "tengu_headless_mcp_prewait"}
		if bundle.AuxiliaryTelemetry.DatadogLogs.Events == nil {
			bundle.AuxiliaryTelemetry.DatadogLogs.Events = map[string]claudeprofile.TelemetryEventProfile{}
		}
		bundle.AuxiliaryTelemetry.DatadogLogs.Events[FactSDKHeadlessMCPPrewait] = claudeprofile.TelemetryEventProfile{EventName: "tengu_headless_mcp_prewait"}
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
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
	datadogMessages := map[string]int{}
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) == nil && len(batch.Events) > 0 {
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
			continue
		}
		var logs []map[string]any
		if json.Unmarshal(request.Body, &logs) == nil {
			for _, entry := range logs {
				if message, _ := entry["message"].(string); message != "" {
					datadogMessages[message]++
				}
			}
		}
	}
	relevant := map[string]bool{"tengu_init": true, "tengu_headless_plugin_install": true, "tengu_headless_mcp_prewait": true, "tengu_sdk_init_handshake": true}
	var ordered []string
	for _, name := range names {
		if relevant[name] {
			ordered = append(ordered, name)
		}
	}
	if got := strings.Join(ordered, ","); got != "tengu_init,tengu_headless_plugin_install,tengu_headless_mcp_prewait,tengu_sdk_init_handshake" {
		t.Fatalf("startup order = %s (all: %s)", got, strings.Join(names, ","))
	}
	wantPayload := `{"subscription_type":"pro","localOnly":false,"willDeferMcp":false,"waitForDeferrable":true,"deadlineMs":2000,"pendingBefore":0,"pendingWaitedBefore":0,"toolsBefore":0,"waitedMs":0,"permissionPromptServerPendingBefore":false,"permissionPromptWaitedMs":0,"pendingAfter":0,"pendingWaitedAfter":0,"permissionPromptServerPendingAfter":false,"toolsAfter":0,"mcpNonBlocking":true}`
	if payloads["tengu_headless_mcp_prewait"] != wantPayload {
		t.Fatalf("prewait payload = %s", payloads["tengu_headless_mcp_prewait"])
	}
	// The datadog-logs mirror is not claimed by this lane; record what the
	// auxiliary batches carried for the report.
	t.Logf("datadog-logs messages observed: %v", datadogMessages)
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		executable[pair.EndpointRole+"/"+pair.EventName] = pair.Executable
	}
	for _, key := range []string{"sdk-event-logging/tengu_headless_mcp_prewait"} {
		if !executable[key] {
			t.Fatalf("%s is emitted but not counted as executable: %+v", key, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("startup-config2 coverage: names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
