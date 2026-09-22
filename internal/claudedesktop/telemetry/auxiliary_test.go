package telemetry

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDatadogMetadataUsesCapturedFieldNames(t *testing.T) {
	got := datadogMetadataFields(map[string]any{
		"durationMs": 120, "costUSD": 0.25, "isTTY": false,
		"cc_prompt_id": "prompt-test", "baseUrl": "https://private.invalid", "stop_reason": "end_turn",
	})
	for key, want := range map[string]any{"duration_ms": 120, "cost_u_s_d": 0.25, "is_t_t_y": false, "prompt_id": "prompt-test", "stop_reason": "end_turn"} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	for _, key := range []string{"durationMs", "costUSD", "isTTY", "cc_prompt_id", "baseUrl", "base_url"} {
		if _, ok := got[key]; ok {
			t.Errorf("SDK-only field retained: %s", key)
		}
	}
	if got := datadogMetadataFields(map[string]any{"cc_prompt_id": ""}); len(got) != 0 {
		t.Fatal("empty prompt identifier was fabricated")
	}
}

func TestDatadogModelDimensionOmitsOnlyDatedRevision(t *testing.T) {
	for input, want := range map[string]string{
		"claude-haiku-4-5-20251001": "claude-haiku-4-5", "claude-sonnet-5": "claude-sonnet-5",
		"claude-opus-5": "claude-opus-5", "claude-test-abcdefgh": "claude-test-abcdefgh",
	} {
		if got := datadogModelName(input); got != want {
			t.Errorf("%s -> %s, want %s", input, got, want)
		}
	}
}

