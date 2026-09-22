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

func TestStartupConfigMetadataMatchesNativeShape(t *testing.T) {
	cases := []struct {
		name string
		got  any
		want string
	}{
		{"plugin_skills_dir_loaded", pluginSkillsDirLoadedMetadata("pro"), `{"subscription_type":"pro","count":0,"user_count":0,"project_count":0,"project_suppressed_count":0,"error_count":0}`},
		{"headless_plugin_install", headlessPluginInstallMetadata("pro"), `{"subscription_type":"pro","marketplaces_installed":0,"delisted_count":0}`},
		{"claudemd_initial_load", claudeMDInitialLoadMetadata("pro"), `{"subscription_type":"pro","file_count":0,"total_content_length":0,"user_count":0,"project_count":0,"local_count":0,"managed_count":0,"automem_count":0,"duration_ms":0}`},
	}
	eligibility, ok := claudeAIMCPEligibilityMetadata("pro", "user:inference")
	if !ok {
		t.Fatal("inference-only scope must settle at missing_scope")
	}
	cases = append(cases, struct {
		name string
		got  any
		want string
	}{"claudeai_mcp_eligibility", eligibility, `{"subscription_type":"pro","state":"missing_scope"}`})
	if _, ok := claudeAIMCPEligibilityMetadata("pro", "user:inference user:mcp_servers"); ok {
		t.Fatal("a token with user:mcp_servers needs the live fetch; the gateway must not pick a state")
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Fatalf("%s metadata = %s, want %s", tc.name, raw, tc.want)
		}
	}
}

