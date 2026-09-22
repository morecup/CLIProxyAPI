package telemetry

import (
	"bytes"
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

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type desktopRendererSessionGolden struct {
	Events []struct {
		EventName    string         `json:"event_name"`
		Fact         string         `json:"fact"`
		SegmentType  string         `json:"segment_type"`
		Roles        []string       `json:"roles"`
		PropertyKeys []string       `json:"property_keys"`
		Constants    map[string]any `json:"constants"`
		DesktopPath  string         `json:"desktop_path"`
		Boundary     *string        `json:"boundary"`
	} `json:"events"`
}

func loadDesktopRendererSessionGolden(t *testing.T) desktopRendererSessionGolden {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-renderer-session-native.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var golden desktopRendererSessionGolden
	if errDecode := json.Unmarshal(raw, &golden); errDecode != nil {
		t.Fatal(errDecode)
	}
	return golden
}

// jsonObjectKeys returns the top-level keys of a JSON object in wire order.
func jsonObjectKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", raw)
	}
	var keys []string
	depth := 0
	for decoder.More() || depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		switch value := token.(type) {
		case json.Delim:
			if value == '{' || value == '[' {
				depth++
			} else {
				depth--
			}
		case string:
			if depth == 0 {
				keys = append(keys, value)
				var skip json.RawMessage
				if errValue := decoder.Decode(&skip); errValue != nil {
					t.Fatal(errValue)
				}
			}
		}
	}
	return keys
}

func rendererSessionTestIdentity() rendererSessionIdentity {
	return rendererSessionIdentity{AccountUUID: testAccountA, OrganizationUUID: testOrgA, BillingType: "stripe_subscription", AppVersion: "1.40609.0"}
}

func TestDesktopRendererSessionPropertiesKeepCapturedKeyOrder(t *testing.T) {
	golden := loadDesktopRendererSessionGolden(t)
	identity := rendererSessionTestIdentity()
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	built := map[string]any{
		FactRendererSessionPage:           buildRendererSessionPage(identity),
		FactRendererShellPage:             buildRendererShellPage(identity),
		FactRendererSessionOpened:         buildRendererSessionOpened(identity),
		FactRendererSessionMetaResolved:   buildRendererSessionMetaResolved(identity, facts),
		FactRendererPageViewed:            buildDesktopRendererPageViewed(identity, "anon", rendererShellPageRoute),
		FactRendererPermissionModeChanged: buildRendererPermissionModeChanged(identity, "default", "plan"),
		FactRendererSessionsHeartbeatCheckBatch: buildRendererSessionsHeartbeatCheckBatch(rendererShellBaseProperties{
			AccountUUID: identity.AccountUUID, OrganizationUUID: identity.OrganizationUUID, BillingType: identity.BillingType,
			Surface: rendererShellSurface, DeploymentMode: rendererShellDeploymentMode, AppVersion: identity.AppVersion, Version: 1,
		}, SessionHeartbeatBatch{Sent: 8, ProbeDispatched: 7, NoWorker: 1}),
	}
	checked := 0
	for _, expected := range golden.Events {
		value, ok := built[expected.Fact]
		if !ok || expected.Boundary != nil {
			continue
		}
		checked++
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if got := jsonObjectKeys(t, encoded); strings.Join(got, ",") != strings.Join(expected.PropertyKeys, ",") {
			t.Fatalf("%s key order = %v, golden %v", expected.EventName, got, expected.PropertyKeys)
		}
		for key, want := range expected.Constants {
			wantEncoded, _ := json.Marshal(want)
			if got := gjson.GetBytes(encoded, key).Raw; got != string(wantEncoded) {
				t.Fatalf("%s.%s = %s, pinned constant %s", expected.EventName, key, got, wantEncoded)
			}
		}
	}
	if checked != 7 {
		t.Fatalf("golden pins %d executable events, want 7", checked)
	}
}

