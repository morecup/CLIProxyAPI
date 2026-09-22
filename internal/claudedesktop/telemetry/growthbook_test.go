package telemetry

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestGrowthbookNativeEnvelopeShape(t *testing.T) {
	at := time.Date(2026, 9, 6, 18, 0, 0, 123000000, time.UTC)
	binding := Binding{DeviceID: testDeviceA, AccountUUID: testAccountA, OrganizationUUID: testOrgA}
	p := projectGrowthbookExposure(binding, features.Exposure{SessionID: "sdk-session", FeatureID: "flag", ExperimentID: "experiment", VariationID: 1.5}, "2.1.247", "event", at)
	var decoded map[string]any
	if json.Unmarshal(p, &decoded) != nil {
		t.Fatal("invalid JSON")
	}
	if gjson.GetBytes(p, "event_type").String() != "GrowthbookExperimentEvent" {
		t.Fatal("wrong envelope type")
	}
	data := decoded["event_data"].(map[string]any)
	for _, field := range []string{"event_name", "client_timestamp", "model", "betas", "process", "additional_metadata", "entrypoint", "prompt_id"} {
		if _, ok := data[field]; ok {
			t.Fatalf("invented field %s", field)
		}
	}
	if len(data) != 10 || data["variation_id"] != float64(2) || data["timestamp"] != "2026-09-06T18:00:00.123Z" || data["session_id"] != "sdk-session" || data["user_attributes"] != `{"appVersion":"2.1.247"}` || data["experiment_metadata"] != `{"feature_id":"flag"}` {
		t.Fatalf("wrong event fields: %s", p)
	}
	optional := projectGrowthbookExposure(Binding{}, features.Exposure{ExperimentID: ""}, "", "event", at)
	for _, field := range []string{"auth", "device_id", "session_id", "user_attributes"} {
		if gjson.GetBytes(optional, "event_data."+field).Exists() {
			t.Fatalf("invented optional %s", field)
		}
	}
}

func TestGrowthbookNativeRoundingAndStringification(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1.5, "2"}, {-1.5, "-1"}, {-0.5, "0"}, {math.Copysign(0, -1), "0"}, {0.49999999999999994, "0"},
		{4503599627370495.5, "4503599627370496"}, {9007199254740992, "9007199254740992"}, {1e21, "1e+21"},
		{math.Inf(1), "null"}, {math.Inf(-1), "null"}, {math.NaN(), "null"},
	} {
		p := projectGrowthbookExposure(Binding{}, features.Exposure{VariationID: tc.in}, "", "id", time.Now())
		if got := gjson.GetBytes(p, "event_data.variation_id").Raw; got != tc.want {
			t.Errorf("%v => %s, want %s", tc.in, got, tc.want)
		}
	}
	feature := "quote\"\\\n\t\x00<>&\u2028\u2029😀\\u003c"
	p := projectGrowthbookExposure(Binding{}, features.Exposure{FeatureID: feature}, "2.1.247", "id", time.Now())
	metadata := gjson.GetBytes(p, "event_data.experiment_metadata").String()
	if !json.Valid(p) || !json.Valid([]byte(metadata)) || gjson.Get(metadata, "feature_id").String() != feature {
		t.Fatal("native strings did not round trip")
	}
	if !strings.Contains(string(p), "<>&\u2028\u2029😀") || strings.Contains(metadata, `\u003c`) && !strings.Contains(metadata, `\\u003c`) {
		t.Fatal("native string escaping changed")
	}
}

