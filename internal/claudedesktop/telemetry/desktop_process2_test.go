package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type desktopProcess2Golden struct {
	ExecutableEvents []struct {
		EventName   string   `json:"event_name"`
		GatewayFact string   `json:"gateway_fact"`
		PayloadKeys []string `json:"payload_keys"`
		Literals    []string `json:"literals"`
		Probes      []struct {
			Addon    string                 `json:"addon"`
			Emitted  bool                   `json:"emitted"`
			Metadata map[string]interface{} `json:"metadata"`
		} `json:"probes"`
	} `json:"executable_events"`
}

func loadDesktopProcess2Golden(t *testing.T) desktopProcess2Golden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-process2-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden desktopProcess2Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func declareDesktopWindowsElevation(bundle *claudeprofile.Bundle) {
	bundle.Telemetry.Events[FactDesktopWindowsElevationDetected] = claudeprofile.TelemetryEventProfile{
		EventName:     "desktop_windows_elevation_detected",
		RequiredFacts: []string{"elevation_type", "can_elevate"},
	}
}

func TestDesktopOAuthFailedMetadataMatchesNativeShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-process2-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		ExecutableEvents []struct {
			EventName           string   `json:"event_name"`
			GatewayFact         string   `json:"gateway_fact"`
			OAuthTypeLiterals   []string `json:"oauth_type_literals"`
			RefreshFailureTypes []struct {
				Type      string `json:"type"`
				HasStatus bool   `json:"has_status"`
			} `json:"refresh_failure_types"`
			Probes []struct {
				Probe    string          `json:"probe"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"probes"`
		} `json:"executable_events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	var index = -1
	for i, event := range golden.ExecutableEvents {
		if event.EventName == "desktop_oauth_failed" {
			index = i
		}
	}
	if index < 0 || golden.ExecutableEvents[index].GatewayFact != FactDesktopOAuthFailed {
		t.Fatalf("golden does not pin desktop_oauth_failed as %s", FactDesktopOAuthFailed)
	}
	pinned := golden.ExecutableEvents[index]
	if strings.Join(pinned.OAuthTypeLiterals, ",") != "refresh,initial" || pinned.OAuthTypeLiterals[0] != DesktopOAuthTypeRefresh {
		t.Fatalf("golden oauth_type literals = %v", pinned.OAuthTypeLiterals)
	}
	if len(pinned.RefreshFailureTypes) != 3 || pinned.RefreshFailureTypes[0].Type != desktopOAuthFailureNetworkError || pinned.RefreshFailureTypes[0].HasStatus || pinned.RefreshFailureTypes[1].Type != desktopOAuthFailureServerError || !pinned.RefreshFailureTypes[1].HasStatus || pinned.RefreshFailureTypes[2].Type != desktopOAuthFailureAuthError || !pinned.RefreshFailureTypes[2].HasStatus {
		t.Fatalf("golden refresh failure enum = %+v", pinned.RefreshFailureTypes)
	}
	// Byte-order check of every probe the gateway can produce (no `client`).
	want := map[string]desktopOAuthFailedMetadata{
		"refresh_network_error":    buildDesktopOAuthFailedMetadata(DesktopOAuthTypeRefresh, DesktopOAuthFailure{Reason: desktopOAuthFailureNetworkError}),
		"refresh_auth_error_401":   buildDesktopOAuthFailedMetadata(DesktopOAuthTypeRefresh, DesktopOAuthFailure{Reason: desktopOAuthFailureAuthError, Status: 401, HasStatus: true}),
		"refresh_server_error_503": buildDesktopOAuthFailedMetadata(DesktopOAuthTypeRefresh, DesktopOAuthFailure{Reason: desktopOAuthFailureServerError, Status: 503, HasStatus: true}),
	}
	checked := 0
	for _, probe := range pinned.Probes {
		metadata, ok := want[probe.Probe]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		var native bytes.Buffer
		if err := json.Compact(&native, probe.Metadata); err != nil {
			t.Fatal(err)
		}
		if string(encoded) != native.String() {
			t.Fatalf("probe %s: gateway %s, native %s", probe.Probe, encoded, native.String())
		}
		checked++
	}
	if checked != len(want) {
		t.Fatalf("golden probes checked = %d, want %d", checked, len(want))
	}
	// Gateway error classes -> native refresh failure enum.
	if failure, ok := ClassifyDesktopOAuthRefreshError(&url.Error{Op: "Post", URL: "https://api.anthropic.com/v1/oauth/token", Err: errors.New("dial tcp: connection refused")}); !ok || failure.Reason != "network_error" || failure.HasStatus {
		t.Fatalf("transport error classified as %+v,%v", failure, ok)
	}
	if failure, ok := ClassifyDesktopOAuthRefreshError(fmt.Errorf("wrapped: %w", &claudedesktop.HTTPStatusError{Status: 502, Detail: "bad gateway"})); !ok || failure.Reason != "server_error" || failure.Status != 502 {
		t.Fatalf("5xx classified as %+v,%v", failure, ok)
	}
	if failure, ok := ClassifyDesktopOAuthRefreshError(&claudedesktop.HTTPStatusError{Status: 400, Detail: "invalid_grant"}); !ok || failure.Reason != "auth_error" || failure.Status != 400 {
		t.Fatalf("4xx classified as %+v,%v", failure, ok)
	}
	for _, unmapped := range []error{nil, errors.New("token endpoint returned no access_token"), errors.New("Claude Desktop refresh token is missing"), fmt.Errorf("parse response: %w", errors.New("unexpected end of JSON input"))} {
		if _, ok := ClassifyDesktopOAuthRefreshError(unmapped); ok {
			t.Fatalf("%v must not be reported (native throws without a typed result)", unmapped)
		}
	}
}

func TestDesktopOAuthFailedIsDeliveredAsApplicationFact(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	// The built-in bundle declares the fact; the baseline removes it so the
	// delta still proves the declaration is what makes the pair executable.
	baseline := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		delete(bundle.Telemetry.Events, FactDesktopOAuthFailed)
	})
	baselineCoverage := baseline.Status().LiveEmitterCoverage
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Events[FactDesktopOAuthFailed] = claudeprofile.TelemetryEventProfile{EventName: "desktop_oauth_failed", RequiredFacts: []string{"oauth_type", "failure_reason"}}
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	// The executor classifies the real service.Refresh error before recording.
	failure, ok := ClassifyDesktopOAuthRefreshError(&claudedesktop.HTTPStatusError{Status: 401, Detail: "invalid_grant"})
	if !ok {
		t.Fatal("HTTP 401 must classify as auth_error")
	}
	if err := m.RecordDesktopOAuthFailed(t.Context(), auth, DesktopOAuthTypeRefresh, failure); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordDesktopOAuthFailed(t.Context(), auth, "", failure); err == nil {
		t.Fatal("an oauth failure without oauth_type must be rejected")
	}
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	count := 0
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
			if event.EventData.EventName != "desktop_oauth_failed" {
				continue
			}
			count++
			if err := json.Unmarshal([]byte(event.EventData.Metadata), &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("desktop_oauth_failed delivered %d times", count)
	}
	if payload["oauth_type"] != "refresh" || payload["failure_reason"] != "auth_error" || payload["status"] != float64(401) {
		t.Fatalf("oauth failed payload = %v", payload)
	}
	if _, hasClient := payload["client"]; hasClient {
		t.Fatalf("client key must be absent for the user:inference scope: %v", payload)
	}
	if _, hasSession := payload["session_id"]; hasSession {
		t.Fatalf("application fact must not carry a session: %v", payload)
	}
	if payload["app_version"] != m.profile.Runtime.ClientVersion || payload["organization_id"] != testOrgA {
		t.Fatalf("oauth failed payload lost the Desktop base metadata: %v", payload)
	}
	coverage := m.Status().LiveEmitterCoverage
	if coverage.LiveEndpointEventCount != baselineCoverage.LiveEndpointEventCount+1 || coverage.LiveEventNameCount != baselineCoverage.LiveEventNameCount+1 {
		t.Fatalf("coverage moved unexpectedly: pairs %d->%d names %d->%d", baselineCoverage.LiveEndpointEventCount, coverage.LiveEndpointEventCount, baselineCoverage.LiveEventNameCount, coverage.LiveEventNameCount)
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.UnmodeledEndpointEventCount)
}

func TestDesktopWindowsElevationMetadataMatchesNativeShape(t *testing.T) {
	golden := loadDesktopProcess2Golden(t)
	if len(golden.ExecutableEvents) != 2 || golden.ExecutableEvents[0].EventName != "desktop_windows_elevation_detected" || golden.ExecutableEvents[0].GatewayFact != FactDesktopWindowsElevationDetected {
		t.Fatalf("golden does not pin desktop_windows_elevation_detected as %s: %+v", FactDesktopWindowsElevationDetected, golden)
	}
	pinned := golden.ExecutableEvents[0]
	if strings.Join(pinned.PayloadKeys, ",") != "elevation_type,can_elevate" {
		t.Fatalf("golden payload keys = %v", pinned.PayloadKeys)
	}
	if strings.Join(pinned.Literals, ",") != "default,full,limited" {
		t.Fatalf("golden addon enum = %v", pinned.Literals)
	}
	// Every addon probe of the native builder must be reproduced byte for byte.
	for _, probe := range pinned.Probes {
		elevationType, _ := probe.Metadata["elevation_type"].(string)
		metadata := buildDesktopWindowsElevationMetadata(elevationType)
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(map[string]any{"elevation_type": probe.Metadata["elevation_type"]})
		if err != nil {
			t.Fatal(err)
		}
		canElevate := "false"
		if probe.Metadata["can_elevate"] == true {
			canElevate = "true"
		}
		expected := strings.TrimSuffix(string(want), "}") + `,"can_elevate":` + canElevate + `}`
		if string(raw) != expected {
			t.Fatalf("probe %s: gateway metadata = %s, want native %s", probe.Addon, raw, expected)
		}
		for key, value := range metadata.toMap() {
			if probe.Metadata[key] != value {
				t.Fatalf("probe %s: native %s = %v, gateway = %v", probe.Addon, key, probe.Metadata[key], value)
			}
		}
	}
	// TOKEN_ELEVATION_TYPE -> addon enum; unknown token values never invent a name.
	for kind, want := range map[uint32]string{1: "default", 2: "full", 3: "limited"} {
		if got, ok := desktopElevationTypeName(kind); !ok || got != want {
			t.Fatalf("elevation type %d = %q,%v want %q", kind, got, ok, want)
		}
	}
	if _, ok := desktopElevationTypeName(0); ok {
		t.Fatal("unknown TOKEN_ELEVATION_TYPE must not be reported")
	}
}

func TestDesktopWindowsElevationIsDeliveredOncePerActivation(t *testing.T) {
	if runtime.GOOS != "windows" {
		if _, ok := probeDesktopWindowsElevation(); ok {
			t.Fatal("non-Windows hosts have no process token elevation type")
		}
		t.Skip("desktop_windows_elevation_detected needs the Windows process token")
	}
	probed, ok := probeDesktopWindowsElevation()
	if !ok {
		t.Fatal("process token elevation type is unreadable")
	}
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	baseline := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		delete(bundle.Telemetry.Events, FactDesktopWindowsElevationDetected)
	})
	baselineCoverage := baseline.Status().LiveEmitterCoverage
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, declareDesktopWindowsElevation)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := m.Activate(auth); err != nil {
		t.Fatal(err)
	}
	// A second activation of the same account is the same emulated main process.
	if err := m.Activate(auth); err != nil {
		t.Fatal(err)
	}
	span := m.BeginRequest(t.Context(), auth, testRequestFacts("99999999-9999-4999-8999-999999999997"))
	span.FinishSuccess(t.Context())
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	var payload map[string]any
	var rawMetadata string
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
			if event.EventData.EventName == "desktop_windows_elevation_detected" {
				rawMetadata = event.EventData.Metadata
				if err := json.Unmarshal([]byte(event.EventData.Metadata), &payload); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	joined := strings.Join(names, ",")
	if strings.Count(joined, "desktop_windows_elevation_detected") != 1 {
		t.Fatalf("elevation detection must run exactly once per emulated main process: %v", names)
	}
	if !strings.HasPrefix(joined, "desktop_windows_elevation_detected,") {
		t.Fatalf("app-ready elevation detection must precede every session event: %v", names)
	}
	if payload["elevation_type"] != probed || payload["can_elevate"] != (probed == "limited" || probed == "full") {
		t.Fatalf("elevation payload %v does not match the process token (%s)", payload, probed)
	}
	if payload["app_version"] != m.profile.Runtime.ClientVersion || payload["organization_id"] != testOrgA {
		t.Fatalf("elevation payload lost the Desktop base metadata: %s", rawMetadata)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := false
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "desktop-event-logging" && pair.EventName == "desktop_windows_elevation_detected" {
			executable = pair.Executable
		}
	}
	if !executable {
		t.Fatalf("desktop_windows_elevation_detected is captured but not counted as executable: %+v", coverage.UnverifiedDeclaredEventNames)
	}
	if coverage.LiveEndpointEventCount != baselineCoverage.LiveEndpointEventCount+1 || coverage.LiveEventNameCount != baselineCoverage.LiveEventNameCount+1 {
		t.Fatalf("coverage moved unexpectedly: pairs %d->%d names %d->%d", baselineCoverage.LiveEndpointEventCount, coverage.LiveEndpointEventCount, baselineCoverage.LiveEventNameCount, coverage.LiveEventNameCount)
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.UnmodeledEndpointEventCount)
}

func TestDesktopWindowsElevationStaysSilentWithoutProfileDeclaration(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		delete(bundle.Telemetry.Events, FactDesktopWindowsElevationDetected)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := m.Activate(auth); err != nil {
		t.Fatal(err)
	}
	span := m.BeginRequest(t.Context(), auth, testRequestFacts("99999999-9999-4999-8999-999999999996"))
	span.FinishSuccess(t.Context())
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, request := range doer.Requests() {
		if strings.Contains(string(request.Body), "desktop_windows_elevation_detected") {
			t.Fatal("undeclared Desktop fact was emitted")
		}
	}
}
