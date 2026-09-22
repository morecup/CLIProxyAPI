package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type desktopProcessGolden struct {
	ExecutableEvents []struct {
		EventName   string   `json:"event_name"`
		GatewayFact string   `json:"gateway_fact"`
		PayloadKeys []string `json:"payload_keys"`
		Probes      []struct {
			Resolution string                 `json:"resolution"`
			Emitted    bool                   `json:"emitted"`
			Metadata   map[string]interface{} `json:"metadata"`
		} `json:"probes"`
		TargetVersionFixture string `json:"target_version_fixture"`
	} `json:"executable_events"`
}

func loadDesktopProcessGolden(t *testing.T) desktopProcessGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-process-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden desktopProcessGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func TestDesktopBinaryResolvedMetadataMatchesNativeShape(t *testing.T) {
	golden := loadDesktopProcessGolden(t)
	var pinned *struct {
		EventName   string   `json:"event_name"`
		GatewayFact string   `json:"gateway_fact"`
		PayloadKeys []string `json:"payload_keys"`
		Probes      []struct {
			Resolution string                 `json:"resolution"`
			Emitted    bool                   `json:"emitted"`
			Metadata   map[string]interface{} `json:"metadata"`
		} `json:"probes"`
		TargetVersionFixture string `json:"target_version_fixture"`
	}
	for index := range golden.ExecutableEvents {
		if golden.ExecutableEvents[index].EventName == "desktop_ccd_binary_resolved" {
			pinned = &golden.ExecutableEvents[index]
		}
	}
	if pinned == nil || pinned.GatewayFact != FactDesktopBinaryResolved {
		t.Fatalf("golden does not pin desktop_ccd_binary_resolved as %s: %+v", FactDesktopBinaryResolved, golden)
	}
	metadata := desktopBinaryResolvedMetadata{Resolution: desktopBinaryResolutionRequiredVersion, ResolvedVersion: pinned.TargetVersionFixture, RequiredVersion: pinned.TargetVersionFixture}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"resolution":"required_version","resolved_version":"` + pinned.TargetVersionFixture + `","required_version":"` + pinned.TargetVersionFixture + `"}`
	if string(raw) != want {
		t.Fatalf("binary resolved metadata = %s, want native order %s", raw, want)
	}
	if strings.Join(pinned.PayloadKeys, ",") != "resolution,resolved_version,required_version" {
		t.Fatalf("golden payload keys = %v", pinned.PayloadKeys)
	}
	var probe map[string]interface{}
	for _, candidate := range pinned.Probes {
		if candidate.Resolution == desktopBinaryResolutionRequiredVersion && candidate.Emitted {
			probe = candidate.Metadata
		}
	}
	if probe == nil {
		t.Fatal("golden has no vm probe for required_version")
	}
	for key, value := range metadata.toMap() {
		if probe[key] != value {
			t.Fatalf("native probe %s = %v, gateway = %v", key, probe[key], value)
		}
	}
	if _, ok := (&Manager{}).binaryResolvedMetadata(); ok {
		t.Fatal("manager without a bundle must not invent a CLI version")
	}
}

func TestDesktopBinaryResolvedIsDeliveredBeforeSessionInitialized(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	// The built-in bundle declares the fact; the baseline removes it so the
	// coverage delta below is attributable to this emitter alone.
	baseline := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		delete(bundle.Telemetry.Events, FactDesktopBinaryResolved)
	})
	baselineCoverage := baseline.Status().LiveEmitterCoverage
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	if declared := m.profile.Events[FactDesktopBinaryResolved]; declared.EventName != "desktop_ccd_binary_resolved" || strings.Join(declared.RequiredFacts, ",") != "resolution,resolved_version,required_version" {
		t.Fatalf("built-in bundle declaration for %s = %+v", FactDesktopBinaryResolved, declared)
	}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	span := m.BeginRequest(t.Context(), auth, facts)
	if !span.Active() {
		t.Fatal("main span inactive")
	}
	span.FinishSuccess(t.Context())
	// A second turn of the same session must not re-run the binary preflight.
	second := m.BeginRequest(t.Context(), auth, facts)
	second.FinishSuccess(t.Context())
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	var payload map[string]any
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://claude.ai/") {
			continue
		}
		var batch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
					Metadata  string `json:"metadata"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			t.Fatal(err)
		}
		for _, event := range batch.Events {
			names = append(names, event.EventData.EventName)
			if event.EventData.EventName == "desktop_ccd_binary_resolved" {
				if err := json.Unmarshal([]byte(event.EventData.Metadata), &payload); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	joined := strings.Join(names, ",")
	if !strings.HasPrefix(joined, "desktop_ccd_binary_resolved,desktop_ccd_session_initialized,desktop_ccd_message_cycle_start,") {
		t.Fatalf("binary preflight must precede the first-turn session init exactly once: %v", names)
	}
	if strings.Count(joined, "desktop_ccd_binary_resolved") != 1 {
		t.Fatalf("binary preflight repeated on a later turn: %v", names)
	}
	if payload["resolution"] != "required_version" || payload["resolved_version"] != m.bundle.CodeVersion || payload["required_version"] != m.bundle.CodeVersion || m.bundle.CodeVersion == "" {
		t.Fatalf("binary resolved payload = %v (code_version %q)", payload, m.bundle.CodeVersion)
	}
	if payload["app_version"] != m.profile.Runtime.ClientVersion || payload["organization_id"] != testOrgA {
		t.Fatalf("binary resolved payload lost the Desktop base metadata: %v", payload)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := false
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "desktop-event-logging" && pair.EventName == "desktop_ccd_binary_resolved" {
			executable = pair.Executable
		}
	}
	if !executable {
		t.Fatalf("desktop_ccd_binary_resolved is captured but not counted as executable: %+v", coverage.UnverifiedDeclaredEventNames)
	}
	if coverage.LiveEndpointEventCount != baselineCoverage.LiveEndpointEventCount+1 || coverage.LiveEventNameCount != baselineCoverage.LiveEventNameCount+1 {
		t.Fatalf("coverage moved unexpectedly: pairs %d->%d names %d->%d", baselineCoverage.LiveEndpointEventCount, coverage.LiveEndpointEventCount, baselineCoverage.LiveEventNameCount, coverage.LiveEventNameCount)
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.UnmodeledEndpointEventCount)
}

func TestDesktopBinaryResolvedStaysSilentWithoutProfileDeclaration(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		delete(bundle.Telemetry.Events, FactDesktopBinaryResolved)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := m.BeginRequest(t.Context(), auth, testRequestFacts("99999999-9999-4999-8999-999999999998"))
	span.FinishSuccess(t.Context())
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, request := range doer.Requests() {
		if strings.Contains(string(request.Body), "desktop_ccd_binary_resolved") {
			t.Fatal("undeclared Desktop fact was emitted")
		}
	}
}