func TestGrowthbookUsesIndependentProtectedSDKQueueAndWire(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 6, 18, 0, 0, 123000000, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(b *claudeprofile.Bundle) {
		b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	accepted, err := m.ObserveSDKFeatureExposure(auth, features.Exposure{SessionID: "sdk-session", FeatureID: "flag<>&\u2028", ExperimentID: "exp", VariationID: 0})
	if !accepted || err != nil {
		t.Fatalf("ack=%v err=%v", accepted, err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("exposure invented lifecycle or mirror requests: %d", len(requests))
	}
	r := requests[0]
	if !strings.Contains(string(r.Body), "flag<>&\u2028") {
		t.Fatal("queue or batch encoder rewrote native exposure strings")
	}
	if r.URL != m.sdkDelivery.endpoint || gjson.GetBytes(r.Body, "events.0.event_type").String() != "GrowthbookExperimentEvent" || gjson.GetBytes(r.Body, "events.#").Int() != 1 {
		t.Fatalf("wrong SDK destination or batch: %s", r.URL)
	}
	if r.Header.Get("User-Agent") != "claude-code/"+m.bundle.CodeVersion || r.Header.Get("Cookie") != "" || r.Header.Get("X-Cc-Atis") != "" {
		t.Fatal("exposure used main or Renderer headers")
	}
}

func TestGrowthbookQueueFailureIsRetainedAndExactRetryRepairs(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Now()}
	doer := &testDoer{}
	makeManager := func() *Manager {
		return newTelemetryTestManager(t, root, clock, doer, func(b *claudeprofile.Bundle) {
			b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		})
	}
	m := makeManager()
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	worker.profile.batch.MaxPendingEvents = 1
	first := features.Exposure{SessionID: "session", FeatureID: "first", ExperimentID: "exp"}
	second := features.Exposure{SessionID: "session", FeatureID: "second", ExperimentID: "exp"}
	if ok, err := m.ObserveSDKFeatureExposure(auth, first); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := m.ObserveSDKFeatureExposure(auth, second); ok || err == nil {
		t.Fatal("full queue acknowledged exposure")
	}
	if issue := worker.factIssueSnapshot(); issue == nil || issue.featureUnresolved != 1 {
		t.Fatalf("queue loss invisible: %+v", issue)
	}
	m.Quarantine()
	m = makeManager()
	worker, err = m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	if issue := worker.factIssueSnapshot(); issue == nil || issue.featureUnresolved != 1 {
		t.Fatalf("failure lost at restart: %+v", issue)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if issue := worker.factIssueSnapshot(); issue == nil {
		t.Fatal("HTTP success erased exposure failure")
	}
	if ok, err := m.ObserveSDKFeatureExposure(auth, second); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if issue := worker.factIssueSnapshot(); issue != nil {
		t.Fatalf("exact retry did not resolve: %+v", issue)
	}
}

func TestGrowthbookNoBatchAcknowledgesButClosedConfiguredLoggerDoesNot(t *testing.T) {
	var absent *Manager
	if ok, err := absent.ObserveSDKFeatureExposure(nil, features.Exposure{}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
	m.Close()
	if ok, err := m.ObserveSDKFeatureExposure(nil, features.Exposure{}); ok || err == nil {
		t.Fatal("closed configured logger acknowledged")
	}
}

func TestGrowthbookCacheHealthSurvivesUnrelatedSuccess(t *testing.T) {
	m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := m.ObserveSDKFeatureState(auth, "", false); err != nil {
		t.Fatal(err)
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m.ObserveSDKFeatureExposure(auth, features.Exposure{FeatureID: "flag"}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if issue := worker.factIssueSnapshot(); issue == nil || !strings.Contains(issue.Reason, "feature cache") {
		t.Fatalf("feature state loss hidden: %+v", issue)
	}
	status := m.Status()
	found := false
	for _, endpoint := range status.DeliveryEndpoints {
		if endpoint.Role == m.sdkDelivery.endpointRole {
			found = true
			if endpoint.Status != "awaiting-sdk-feature-facts" {
				t.Fatalf("feature state not visible: %+v", endpoint)
			}
		}
	}
	if !found {
		t.Fatal("SDK endpoint missing")
	}
	if err := m.ObserveSDKFeatureState(auth, "", true); err != nil {
		t.Fatal(err)
	}
	if issue := worker.factIssueSnapshot(); issue != nil {
		t.Fatalf("matching recovery did not clear: %+v", issue)
	}
}

func TestGrowthbookOAuthUsesCurrentAccountSnapshotAndRotation(t *testing.T) {
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Attributes = map[string]string{cliproxyauth.AttributeAPIKey: "current-oauth", cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}
	for _, token := range []string{"current-oauth", "rotated-oauth"} {
		auth = auth.Clone()
		auth.Attributes[cliproxyauth.AttributeAPIKey] = token
		if ok, err := m.ObserveSDKFeatureExposure(auth, features.Exposure{FeatureID: token, ExperimentID: "exp"}); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	requests := doer.Requests()
	if len(requests) != 2 || requests[0].Header.Get("Authorization") != "Bearer current-oauth" || requests[1].Header.Get("Authorization") != "Bearer rotated-oauth" {
		t.Fatal("telemetry used stale metadata instead of current OAuth snapshot")
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	missing := auth.Clone()
	delete(missing.Attributes, cliproxyauth.AttributeAPIKey)
	delete(missing.Metadata, "access_token")
	worker.updateAuth(missing)
	if _, err := worker.authorizationHeader(); err == nil {
		t.Fatal("missing OAuth credential fabricated")
	}
}
