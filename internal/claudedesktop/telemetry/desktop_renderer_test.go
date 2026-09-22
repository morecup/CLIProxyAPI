package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type desktopRendererGolden struct {
	Events []struct {
		EventName            string   `json:"event_name"`
		Roles                []string `json:"roles"`
		DualFireFact         string   `json:"dual_fire_fact"`
		DesktopEventDataKeys []string `json:"desktop_event_data_keys"`
		PropertyKeys         []string `json:"property_keys"`
		DualFireSuffixKeys   []string `json:"dual_fire_suffix_keys"`
	} `json:"events"`
}

func loadDesktopRendererGolden(t *testing.T) desktopRendererGolden {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-renderer-native.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var golden desktopRendererGolden
	if errDecode := json.Unmarshal(raw, &golden); errDecode != nil {
		t.Fatal(errDecode)
	}
	return golden
}

func TestDesktopRendererDualFireMirrorsSegmentTrackIntoDesktopEventLogging(t *testing.T) {
	golden := loadDesktopRendererGolden(t)
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
	bundle.Telemetry.Events[FactRendererMessageSubmitted] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.code.message.submitted"}
	bundle.Telemetry.Events[FactRendererSessionTTFT] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.code.session.ttft"}
	if errValidate := bundle.Validate(); errValidate != nil {
		t.Fatalf("validate test bundle: %v", errValidate)
	}
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	var captureMu sync.Mutex
	captured := make(map[string][]recordedRequest)
	rendererRole := bundle.Telemetry.EndpointRole
	sdkRole := bundle.SDKTelemetry.EndpointRole
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle, Now: clock.Now, RandomFloat: func() float64 { return 0 },
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				captureMu.Lock()
				captured[role] = append(captured[role], recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
				captureMu.Unlock()
				proto, major, minor := "HTTP/2.0", 2, 0
				if role == sdkRole || role == datadogLogsRole || role == datadogLogsBrowserRole {
					proto, major, minor = "HTTP/1.1", 1, 1
				}
				return &http.Response{StatusCode: http.StatusOK, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
		},
	})
	t.Cleanup(manager.Close)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	// Activation is not required for request-scoped renderer analytics: the
	// Segment and Desktop workers are bound when the span begins.
	before := manager.Status().LiveEmitterCoverage.LiveEndpointEventCount
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`), http.Header{})
	span.ObserveFirstByte(clock.Now().Add(150 * time.Millisecond))
	span.ObserveResponse("req_renderer_dual", "end_turn")
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	captureMu.Lock()
	rendererRequests := append([]recordedRequest(nil), captured[rendererRole]...)
	segmentRequests := append([]recordedRequest(nil), captured[segmentRole]...)
	captureMu.Unlock()

	segmentProperties := make(map[string]map[string]any)
	segmentAnonymous := ""
	for _, request := range segmentRequests {
		var batch struct {
			Batch []map[string]any `json:"batch"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, item := range batch.Batch {
			name, _ := item["event"].(string)
			if properties, ok := item["properties"].(map[string]any); ok && name != "" {
				segmentProperties[name] = properties
				segmentAnonymous, _ = item["anonymousId"].(string)
			}
		}
	}
	type desktopEvent struct {
		EventType string `json:"event_type"`
		EventData struct {
			EventName        string `json:"event_name"`
			EventID          string `json:"event_id"`
			EventTimestamp   string `json:"event_timestamp"`
			AccountUUID      string `json:"account_uuid"`
			OrganizationUUID string `json:"organization_uuid"`
			Properties       string `json:"properties"`
		} `json:"event_data"`
	}
	dualFire := make(map[string]desktopEvent)
	for _, request := range rendererRequests {
		var batch struct {
			Events []json.RawMessage `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, raw := range batch.Events {
			var event desktopEvent
			if errDecode := json.Unmarshal(raw, &event); errDecode != nil {
				t.Fatal(errDecode)
			}
			if event.EventType == desktopRendererEventType {
				dualFire[event.EventData.EventName] = event
			}
		}
	}
	for _, expected := range golden.Events {
		if len(expected.Roles) != 2 || expected.DualFireFact == "" || expected.EventName == "$identify" {
			continue
		}
		event, ok := dualFire[expected.EventName]
		if !ok {
			t.Fatalf("Desktop dual-fire copy of %q was not delivered; got %v", expected.EventName, dualFire)
		}
		if event.EventData.AccountUUID != testAccountA || event.EventData.OrganizationUUID != testOrgA || event.EventData.EventID == "" {
			t.Fatalf("Desktop dual-fire envelope for %q is incomplete: %+v", expected.EventName, event.EventData)
		}
		if _, errTime := time.Parse("2006-01-02T15:04:05.000Z", event.EventData.EventTimestamp); errTime != nil {
			t.Fatalf("Desktop dual-fire timestamp format: %v", errTime)
		}
		var properties map[string]any
		if errDecode := json.Unmarshal([]byte(event.EventData.Properties), &properties); errDecode != nil {
			t.Fatalf("decode dual-fire properties for %q: %v", expected.EventName, errDecode)
		}
		twin := segmentProperties[expected.EventName]
		if twin == nil {
			t.Fatalf("Segment twin of %q was not delivered", expected.EventName)
		}
		allowed := make(map[string]bool, len(expected.PropertyKeys))
		for _, key := range expected.PropertyKeys {
			allowed[key] = true
		}
		// projectSegment shares one base property set for both events; the
		// captured ttft schema does not carry renderer_surface. The mirror must
		// stay byte-identical to its Segment twin, so this pre-existing Segment
		// deviation is reported rather than hidden by the copy.
		knownSegmentDeviation := map[string]bool{"claudeai.code.session.ttft\x00renderer_surface": true}
		for key, value := range twin {
			if !allowed[key] {
				if !knownSegmentDeviation[expected.EventName+"\x00"+key] {
					t.Fatalf("Segment property %q of %q is not part of the captured schema", key, expected.EventName)
				}
				t.Logf("known Segment deviation mirrored: %s.%s is absent from the captured schema", expected.EventName, key)
			}
			encodedTwin, _ := json.Marshal(value)
			encodedCopy, _ := json.Marshal(properties[key])
			if string(encodedTwin) != string(encodedCopy) {
				t.Fatalf("dual-fire property %q of %q = %s, Segment twin %s", key, expected.EventName, encodedCopy, encodedTwin)
			}
		}
		if len(properties) != len(twin)+len(expected.DualFireSuffixKeys) {
			t.Fatalf("dual-fire copy of %q carries %d properties, want %d", expected.EventName, len(properties), len(twin)+len(expected.DualFireSuffixKeys))
		}
		suffix := strings.Join(expected.DualFireSuffixKeys, `":`)
		if !strings.Contains(event.EventData.Properties, `"`+strings.ReplaceAll(suffix, `":`, `":`)+`"`) && !strings.Contains(event.EventData.Properties, `"_dual_fire":true,"anonymous_id":`) {
			t.Fatalf("dual-fire suffix order is not native: %s", event.EventData.Properties)
		}
		if properties["_dual_fire"] != true || properties["service_name"] != desktopRendererServiceName || properties["path"] != desktopRendererPath || properties["anonymous_id"] != segmentAnonymous {
			t.Fatalf("dual-fire suffix values are wrong: %v", properties)
		}
	}
	// Other renderer topic files (session lifecycle, shell state) add their
	// own dual-fire copies on the first turn; this test owns exactly the two
	// request-moment copies and requires each once.
	for _, name := range []string{"claudeai.code.message.submitted", "claudeai.code.session.ttft"} {
		if _, ok := dualFire[name]; !ok {
			t.Fatalf("dual-fire copy of %q missing; got %v", name, dualFire)
		}
	}
	after := manager.Status().LiveEmitterCoverage.LiveEndpointEventCount
	if after != before {
		t.Fatalf("coverage changed during the request: before=%d after=%d", before, after)
	}
	executable := manager.executableEndpointEvents()
	for _, name := range []string{"claudeai.code.message.submitted", "claudeai.code.session.ttft"} {
		if _, ok := executable[rendererRole+"\x00"+name]; !ok {
			t.Fatalf("desktop-event-logging %q is not counted as executable", name)
		}
	}
	t.Logf("live endpoint_events=%d", after)
}