func TestAuxiliaryDeliveryBodyFormatsHeadersQueriesAndCompression(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle})
	t.Cleanup(manager.Close)
	materials := auxiliaryTestMaterials()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	segmentProfile := manager.auxiliaryDeliveries[segmentRole]
	segmentEncoded, errSegment := encodeDeliveryBatch(segmentProfile, []Envelope{{Payload: json.RawMessage(`{"event":"claudeai.code.message.submitted","type":"track"}`)}}, map[string]string{"write_key": materials.SegmentWriteKey}, now)
	if errSegment != nil {
		t.Fatal(errSegment)
	}
	var segmentBody struct {
		WriteKey string           `json:"writeKey"`
		Batch    []map[string]any `json:"batch"`
		SentAt   string           `json:"sentAt"`
	}
	if errDecode := json.Unmarshal(segmentEncoded.body, &segmentBody); errDecode != nil {
		t.Fatal(errDecode)
	}
	if segmentBody.WriteKey != materials.SegmentWriteKey || len(segmentBody.Batch) != 1 || segmentBody.Batch[0]["writeKey"] != materials.SegmentWriteKey || segmentBody.SentAt == "" {
		t.Fatalf("Segment wrapper does not match the captured contract")
	}

	logsProfile := manager.auxiliaryDeliveries[datadogLogsRole]
	logsEncoded, errLogs := encodeDeliveryBatch(logsProfile, []Envelope{{Payload: json.RawMessage(`{"message":"tengu_api_success"}`)}}, map[string]string{"api_key": materials.DatadogLogsAPIKey}, now)
	if errLogs != nil {
		t.Fatal(errLogs)
	}
	var logsBody []map[string]any
	if errDecode := json.Unmarshal(logsEncoded.body, &logsBody); errDecode != nil || len(logsBody) != 1 || logsBody[0]["message"] != "tengu_api_success" {
		t.Fatalf("Datadog logs body does not match the captured array contract: %v", errDecode)
	}
	logsRequest := newAuxiliaryTestRequest(t, logsProfile, logsEncoded)
	if errApply := applyAuxiliaryRequest(logsRequest, logsProfile, map[string]string{"api_key": materials.DatadogLogsAPIKey}, logsEncoded); errApply != nil {
		t.Fatal(errApply)
	}
	if logsRequest.Header.Get("DD-API-KEY") != materials.DatadogLogsAPIKey || logsRequest.URL.RawQuery != "" {
		t.Fatal("Datadog logs authentication was not isolated to its header")
	}

	browserLogsProfile := manager.auxiliaryDeliveries[datadogLogsBrowserRole]
	browserLogsEncoded, errBrowserLogs := encodeDeliveryBatch(browserLogsProfile, []Envelope{{Payload: json.RawMessage(`{"status":"error"}`)}}, map[string]string{"api_key": materials.DatadogLogsAPIKey}, now)
	if errBrowserLogs != nil {
		t.Fatal(errBrowserLogs)
	}
	browserLogsRequest := newAuxiliaryTestRequest(t, browserLogsProfile, browserLogsEncoded)
	if errApply := applyAuxiliaryRequest(browserLogsRequest, browserLogsProfile, map[string]string{"api_key": materials.DatadogLogsAPIKey}, browserLogsEncoded); errApply != nil {
		t.Fatal(errApply)
	}
	if browserLogsRequest.Header.Get("DD-API-KEY") != "" || queryValue(t, browserLogsRequest.URL.String(), "dd-api-key") != materials.DatadogLogsAPIKey {
		t.Fatal("Datadog browser logs authentication did not stay isolated to the captured query contract")
	}
	for _, key := range []string{"ddsource", "dd-evp-origin", "dd-evp-origin-version", "dd-request-id"} {
		if queryValue(t, browserLogsRequest.URL.String(), key) == "" {
			t.Fatalf("Datadog browser logs query omits %q", key)
		}
	}

	rumProfile := manager.auxiliaryDeliveries[datadogRUMRole]
	rumPayload := json.RawMessage(`{"type":"action","message":"` + strings.Repeat("r", 20*1024) + `"}`)
	rumEncoded, errRUM := encodeDeliveryBatch(rumProfile, []Envelope{{Payload: rumPayload}}, map[string]string{
		"client_token":   materials.DatadogRUMClientToken,
		"application_id": materials.DatadogRUMApplicationID,
	}, now)
	if errRUM != nil {
		t.Fatal(errRUM)
	}
	if rumEncoded.contentEncoding != "deflate" || rumEncoded.query.Get("dd-api-key") != materials.DatadogRUMClientToken || rumEncoded.query.Get("dd-evp-encoding") != "deflate" {
		t.Fatal("Datadog RUM query or compression does not match the captured contract")
	}
	rumDecoded := inflateTestBody(t, rumEncoded.body)
	if !bytes.Equal(rumDecoded, append(append([]byte(nil), rumPayload...), '\n')) {
		t.Fatal("Datadog RUM deflate body did not decode to NDJSON")
	}

	sentryProfile := manager.auxiliaryDeliveries[sentryRole]
	sentryPayload := json.RawMessage(`{"type":"Error","message":"` + strings.Repeat("s", 70*1024) + `"}`)
	sentryEncoded, errSentry := encodeDeliveryBatch(sentryProfile, []Envelope{{CatalogEvent: "event", Payload: sentryPayload}}, map[string]string{"public_key": materials.SentryPublicKey}, now)
	if errSentry != nil {
		t.Fatal(errSentry)
	}
	if sentryEncoded.contentEncoding != "gzip" || sentryEncoded.query.Get("sentry_key") != materials.SentryPublicKey || sentryEncoded.query.Get("sentry_version") != "7" {
		t.Fatal("Sentry query or compression does not match the captured contract")
	}
	sentryDecoded := gunzipTestBody(t, sentryEncoded.body)
	if !bytes.Contains(sentryDecoded, []byte(`"type":"event"`)) || !bytes.Contains(sentryDecoded, []byte(`"dsn":"https://`+materials.SentryPublicKey+`@o1158394.ingest.us.sentry.io/4507368973008896"`)) {
		t.Fatal("Sentry envelope header or item header is incomplete")
	}
}

