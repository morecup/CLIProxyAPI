package telemetry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudedesktop "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// queryContextGolden is the audit output of
// audit-sdk-telemetry-query-context-source.mjs.
type queryContextGolden struct {
	Contracts []struct {
		Event   string   `json:"event"`
		Gateway string   `json:"gateway"`
		Keys    []string `json:"keys"`
	} `json:"contracts"`
	OwnedAttachment          json.RawMessage `json:"owned_attachment"`
	OwnedAttachmentSizeBytes int             `json:"owned_attachment_size_bytes"`
	DurationCases            []struct {
		Label  string  `json:"label"`
		Random float64 `json:"random"`
		Events []struct {
			Name     string          `json:"name"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"events"`
	} `json:"duration_cases"`
}

func loadQueryContextGolden(t *testing.T) queryContextGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-query-context-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden queryContextGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func TestQueryContextMetadataMatchesNativeGolden(t *testing.T) {
	golden := loadQueryContextGolden(t)
	attachment := NewDeferredToolsDeltaAttachment([]string{"SendMessage", "TaskOutput", "TaskStop"}, nil)
	var native bytes.Buffer
	if err := json.Compact(&native, golden.OwnedAttachment); err != nil {
		t.Fatal(err)
	}
	if string(attachment) != native.String() {
		t.Fatalf("deferred_tools_delta attachment = %s, native %s", attachment, native.String())
	}
	if len(attachment) != golden.OwnedAttachmentSizeBytes {
		t.Fatalf("attachment size %d, native JSON.stringify length %d", len(attachment), golden.OwnedAttachmentSizeBytes)
	}
	built := map[string]any{
		"tengu_attachment_compute_duration": attachmentComputeDurationMetadata("", "", AttachmentComputeSample{Label: "deferred_tools_delta", Duration: 3 * time.Millisecond, Attachments: []json.RawMessage{attachment}}),
	}
	executable := 0
	for _, contract := range golden.Contracts {
		value, ok := built[contract.Event]
		if contract.Gateway != "executable" {
			if ok {
				t.Fatalf("%s is a native boundary but has an emitter", contract.Event)
			}
			continue
		}
		executable++
		if !ok {
			t.Fatalf("executable contract %s has no emitter", contract.Event)
		}
		if got := queryBuildMetadataKeys(t, value); strings.Join(got, ",") != strings.Join(contract.Keys, ",") {
			t.Fatalf("%s keys = %v, want native %v", contract.Event, got, contract.Keys)
		}
	}
	if executable != 1 {
		t.Fatalf("golden declares %d executable contracts, want 1", executable)
	}
	// The vm-executed fi() cases: sampled runs reproduce byte for byte (without
	// the logger prefix); draws at or above 0.05 emit nothing.
	for _, durationCase := range golden.DurationCases {
		sampled := AttachmentComputeSampled(durationCase.Random)
		if sampled != (len(durationCase.Events) == 1) {
			t.Fatalf("draw %v sampled=%v but native emitted %d events", durationCase.Random, sampled, len(durationCase.Events))
		}
		for _, event := range durationCase.Events {
			var metadata struct {
				DurationMS      int64 `json:"duration_ms"`
				AttachmentCount int   `json:"attachment_count"`
			}
			if err := json.Unmarshal(event.Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			sample := AttachmentComputeSample{Label: durationCase.Label, Duration: time.Duration(metadata.DurationMS) * time.Millisecond}
			for i := 0; i < metadata.AttachmentCount; i++ {
				sample.Attachments = append(sample.Attachments, attachment)
			}
			raw, err := json.Marshal(attachmentComputeDurationMetadata("", "", sample))
			if err != nil {
				t.Fatal(err)
			}
			native.Reset()
			if err := json.Compact(&native, event.Metadata); err != nil {
				t.Fatal(err)
			}
			if string(raw) != native.String() {
				t.Fatalf("%s = %s, native %s", event.Name, raw, native.String())
			}
		}
	}
	raw, err := json.Marshal(attachmentComputeDurationMetadata("pro", "prompt-1", AttachmentComputeSample{Label: "deferred_tools_delta", Duration: 1500 * time.Microsecond}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"subscription_type":"pro","cc_prompt_id":"prompt-1","label":"deferred_tools_delta","duration_ms":1,"attachment_size_bytes":0,"attachment_count":0}` {
		t.Fatalf("empty generator metadata = %s", raw)
	}
}

func queryContextTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKAttachmentComputeDuration] = claudeprofile.TelemetryEventProfile{EventName: "tengu_attachment_compute_duration", RequiredFacts: []string{"owned_request", "attachment_generator_timing", "session_default_betas"}}
	})
}

func TestAttachmentComputeDurationIsSampledAndDelivered(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := queryContextTestManager(t, clock, doer)
	before := m.Status().LiveEmitterCoverage
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: promptID, Model: model})
	if !span.Active() {
		t.Fatal("main span inactive")
	}
	attachment := NewDeferredToolsDeltaAttachment([]string{"SendMessage", "TaskOutput", "TaskStop"}, nil)
	sample := AttachmentComputeSample{Label: "deferred_tools_delta", Duration: 3 * time.Millisecond, Attachments: []json.RawMessage{attachment}}
	// Draws at or above the native 0.05 threshold and inactive spans emit nothing.
	span.ObserveAttachmentComputeDuration(sample, func() float64 { return 0.05 })
	span.ObserveAttachmentComputeDuration(sample, func() float64 { return 0.9 })
	var inactive *RequestSpan
	inactive.ObserveAttachmentComputeDuration(sample, func() float64 { return 0 })
	span.ObserveAttachmentComputeDuration(sample, func() float64 { return 0.049 })
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	betas, _ := m.sdkProfile.InputBetaHeader(model)
	delivered := 0
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		_ = json.Unmarshal(request.Body, &batch)
		for _, wrapper := range batch.Events {
			data := wrapper.EventData
			if data.EventName != "tengu_attachment_compute_duration" {
				continue
			}
			delivered++
			if data.SessionID != session || data.Model != model || data.Betas != betas || data.UserType != "external" || data.Auth.AccountUUID != testAccountA {
				t.Fatalf("borrowed dimensions: %+v", data)
			}
			raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"subscription_type":"pro","cc_prompt_id":"` + promptID + `","label":"deferred_tools_delta","duration_ms":3,"attachment_size_bytes":` + itoa(len(attachment)) + `,"attachment_count":1}`
			if string(raw) != want {
				t.Fatalf("payload = %s, want %s", raw, want)
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered %d tengu_attachment_compute_duration events, want exactly the sampled one", delivered)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := false
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" && pair.EventName == "tengu_attachment_compute_duration" {
			executable = pair.Executable
		}
	}
	if !executable {
		t.Fatalf("tengu_attachment_compute_duration delivered but not counted as executable: %+v", coverage.UnverifiedDeclaredEventNames)
	}
	if coverage.LiveEndpointEventCount < before.LiveEndpointEventCount || coverage.LiveEventNameCount < before.LiveEventNameCount {
		t.Fatalf("coverage regressed: %+v -> %+v", before, coverage)
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.ObservableEndpointEventCount-coverage.LiveEndpointEventCount)
}

func TestDefaultCreditBetaStripIsDeliveredToSDKAndDatadog(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	m := NewManager(Options{
		StatePath: t.TempDir(),
		Bundle:    bundle,
		Now:       clock.Now,
		RandomFloat: func() float64 {
			return 0
		},
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				response, err := doer.Do(request)
				if response != nil {
					response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/2.0", 2, 0
					if role == bundle.SDKTelemetry.EndpointRole || role == datadogLogsRole || role == datadogLogsBrowserRole {
						response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
					}
				}
				return response, err
			})
		},
	})
	t.Cleanup(m.Close)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: promptID, Model: model})
	span.ObserveDefaultCreditBetaStrip(true)
	child := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleSubagent, SessionID: session, PromptID: uuid.NewString(), Model: model})
	child.ObserveDefaultCreditBetaStrip(false)
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var sdkRequests, logRequests []recordedRequest
	for _, request := range doer.Requests() {
		switch {
		case strings.Contains(request.URL, "/api/event_logging"):
			sdkRequests = append(sdkRequests, request)
		case strings.Contains(request.URL, "/api/v2/logs"):
			logRequests = append(logRequests, request)
		}
	}
	if got := capturedSDKEventNames(t, sdkRequests)["tengu_rotunda_pennant_strip"]; got != 1 {
		t.Fatalf("SDK strip event count = %d", got)
	}
	if got := capturedDatadogMessages(t, logRequests)["tengu_rotunda_pennant_strip"]; got != 1 {
		urls := make([]string, 0, len(doer.Requests()))
		for _, request := range doer.Requests() {
			urls = append(urls, request.URL)
		}
		t.Fatalf("Datadog strip event count = %d; requests=%v; status=%+v", got, urls, m.Status().Accounts)
	}
	found := false
	for _, request := range sdkRequests {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			t.Fatal(err)
		}
		for _, wrapper := range batch.Events {
			if wrapper.EventData.EventName != "tengu_rotunda_pennant_strip" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(wrapper.EventData.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"subscription_type":"pro","cc_prompt_id":"` + promptID + `","shape":"credit_beta_header","mode":"none","non_streaming":true,"query_source":"sdk","sticky_scope":"session"}`
			if string(raw) != want {
				t.Fatalf("strip metadata = %s", raw)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("strip metadata was not delivered")
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		executable[pair.EndpointRole+"/"+pair.EventName] = pair.Executable
	}
	for _, key := range []string{"sdk-event-logging/tengu_rotunda_pennant_strip", datadogLogsRole + "/tengu_rotunda_pennant_strip"} {
		if !executable[key] {
			t.Fatalf("%s was delivered but not counted as executable", key)
		}
	}
}

func itoa(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
