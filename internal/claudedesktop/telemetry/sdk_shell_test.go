package telemetry

import (
	"encoding/json"
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

const testBashExecutedMetadata = `{"command_type":"other","stdout_length":91,"stderr_length":0,"exit_code":0,"interrupted":false,"executor_shell":"bash","executor_shell_overridden":false,"sandboxed":false,"sandbox_enabled":false,"dangerously_disable_sandbox":false,"filesystem_policy":"strict","call_origin":"local","had_sandbox_violation":false,"was_backgrounded":false,"tool_use_id":"tool-owned","destructive_category":"none","destructive_target_scope":"none","git_destructive_target":"none","permission_mode":"auto"}`

const testPowerShellExecutedMetadata = `{"command_type":"cmdlet_get","stdout_length":37,"stderr_length":0,"exit_code":0,"interrupted":false,"powershell_edition":"desktop","destructive_category":"none","destructive_target_scope":"none","permission_mode":"default"}`

func TestSDKShellMetadataAcceptsOnlyCompleteContentFreeOwnerFacts(t *testing.T) {
	tests := []struct {
		name     string
		fact     string
		metadata string
		valid    bool
	}{
		{name: "bash", fact: FactSDKBashToolCommandExecuted, metadata: testBashExecutedMetadata, valid: true},
		{name: "powershell", fact: FactSDKPowerShellToolCommandExecuted, metadata: testPowerShellExecutedMetadata, valid: true},
		{name: "trailing JSON", fact: FactSDKBashToolCommandExecuted, metadata: testBashExecutedMetadata + ` true`},
		{name: "missing result", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `,"exit_code":0`, "", 1)},
		{name: "fractional length", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `"stdout_length":91`, `"stdout_length":1.5`, 1)},
		{name: "reserved prefix", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `{`, `{"subscription_type":"forged",`, 1)},
		{name: "command content", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `{`, `{"command":"PRIVATE_COMMAND",`, 1)},
		{name: "unknown content", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `{`, `{"script":"PRIVATE_SCRIPT",`, 1)},
		{name: "duplicate", fact: FactSDKBashToolCommandExecuted, metadata: strings.Replace(testBashExecutedMetadata, `{`, `{"stdout_length":1,`, 1)},
		{name: "bash field on powershell", fact: FactSDKPowerShellToolCommandExecuted, metadata: strings.Replace(testPowerShellExecutedMetadata, `{`, `{"sandboxed":false,`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSDKShellExecutedMetadata(test.fact, json.RawMessage(test.metadata))
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid shell metadata was accepted")
			}
		})
	}
}

func TestSDKShellCommandDeliversBashMirrorButNotPowerShellMirror(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)}
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
	if errValidate := bundle.Validate(); errValidate != nil {
		t.Fatal(errValidate)
	}
	var captureMu sync.Mutex
	captured := make(map[string][]recordedRequest)
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle, Now: clock.Now, RandomFloat: func() float64 { return 0 },
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
	t.Cleanup(manager.Close)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	if err := manager.RecordSDKShellCommand(t.Context(), auth, "session-shell", "claude-opus-5", "prompt-shell", "tengu_bash_tool_command_executed", json.RawMessage(testBashExecutedMetadata)); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordSDKShellCommand(t.Context(), auth, "session-shell", "claude-opus-5", "prompt-shell", "tengu_powershell_tool_command_executed", json.RawMessage(testPowerShellExecutedMetadata)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	captureMu.Lock()
	requests := make(map[string][]recordedRequest, len(captured))
	for role, values := range captured {
		requests[role] = append([]recordedRequest(nil), values...)
	}
	captureMu.Unlock()
	var shellEvents []sdkEventWrapper
	for _, event := range sdkEventsFromRequests(t, requests[bundle.SDKTelemetry.EndpointRole]) {
		if event.EventData.EventName == "tengu_bash_tool_command_executed" || event.EventData.EventName == "tengu_powershell_tool_command_executed" {
			shellEvents = append(shellEvents, event)
		}
	}
	if len(shellEvents) != 2 || shellEvents[0].EventData.EventName != "tengu_bash_tool_command_executed" || shellEvents[1].EventData.EventName != "tengu_powershell_tool_command_executed" {
		t.Fatalf("SDK shell event sequence = %+v", shellEvents)
	}
	for index, raw := range []string{testBashExecutedMetadata, testPowerShellExecutedMetadata} {
		want := `{"subscription_type":"pro","cc_prompt_id":"prompt-shell",` + strings.TrimPrefix(raw, `{`)
		if got := sdkMetadataJSON(t, shellEvents[index]); got != want {
			t.Fatalf("shell metadata %d = %s, want %s", index, got, want)
		}
	}
	datadogMessages := capturedDatadogMessages(t, requests[datadogLogsRole])
	if datadogMessages["tengu_bash_tool_command_executed"] != 1 || datadogMessages["tengu_powershell_tool_command_executed"] != 0 {
		t.Fatalf("Datadog shell mirrors = %v", datadogMessages)
	}
	for _, request := range append(requests[bundle.SDKTelemetry.EndpointRole], requests[datadogLogsRole]...) {
		if strings.Contains(string(request.Body), "PRIVATE_") {
			t.Fatal("shell command content entered telemetry")
		}
	}
}

func TestSDKShellCommandDisabledAndUnknownEvent(t *testing.T) {
	if err := (&Manager{}).RecordSDKShellCommand(t.Context(), nil, "", "", "", "unknown", json.RawMessage(`not-json`)); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 9, 22, 10, 30, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := manager.RecordSDKShellCommand(t.Context(), auth, "session", "claude-opus-5", "prompt", "unknown", json.RawMessage(testBashExecutedMetadata)); err == nil {
		t.Fatal("unknown shell event was accepted")
	}
}