func TestAuxiliaryEndpointTransportsAndCredentialsAreIsolated(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, batch := range []*claudeprofile.TelemetryBatchProfile{
		&bundle.Telemetry.Batch,
		&bundle.SDKTelemetry.Batch,
		&bundle.AuxiliaryTelemetry.Segment.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogs.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogsBrowser.Batch,
		&bundle.AuxiliaryTelemetry.DatadogRUM.Batch,
		&bundle.AuxiliaryTelemetry.Sentry.Batch,
	} {
		batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		batch.JitterMinimum = 1
		batch.JitterMaximum = 1
	}
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	var captureMu sync.Mutex
	captured := make(map[string][]recordedRequest)
	rendererRole := bundle.Telemetry.EndpointRole
	sdkRole := bundle.SDKTelemetry.EndpointRole
	manager := NewManager(Options{
		StatePath: t.TempDir(),
		Bundle:    bundle,
		Now:       clock.Now,
		RandomFloat: func() float64 {
			return 0
		},
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
	materials := auxiliaryTestMaterials()
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = materials
	if errActivate := manager.Activate(auth); errActivate != nil {
		t.Fatalf("activate application telemetry: %v", errActivate)
	}
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`), http.Header{
		"Cookie":        {"caller-cookie"},
		"Authorization": {"Bearer caller-secret"},
		"X-Caller-Only": {"caller-value"},
	})
	span.RecordScheduledRetry(context.Background(), 1, 250*time.Millisecond, telemetryStatusError{status: http.StatusBadGateway})
	span.ObserveFirstByte(clock.Now().Add(150 * time.Millisecond))
	span.ObserveResponse("req_auxiliary", "end_turn")
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush all endpoint roles: %v", errFlush)
	}

	wantRoles := []string{rendererRole, sdkRole, segmentRole, datadogLogsRole, datadogLogsBrowserRole, datadogRUMRole, sentryRole}
	sort.Strings(wantRoles)
	captureMu.Lock()
	gotRoles := make([]string, 0, len(captured))
	for role := range captured {
		gotRoles = append(gotRoles, role)
	}
	sort.Strings(gotRoles)
	requests := make(map[string]recordedRequest, len(captured))
	for role, roleRequests := range captured {
		if len(roleRequests) > 0 {
			requests[role] = roleRequests[0]
		}
	}
	captureMu.Unlock()
	if strings.Join(gotRoles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("delivered roles = %v, want %v", gotRoles, wantRoles)
	}
	for role, request := range requests {
		if request.Header.Get("Cookie") != "" || request.Header.Get("X-Caller-Only") != "" || request.Header.Get("Authorization") == "Bearer caller-secret" {
			t.Fatalf("endpoint %q inherited caller credentials", role)
		}
	}
	if requests[sdkRole].Header.Get("Authorization") != "Bearer sensitive-access-token" {
		t.Fatal("SDK event logger did not use its OAuth bearer credential")
	}
	for _, role := range []string{rendererRole, segmentRole, datadogLogsRole, datadogLogsBrowserRole, datadogRUMRole, sentryRole} {
		if requests[role].Header.Get("Authorization") != "" {
			t.Fatalf("endpoint %q inherited SDK OAuth authorization", role)
		}
	}
	if requests[datadogLogsRole].Header.Get("DD-API-KEY") != materials.DatadogLogsAPIKey {
		t.Fatal("Datadog logs did not receive its dedicated API key")
	}
	if queryValue(t, requests[datadogLogsBrowserRole].URL, "dd-api-key") != materials.DatadogLogsAPIKey || requests[datadogLogsBrowserRole].Header.Get("DD-API-KEY") != "" {
		t.Fatal("Datadog browser logs did not use its query-bound logs key")
	}
	for _, role := range []string{rendererRole, sdkRole, segmentRole, datadogLogsBrowserRole, datadogRUMRole, sentryRole} {
		if requests[role].Header.Get("DD-API-KEY") != "" {
			t.Fatalf("endpoint %q inherited the Datadog logs key", role)
		}
	}
	if queryValue(t, requests[datadogRUMRole].URL, "dd-api-key") != materials.DatadogRUMClientToken {
		t.Fatal("Datadog RUM did not receive its dedicated client token")
	}
	if queryValue(t, requests[sentryRole].URL, "sentry_key") != materials.SentryPublicKey {
		t.Fatal("Sentry did not receive its dedicated public key")
	}
	if !bytes.Contains(requests[segmentRole].Body, []byte(materials.SegmentWriteKey)) {
		t.Fatal("Segment did not receive its dedicated write key")
	}
}

func TestDatadogRUMViewLifecycleStartsOnceAndStopsOnManagerClose(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, batch := range []*claudeprofile.TelemetryBatchProfile{
		&bundle.Telemetry.Batch,
		&bundle.SDKTelemetry.Batch,
		&bundle.AuxiliaryTelemetry.Segment.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogs.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogsBrowser.Batch,
		&bundle.AuxiliaryTelemetry.DatadogRUM.Batch,
		&bundle.AuxiliaryTelemetry.Sentry.Batch,
	} {
		batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		batch.JitterMinimum = 1
		batch.JitterMaximum = 1
	}
	var captureMu sync.Mutex
	var captured []recordedRequest
	sdkEndpointRole := bundle.SDKTelemetry.EndpointRole
	manager := NewManager(Options{
		StatePath: t.TempDir(),
		Bundle:    bundle,
		Now:       clock.Now,
		RandomFloat: func() float64 {
			return 0
		},
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				captureMu.Lock()
				captured = append(captured, recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
				captureMu.Unlock()
				proto, major, minor := "HTTP/2.0", 2, 0
				if role == sdkEndpointRole || role == datadogLogsRole || role == datadogLogsBrowserRole {
					proto, major, minor = "HTTP/1.1", 1, 1
				}
				return &http.Response{StatusCode: http.StatusOK, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
		},
	})
	t.Cleanup(manager.Close)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")

	first := manager.BeginRequest(context.Background(), auth, facts)
	clock.Advance(4 * time.Second)
	first.FinishSuccess(context.Background())
	second := manager.BeginRequest(context.Background(), auth, facts)
	clock.Advance(5 * time.Second)
	second.FinishSuccess(context.Background())
	clock.Advance(20 * time.Second)
	manager.Close()

	var lifecycle []map[string]any
	captureMu.Lock()
	requests := append([]recordedRequest(nil), captured...)
	captureMu.Unlock()
	for _, request := range requests {
		if !strings.Contains(request.URL, "/api/v2/rum") {
			continue
		}
		body := request.Body
		if request.Header.Get("Content-Encoding") == "deflate" {
			body = inflateTestBody(t, body)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
			var payload map[string]any
			if errDecode := json.Unmarshal(line, &payload); errDecode != nil {
				t.Fatalf("decode Datadog RUM payload: %v", errDecode)
			}
			view, _ := payload["view"].(map[string]any)
			if payload["type"] == "view" && view["is_active"] != nil {
				lifecycle = append(lifecycle, payload)
			}
		}
	}
	if len(lifecycle) != 2 {
		t.Fatalf("Datadog RUM lifecycle views = %d, want start and stop: %+v", len(lifecycle), lifecycle)
	}
	startView := lifecycle[0]["view"].(map[string]any)
	stopView := lifecycle[1]["view"].(map[string]any)
	if startView["is_active"] != true || startView["loading_type"] != "initial_load" {
		t.Fatalf("Datadog RUM start view = %+v", startView)
	}
	if stopView["is_active"] != false || stopView["loading_type"] != nil || stopView["time_spent"] != float64((29*time.Second).Nanoseconds()) {
		t.Fatalf("Datadog RUM stop view = %+v", stopView)
	}
	if startView["id"] == "" || startView["id"] != stopView["id"] {
		t.Fatalf("Datadog RUM view identity changed: start=%+v stop=%+v", startView, stopView)
	}
	startSession := lifecycle[0]["session"].(map[string]any)
	stopSession := lifecycle[1]["session"].(map[string]any)
	if startSession["id"] == "" || startSession["id"] != stopSession["id"] {
		t.Fatalf("Datadog RUM session identity changed: start=%+v stop=%+v", startSession, stopSession)
	}
}

func TestMissingAuxiliaryMaterialsAreVisibleWithoutDisablingCoreTelemetry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	if !span.Active() || span.worker == nil || span.sdkWorker == nil {
		t.Fatal("missing auxiliary materials disabled renderer or SDK telemetry")
	}
	statusByRole := make(map[string]DeliveryEndpointStatus)
	for _, endpoint := range manager.Status().DeliveryEndpoints {
		statusByRole[endpoint.Role] = endpoint
	}
	for _, role := range []string{segmentRole, datadogLogsRole, datadogLogsBrowserRole, datadogRUMRole, sentryRole} {
		endpoint := statusByRole[role]
		if endpoint.Status != "awaiting-enrollment-material" || endpoint.Reason == "" {
			t.Fatalf("endpoint %q status = %+v", role, endpoint)
		}
	}
}

func TestSentryRuntimeSessionUsesApplicationLifetimeAndCrashKeepsMarker(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	const firstAppSession = "11111111-aaaa-4111-8111-111111111111"
	const secondAppSession = "22222222-bbbb-4222-8222-222222222222"
	newManager := func(appSessionID string, doer *testDoer) *Manager {
		return NewManager(Options{
			StatePath: root, Bundle: bundle, ApplicationSessionID: appSessionID,
			DoerFactory: func(string) HTTPDoer { return doer },
			Now:         clock.Now, RandomFloat: func() float64 { return 0 },
		})
	}

	first := newManager(firstAppSession, &testDoer{})
	if errActivate := first.Activate(auth); errActivate != nil {
		t.Fatalf("activate first application lifetime: %v", errActivate)
	}
	firstWorker, errFirstWorker := first.workerForDelivery(auth, first.auxiliaryDeliveries[sentryRole])
	if errFirstWorker != nil {
		t.Fatalf("resolve first Sentry worker: %v", errFirstWorker)
	}
	if firstWorker == nil {
		t.Fatal("first application lifetime did not create a Sentry worker")
	}
	firstMarker := firstWorker.sentryRuntimeMarkerPath()
	if _, errStat := os.Stat(firstMarker); errStat != nil {
		t.Fatalf("first Sentry runtime marker is missing: %v", errStat)
	}
	firstPayload, _, errFirstPayload := first.projectSentrySession(firstWorker, true)
	if errFirstPayload != nil {
		t.Fatal(errFirstPayload)
	}
	var firstSession map[string]any
	if errDecode := json.Unmarshal(firstPayload, &firstSession); errDecode != nil {
		t.Fatal(errDecode)
	}
	if firstSession["sid"] != firstAppSession {
		t.Fatalf("first Sentry sid = %v", firstSession["sid"])
	}
	first.Close()
	if _, errStat := os.Stat(firstMarker); !os.IsNotExist(errStat) {
		t.Fatalf("normal shutdown retained Sentry marker: %v", errStat)
	}

	second := newManager(secondAppSession, &testDoer{errors: []error{errors.New("offline")}})
	if errActivate := second.Activate(auth); errActivate != nil {
		t.Fatalf("activate second application lifetime: %v", errActivate)
	}
	secondWorker, errSecondWorker := second.workerForDelivery(auth, second.auxiliaryDeliveries[sentryRole])
	if errSecondWorker != nil {
		t.Fatalf("resolve second Sentry worker: %v", errSecondWorker)
	}
	if secondWorker == nil {
		t.Fatal("second application lifetime did not create a Sentry worker")
	}
	secondMarker := secondWorker.sentryRuntimeMarkerPath()
	secondPayload, _, errSecondPayload := second.projectSentrySession(secondWorker, true)
	if errSecondPayload != nil {
		t.Fatal(errSecondPayload)
	}
	var secondSession map[string]any
	if errDecode := json.Unmarshal(secondPayload, &secondSession); errDecode != nil {
		t.Fatal(errDecode)
	}
	if secondSession["sid"] != secondAppSession || secondSession["sid"] == firstSession["sid"] {
		t.Fatalf("Sentry application sessions were reused: first=%v second=%v", firstSession["sid"], secondSession["sid"])
	}
	pendingBeforeCrash := secondWorker.statusSnapshot().Pending
	if pendingBeforeCrash < 1 {
		t.Fatalf("Sentry runtime-start event was not durable before crash: %+v", secondWorker.statusSnapshot())
	}
	second.Quarantine()
	if _, errStat := os.Stat(secondMarker); errStat != nil {
		t.Fatalf("crash/quarantine cleared the unclean-exit marker: %v", errStat)
	}
	if pendingAfterCrash := secondWorker.statusSnapshot().Pending; pendingAfterCrash != pendingBeforeCrash {
		t.Fatalf("crash synthesized a Sentry stop event: before=%d after=%d", pendingBeforeCrash, pendingAfterCrash)
	}
}

func TestApplicationLifecycleEmitsCapturedSDKSegmentAndDatadogSequenceOnce(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, batch := range []*claudeprofile.TelemetryBatchProfile{
		&bundle.Telemetry.Batch,
		&bundle.SDKTelemetry.Batch,
		&bundle.AuxiliaryTelemetry.Segment.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogs.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogsBrowser.Batch,
		&bundle.AuxiliaryTelemetry.DatadogRUM.Batch,
		&bundle.AuxiliaryTelemetry.Sentry.Batch,
	} {
		batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		batch.JitterMinimum = 1
		batch.JitterMaximum = 1
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
	for _, name := range []string{"tengu_started", "tengu_init", "tengu_sdk_init_handshake", "tengu_shutdown_pending_state"} {
		if sdkNames[name] != 1 {
			t.Fatalf("SDK lifecycle event %q count = %d; all=%v", name, sdkNames[name], sdkNames)
		}
	}
	segmentTypes := capturedSegmentTypes(t, requests[segmentRole])
	if segmentTypes["identify"] != 1 {
		t.Fatalf("Segment identify count = %d; all=%v", segmentTypes["identify"], segmentTypes)
	}
	datadogMessages := capturedDatadogMessages(t, requests[datadogLogsRole])
	for _, name := range []string{"tengu_started", "tengu_init", "tengu_sdk_init_handshake", "tengu_shutdown_pending_state"} {
		if datadogMessages[name] != 1 {
			t.Fatalf("Datadog lifecycle event %q count = %d; all=%v", name, datadogMessages[name], datadogMessages)
		}
	}
	if datadogMessages["startup"] != 0 || datadogMessages["plugins_init"] != 0 {
		t.Fatalf("Datadog timer names were incorrectly used as event names: %v", datadogMessages)
	}
}

func capturedSDKEventNames(t *testing.T, requests []recordedRequest) map[string]int {
	t.Helper()
	result := make(map[string]int)
	for _, request := range requests {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatalf("decode SDK lifecycle batch: %v", errDecode)
		}
		for _, event := range batch.Events {
			result[event.EventData.EventName]++
		}
	}
	return result
}

func capturedSegmentTypes(t *testing.T, requests []recordedRequest) map[string]int {
	t.Helper()
	result := make(map[string]int)
	for _, request := range requests {
		var batch struct {
			Batch []map[string]any `json:"batch"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatalf("decode Segment lifecycle batch: %v", errDecode)
		}
		for _, event := range batch.Batch {
			if eventType, _ := event["type"].(string); eventType != "" {
				result[eventType]++
			}
		}
	}
	return result
}