func TestDesktopRendererIdentifyCopyFollowsSegmentIdentifyOnce(t *testing.T) {
	golden := loadDesktopRendererGolden(t)
	var expectedKeys []string
	for _, event := range golden.Events {
		if event.EventName == "$identify" {
			expectedKeys = event.PropertyKeys
		}
	}
	if len(expectedKeys) == 0 {
		t.Fatal("golden does not pin $identify")
	}
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Events[FactRendererIdentify] = claudeprofile.TelemetryEventProfile{EventName: "$identify"}
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	segmentWorker, errWorker := manager.workerForDelivery(auth, manager.auxiliaryDeliveries[segmentRole])
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	desktopWorker, errDesktop := manager.workerFor(auth)
	if errDesktop != nil {
		t.Fatal(errDesktop)
	}
	for i := 0; i < 2; i++ {
		if errIdentify := manager.ensureSegmentIdentified(segmentWorker); errIdentify != nil {
			t.Fatal(errIdentify)
		}
	}
	files, errScan := desktopWorker.scanQueue()
	if errScan != nil {
		t.Fatal(errScan)
	}
	// The copy may still sit in the protected queue or already be delivered
	// (later activation hooks wake the worker); both places are inspected.
	var payloads []json.RawMessage
	for _, file := range files {
		raw, errRead := readProtectedFile(file.path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var envelope Envelope
		if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
			t.Fatal(errDecode)
		}
		if envelope.CatalogFact != FactRendererIdentify {
			continue
		}
		if envelope.CatalogEvent != "$identify" || envelope.EndpointRole != manager.profile.EndpointRole || envelope.SessionID != manager.appSessionID {
			t.Fatalf("identify envelope = %+v", envelope)
		}
		payloads = append(payloads, envelope.Payload)
	}
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://claude.ai/") {
			continue
		}
		var batch struct {
			Events []json.RawMessage `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, raw := range batch.Events {
			if strings.Contains(string(raw), `"event_name":"$identify"`) {
				payloads = append(payloads, raw)
			}
		}
	}
	copies := 0
	for _, payload := range payloads {
		copies++
		var event desktopRendererEvent
		if errDecode := json.Unmarshal(payload, &event); errDecode != nil {
			t.Fatal(errDecode)
		}
		if event.EventType != desktopRendererEventType || event.EventData.EventName != "$identify" || event.EventData.AccountUUID != testAccountA || event.EventData.OrganizationUUID != testOrgA {
			t.Fatalf("identify event_data = %+v", event)
		}
		want := `{"anonymous_id":"` + segmentWorker.binding.RuntimeUUID + `","service_name":"claude_ai","path":"/epitaxy"}`
		if event.EventData.Properties != want {
			t.Fatalf("identify properties = %s, want %s", event.EventData.Properties, want)
		}
		var properties map[string]any
		_ = json.Unmarshal([]byte(event.EventData.Properties), &properties)
		if len(properties) != len(expectedKeys) {
			t.Fatalf("identify property keys = %v, golden %v", properties, expectedKeys)
		}
		for _, key := range expectedKeys {
			if _, ok := properties[key]; !ok {
				t.Fatalf("identify property %q missing", key)
			}
		}
	}
	if copies != 1 {
		t.Fatalf("Desktop $identify copies = %d, want exactly one per activation", copies)
	}
	if _, ok := manager.executableEndpointEvents()[manager.profile.EndpointRole+"\x00$identify"]; !ok {
		t.Fatal("desktop-event-logging $identify is not counted as executable")
	}
}

func TestDesktopRendererDualFirePropertiesKeepNativeSuffixOrder(t *testing.T) {
	segment := []byte(`{"event":"claudeai.code.message.submitted","type":"track","properties":{"account_uuid":"a","version":1},"anonymousId":"anon-1"}`)
	properties, errProperties := desktopRendererDualFireProperties(segment)
	if errProperties != nil {
		t.Fatal(errProperties)
	}
	want := `{"account_uuid":"a","version":1,"_dual_fire":true,"anonymous_id":"anon-1","service_name":"claude_ai","path":"/epitaxy/$sessionId"}`
	if properties != want {
		t.Fatalf("properties = %s, want %s", properties, want)
	}
	if _, errEmpty := desktopRendererDualFireProperties([]byte(`{"event":"x"}`)); errEmpty == nil {
		t.Fatal("missing properties must fail the projection")
	}
}
