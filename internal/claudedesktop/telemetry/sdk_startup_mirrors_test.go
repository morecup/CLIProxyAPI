package telemetry

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Startup emissions registered by topic files go through enqueueSDKEventAt,
// so every fact the datadog-logs profile maps is mirrored exactly like the
// core lifecycle events (the _675.js known-event set includes tengu_timer
// and tengu_headless_mcp_prewait).
func TestStartupMirrorsReachDatadogLogsForMappedFacts(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, batch := range []*claudeprofile.TelemetryBatchProfile{
		&bundle.Telemetry.Batch, &bundle.SDKTelemetry.Batch, &bundle.AuxiliaryTelemetry.Segment.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogs.Batch, &bundle.AuxiliaryTelemetry.DatadogLogsBrowser.Batch,
		&bundle.AuxiliaryTelemetry.DatadogRUM.Batch, &bundle.AuxiliaryTelemetry.Sentry.Batch,
	} {
		batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		batch.JitterMinimum = 1
		batch.JitterMaximum = 1
	}
	for fact, name := range map[string]string{FactSDKTimer: "tengu_timer", FactSDKHeadlessMCPPrewait: "tengu_headless_mcp_prewait"} {
		bundle.SDKTelemetry.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
		bundle.AuxiliaryTelemetry.DatadogLogs.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
	}
	if errValidate := bundle.Validate(); errValidate != nil {
		t.Fatalf("validate test bundle: %v", errValidate)
	}
	var captureMu sync.Mutex
	captured := make(map[string][]recordedRequest)
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle, ApplicationSessionID: "99999999-9999-4999-8999-999999999999",
		Now: clock.Now, RandomFloat: func() float64 { return 0 },
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				captureMu.Lock()
				captured[role] = append(captured[role], recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
				captureMu.Unlock()
				proto, major, minor := "HTTP/2.0", 2, 0
				if role == bundle.SDKTelemetry.EndpointRole || role == datadogLogsRole || role == datadogLogsBrowserRole {
					proto, major, minor = "HTTP/1.1", 1, 1
				}
				return &http.Response{StatusCode: http.StatusOK, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
		},
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	if errActivate := manager.Activate(auth); errActivate != nil {
		t.Fatal(errActivate)
	}
	clock.Advance(12 * time.Second)
	manager.Close()

	captureMu.Lock()
	requests := make(map[string][]recordedRequest, len(captured))
	for role, values := range captured {
		requests[role] = append([]recordedRequest(nil), values...)
	}
	captureMu.Unlock()
	sdkNames := capturedSDKEventNames(t, requests[bundle.SDKTelemetry.EndpointRole])
	datadogMessages := capturedDatadogMessages(t, requests[datadogLogsRole])
	for _, name := range []string{"tengu_timer", "tengu_headless_mcp_prewait", "tengu_started", "tengu_sdk_init_handshake"} {
		if sdkNames[name] != 1 {
			t.Fatalf("SDK startup event %q count = %d; all=%v", name, sdkNames[name], sdkNames)
		}
		if datadogMessages[name] != 1 {
			t.Fatalf("Datadog mirror of %q count = %d; all=%v", name, datadogMessages[name], datadogMessages)
		}
	}
	coverage := manager.Status().LiveEmitterCoverage
	mirrored := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == datadogLogsRole && pair.Executable {
			mirrored[pair.EventName] = true
		}
	}
	if !mirrored["tengu_timer"] || !mirrored["tengu_headless_mcp_prewait"] {
		t.Fatalf("datadog-logs mirrors are not counted as executable: %v", mirrored)
	}
}