// TestStartupConfigMetadataMatchesGolden byte-compares the emitted metadata
// (key order and zero-inventory values) and the startup orders against the
// native golden produced by audit-sdk-telemetry-startup-config-source.mjs.
func TestStartupConfigMetadataMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-startup-config-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		SDKSHA256    string   `json:"sdk_sha256"`
		LoggerPrefix []string `json:"logger_prefix"`
		Events       []struct {
			Fact         string   `json:"fact"`
			EventName    string   `json:"event_name"`
			Order        int      `json:"order"`
			Keys         []string `json:"keys"`
			MetadataJSON string   `json:"metadata_json"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.SDKSHA256 != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(golden.Events) != 3 {
		t.Fatalf("golden is not the reviewed SDK: %s events=%d", golden.SDKSHA256, len(golden.Events))
	}
	if strings.Join(golden.LoggerPrefix, ",") != "subscription_type" {
		t.Fatalf("logger prefix = %v", golden.LoggerPrefix)
	}
	builders := map[string]func(string) any{
		FactSDKPluginSkillsDirLoaded: func(s string) any { return pluginSkillsDirLoadedMetadata(s) },
		FactSDKHeadlessPluginInstall: func(s string) any { return headlessPluginInstallMetadata(s) },
		FactSDKClaudeMDInitialLoad:   func(s string) any { return claudeMDInitialLoadMetadata(s) },
	}
	orders := map[string]int{
		FactSDKPluginSkillsDirLoaded: sdkStartupOrderPluginSkillsDirLoaded,
		FactSDKHeadlessPluginInstall: sdkStartupOrderHeadlessPluginInstall,
		FactSDKClaudeMDInitialLoad:   sdkStartupOrderClaudeMDInitialLoad,
	}
	for _, event := range golden.Events {
		build, ok := builders[event.Fact]
		if !ok {
			t.Fatalf("golden fact %q has no gateway builder", event.Fact)
		}
		if executableEvents["sdk-event-logging"][event.Fact] != event.EventName {
			t.Fatalf("%s registered as %q, golden %q", event.Fact, executableEvents["sdk-event-logging"][event.Fact], event.EventName)
		}
		if orders[event.Fact] != event.Order {
			t.Fatalf("%s startup order %d, golden %d", event.Fact, orders[event.Fact], event.Order)
		}
		emitted, err := json.Marshal(build("pro"))
		if err != nil {
			t.Fatal(err)
		}
		// The native logger prepends subscription_type to the event's own
		// metadata object; the golden carries the object as the SDK builds it.
		want := `{"subscription_type":"pro",` + strings.TrimPrefix(event.MetadataJSON, "{")
		if string(emitted) != want {
			t.Fatalf("%s metadata = %s, golden %s", event.Fact, emitted, want)
		}
		var keys []string
		decoder := json.NewDecoder(strings.NewReader(event.MetadataJSON))
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				t.Fatal(err)
			}
			if key, ok := token.(string); ok && len(keys) < len(event.Keys) && key == event.Keys[len(keys)] {
				keys = append(keys, key)
			}
		}
		if strings.Join(keys, ",") != strings.Join(event.Keys, ",") {
			t.Fatalf("%s golden keys %v disagree with metadata_json %s", event.Fact, event.Keys, event.MetadataJSON)
		}
	}
}

func TestStartupConfigEventsFireFromRuntimeStart(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKPluginSkillsDirLoaded] = claudeprofile.TelemetryEventProfile{EventName: "tengu_plugin_skills_dir_loaded"}
		bundle.SDKTelemetry.Events[FactSDKHeadlessPluginInstall] = claudeprofile.TelemetryEventProfile{EventName: "tengu_headless_plugin_install"}
		bundle.SDKTelemetry.Events[FactSDKClaudeMDInitialLoad] = claudeprofile.TelemetryEventProfile{EventName: "tengu_claudemd__initial_load"}
		bundle.SDKTelemetry.Events[FactSDKClaudeAIMCPEligibility] = claudeprofile.TelemetryEventProfile{EventName: "tengu_claudeai_mcp_eligibility"}
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
	// Other lanes register further startup events; assert only the relative
	// order of this lane's events against the three core startup events.
	relevant := map[string]bool{"tengu_started": true, "tengu_init": true, "tengu_sdk_init_handshake": true,
		"tengu_plugin_skills_dir_loaded": true, "tengu_headless_plugin_install": true, "tengu_claudemd__initial_load": true, "tengu_claudeai_mcp_eligibility": true}
	var ordered []string
	for _, name := range names {
		if relevant[name] {
			ordered = append(ordered, name)
		}
	}
	startup := strings.Join(ordered, ",")
	wantOrder := "tengu_started,tengu_plugin_skills_dir_loaded,tengu_claudeai_mcp_eligibility,tengu_init,tengu_headless_plugin_install,tengu_sdk_init_handshake,tengu_claudemd__initial_load"
	if startup != wantOrder {
		t.Fatalf("startup order = %s (all: %s), want %s", startup, strings.Join(names, ","), wantOrder)
	}
	if payloads["tengu_plugin_skills_dir_loaded"] != `{"subscription_type":"pro","count":0,"user_count":0,"project_count":0,"project_suppressed_count":0,"error_count":0}` {
		t.Fatalf("skills dir payload = %s", payloads["tengu_plugin_skills_dir_loaded"])
	}
	if payloads["tengu_headless_plugin_install"] != `{"subscription_type":"pro","marketplaces_installed":0,"delisted_count":0}` {
		t.Fatalf("plugin install payload = %s", payloads["tengu_headless_plugin_install"])
	}
	if payloads["tengu_claudeai_mcp_eligibility"] != `{"subscription_type":"pro","state":"missing_scope"}` {
		t.Fatalf("eligibility payload = %s", payloads["tengu_claudeai_mcp_eligibility"])
	}
	if payloads["tengu_claudemd__initial_load"] != `{"subscription_type":"pro","file_count":0,"total_content_length":0,"user_count":0,"project_count":0,"local_count":0,"managed_count":0,"automem_count":0,"duration_ms":0}` {
		t.Fatalf("claudemd payload = %s", payloads["tengu_claudemd__initial_load"])
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" {
			executable[pair.EventName] = pair.Executable
		}
	}
	for _, name := range []string{"tengu_plugin_skills_dir_loaded", "tengu_headless_plugin_install", "tengu_claudemd__initial_load", "tengu_claudeai_mcp_eligibility"} {
		if !executable[name] {
			t.Fatalf("%s is emitted but not counted as executable: %+v", name, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("startup-config coverage: names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