func capturedDatadogMessages(t *testing.T, requests []recordedRequest) map[string]int {
	t.Helper()
	result := make(map[string]int)
	for _, request := range requests {
		var events []map[string]any
		if errDecode := json.Unmarshal(request.Body, &events); errDecode != nil {
			t.Fatalf("decode Datadog lifecycle batch: %v", errDecode)
		}
		for _, event := range events {
			if message, _ := event["message"].(string); message != "" {
				result[message]++
			}
		}
	}
	return result
}

func auxiliaryTestMaterials() claudedesktop.TelemetryMaterials {
	return claudedesktop.TelemetryMaterials{
		SegmentWriteKey:         "segment0123456789abcdef01234567",
		DatadogLogsAPIKey:       "datadoglogs0123456789abcdef01234567",
		DatadogRUMClientToken:   "datadogrum0123456789abcdef012345678",
		DatadogRUMApplicationID: "77777777-7777-4777-8777-777777777777",
		SentryPublicKey:         "abcdef0123456789abcdef0123456789",
	}
}

func newAuxiliaryTestRequest(t *testing.T, profile deliveryProfile, encoded encodedDeliveryBatch) *http.Request {
	t.Helper()
	request, errRequest := http.NewRequest(http.MethodPost, profile.endpoint, bytes.NewReader(encoded.body))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	for _, header := range profile.headers {
		request.Header.Set(header.Name, header.Value)
	}
	return request
}

func inflateTestBody(t *testing.T, payload []byte) []byte {
	t.Helper()
	reader, errReader := zlib.NewReader(bytes.NewReader(payload))
	if errReader != nil {
		t.Fatal(errReader)
	}
	defer func() { _ = reader.Close() }()
	decoded, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return decoded
}

func gunzipTestBody(t *testing.T, payload []byte) []byte {
	t.Helper()
	reader, errReader := gzip.NewReader(bytes.NewReader(payload))
	if errReader != nil {
		t.Fatal(errReader)
	}
	defer func() { _ = reader.Close() }()
	decoded, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return decoded
}

func queryValue(t *testing.T, rawURL, name string) string {
	t.Helper()
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	return parsed.Query().Get(name)
}