func TestDesktopRendererSessionOpenDeliversSegmentAndDesktopCopies(t *testing.T) {
	golden := loadDesktopRendererSessionGolden(t)
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
	bundle.AuxiliaryTelemetry.Segment.Events[FactRendererSessionPage] = claudeprofile.TelemetryEventProfile{EventName: rendererSessionPageName}
	bundle.AuxiliaryTelemetry.Segment.Events[FactRendererSessionOpened] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.session.opened"}
	bundle.AuxiliaryTelemetry.Segment.Events[FactRendererSessionMetaResolved] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.session.meta_resolved"}
	bundle.Telemetry.Events[FactRendererSessionOpened] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.session.opened"}
	bundle.Telemetry.Events[FactRendererSessionMetaResolved] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.session.meta_resolved"}
	bundle.AuxiliaryTelemetry.Segment.Events[FactRendererShellPage] = claudeprofile.TelemetryEventProfile{EventName: rendererShellPageName}
	bundle.AuxiliaryTelemetry.Segment.Events[FactRendererPermissionModeChanged] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.code.permission_mode.changed"}
	bundle.Telemetry.Events[FactRendererPageViewed] = claudeprofile.TelemetryEventProfile{EventName: "page_viewed"}
	bundle.Telemetry.Events[FactRendererPermissionModeChanged] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.code.permission_mode.changed"}
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
	before := manager.Status().LiveEmitterCoverage
	// Application activation: the Segment identify runs the renderer
	// activation hooks (shell page call + page_viewed copy).
	segmentWorker, errSegmentWorker := manager.workerForDelivery(auth, manager.auxiliaryDeliveries[segmentRole])
	if errSegmentWorker != nil {
		t.Fatal(errSegmentWorker)
	}
	if errIdentify := manager.ensureSegmentIdentified(segmentWorker); errIdentify != nil {
		t.Fatal(errIdentify)
	}
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	// Three turns of the same session: the session-open analytics belong to
	// the first turn only; the third turn switches the permission mode.
	for turn := 0; turn < 3; turn++ {
		turnFacts := facts
		turnFacts.PermissionMode = "default"
		if turn == 2 {
			turnFacts.PermissionMode = "plan"
		}
		span := manager.BeginRequest(context.Background(), auth, turnFacts)
		// Request hook insertion point (manager.go BeginRequest, see W6.wiring.md).
		manager.runRendererRequestHooks(context.Background(), span)
		span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`), http.Header{})
		span.ObserveFirstByte(clock.Now().Add(150 * time.Millisecond))
		span.ObserveResponse("req_renderer_session", "end_turn")
		span.FinishSuccess(context.Background())
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	captureMu.Lock()
	rendererRequests := append([]recordedRequest(nil), captured[rendererRole]...)
	segmentRequests := append([]recordedRequest(nil), captured[segmentRole]...)
	captureMu.Unlock()

	type segmentItem struct {
		properties  string
		anonymousID string
		kind        string
		count       int
	}
	segmentItems := make(map[string]*segmentItem)
	var segmentOrder []string
	for _, request := range segmentRequests {
		for _, item := range gjson.GetBytes(request.Body, "batch").Array() {
			kind := item.Get("type").String()
			name := item.Get("event").String()
			if kind == "page" {
				name = item.Get("name").String()
			}
			if name == "" {
				continue
			}
			if existing, ok := segmentItems[name]; ok {
				existing.count++
				continue
			}
			segmentItems[name] = &segmentItem{properties: item.Get("properties").Raw, anonymousID: item.Get("anonymousId").String(), kind: kind, count: 1}
			segmentOrder = append(segmentOrder, name)
		}
	}
	desktopCopies := make(map[string][]string)
	for _, request := range rendererRequests {
		for _, event := range gjson.GetBytes(request.Body, "events").Array() {
			if event.Get("event_type").String() != desktopRendererEventType {
				continue
			}
			name := event.Get("event_data.event_name").String()
			if event.Get("event_data.account_uuid").String() != testAccountA || event.Get("event_data.organization_uuid").String() != testOrgA {
				t.Fatalf("Desktop copy of %q carries the wrong account envelope", name)
			}
			desktopCopies[name] = append(desktopCopies[name], event.Get("event_data.properties").String())
		}
	}
	// Captured order inside the session-open batch: page, opened, meta_resolved.
	wantOrder := []string{rendererSessionPageName, "claudeai.epitaxy.session.opened", "claudeai.epitaxy.session.meta_resolved"}
	var gotOrder []string
	for _, name := range segmentOrder {
		for _, want := range wantOrder {
			if name == want {
				gotOrder = append(gotOrder, name)
			}
		}
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("Segment session-open order = %v, want %v", gotOrder, wantOrder)
	}
	// page_viewed copies: one per successful Segment page call, in activation
	// (shell) then session-open order, with the pinned route markers.
	pageViewed := desktopCopies["page_viewed"]
	if len(pageViewed) != 2 {
		t.Fatalf("page_viewed copies = %d, want 2: %v", len(pageViewed), pageViewed)
	}
	for index, route := range []rendererPageRoute{rendererShellPageRoute, rendererSessionPageRoute} {
		wantViewed, _ := json.Marshal(buildDesktopRendererPageViewed(rendererSessionIdentity{AccountUUID: testAccountA, OrganizationUUID: testOrgA, BillingType: gjson.Get(pageViewed[index], "billing_type").String(), AppVersion: "1.40609.0"}, segmentItems[rendererShellPageName].anonymousID, route))
		if pageViewed[index] != string(wantViewed) {
			t.Fatalf("page_viewed copy %d = %s, want %s", index, pageViewed[index], wantViewed)
		}
	}
	for _, expected := range golden.Events {
		if expected.Boundary != nil || expected.Fact == FactRendererPageViewed || expected.Fact == FactRendererSessionsHeartbeatCheckBatch {
			continue
		}
		item := segmentItems[expected.EventName]
		if item == nil {
			t.Fatalf("Segment %s %q was not delivered; got %v", expected.SegmentType, expected.EventName, segmentOrder)
		}
		if item.count != 1 {
			t.Fatalf("Segment %q was delivered %d times for one session, want once", expected.EventName, item.count)
		}
		if item.kind != expected.SegmentType {
			t.Fatalf("Segment %q type = %s, want %s", expected.EventName, item.kind, expected.SegmentType)
		}
		if got := jsonObjectKeys(t, []byte(item.properties)); strings.Join(got, ",") != strings.Join(expected.PropertyKeys, ",") {
			t.Fatalf("Segment %q wire key order = %v, golden %v", expected.EventName, got, expected.PropertyKeys)
		}
		if gjson.Get(item.properties, "account_uuid").String() != testAccountA || gjson.Get(item.properties, "organization_uuid").String() != testOrgA {
			t.Fatalf("Segment %q account properties are wrong: %s", expected.EventName, item.properties)
		}
		if expected.Fact == FactRendererSessionMetaResolved && gjson.Get(item.properties, "session_id").String() != rendererRecordID(facts) {
			t.Fatalf("meta_resolved session_id = %s", item.properties)
		}
		if expected.Fact == FactRendererPermissionModeChanged {
			if gjson.Get(item.properties, "previous_mode").String() != "default" || gjson.Get(item.properties, "current_mode").String() != "plan" || gjson.Get(item.properties, "surface").String() != rendererPermissionSurface {
				t.Fatalf("permission_mode.changed properties = %s", item.properties)
			}
		}
		copies := desktopCopies[expected.EventName]
		if len(expected.Roles) == 1 {
			if len(copies) != 0 {
				t.Fatalf("%q must not have a Desktop copy: %v", expected.EventName, copies)
			}
			continue
		}
		if len(copies) != 1 {
			t.Fatalf("Desktop copy of %q delivered %d times, want once: %v", expected.EventName, len(copies), copies)
		}
		want := strings.TrimSuffix(item.properties, "}") + `,"_dual_fire":true,"anonymous_id":"` + item.anonymousID + `","service_name":"claude_ai","path":"` + expected.DesktopPath + `"}`
		if copies[0] != want {
			t.Fatalf("Desktop copy of %q = %s, want Segment twin plus suffix %s", expected.EventName, copies[0], want)
		}
	}
	after := manager.Status().LiveEmitterCoverage
	if after.LiveEndpointEventCount != before.LiveEndpointEventCount {
		t.Fatalf("coverage changed during the request: before=%d after=%d", before.LiveEndpointEventCount, after.LiveEndpointEventCount)
	}
	executable := manager.executableEndpointEvents()
	for _, pair := range [][2]string{
		{segmentRole, rendererSessionPageName}, {segmentRole, rendererShellPageName}, {segmentRole, "claudeai.epitaxy.session.opened"}, {segmentRole, "claudeai.epitaxy.session.meta_resolved"}, {segmentRole, "claudeai.code.permission_mode.changed"},
		{rendererRole, "claudeai.epitaxy.session.opened"}, {rendererRole, "claudeai.epitaxy.session.meta_resolved"}, {rendererRole, "page_viewed"}, {rendererRole, "claudeai.code.permission_mode.changed"},
	} {
		if _, ok := executable[pair[0]+"\x00"+pair[1]]; !ok {
			t.Fatalf("%s %q is not counted as executable", pair[0], pair[1])
		}
	}
	t.Logf("live names=%d endpoint_events=%d/303 gaps=%d", after.LiveEventNameCount, after.LiveEndpointEventCount, 303-after.LiveEndpointEventCount)
}
