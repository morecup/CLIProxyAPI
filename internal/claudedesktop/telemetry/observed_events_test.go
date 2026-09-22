package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestObservedExecutableCatalogDeliversEveryAddedEndpointPair(t *testing.T) {
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
		batch.JitterMinimum, batch.JitterMaximum = 1, 1
		batch.MaxEvents, batch.MaxPendingEvents, batch.MaxBytes = 1000, 2000, 32<<20
	}
	var mu sync.Mutex
	captured := make(map[string][]recordedRequest)
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		Now:         func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) },
		RandomFloat: func() float64 { return 0 },
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				mu.Lock()
				captured[role] = append(captured[role], recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
				mu.Unlock()
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
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()

	events := claudeprofile.V140609ObservedExecutableEvents()
	pairs := 0
	for _, event := range events {
		pairs += len(event.EndpointRoles)
		fields := validObservedTestFields(t, event)
		switch event.Source {
		case claudeprofile.ObservedEventSourceRenderer:
			errBundle = manager.RecordObservedRendererEvent(t.Context(), auth, ObservedRendererEvent{
				Kind: event.Kind, Route: validObservedTestRoute(t, event), SessionID: "session_observed", ClientRequestID: "request_observed",
				Properties: fields,
			})
		case claudeprofile.ObservedEventSourceMainProcess:
			errBundle = manager.RecordObservedMainProcessEvent(t.Context(), auth, ObservedMainProcessEvent{
				Kind: event.Kind, SessionID: "session_observed", ClientRequestID: "request_observed",
				Metadata: fields,
			})
		case claudeprofile.ObservedEventSourceSDK:
			errBundle = manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
				Kind: event.Kind, SessionID: "session_observed", Model: "claude-opus-5", PromptID: "prompt_observed",
				ClientRequestID: "request_observed", SkillName: validObservedTestSkillName(t, event), Metadata: fields,
			})
		case claudeprofile.ObservedEventSourcePerformance:
			errBundle = manager.RecordObservedPerformanceEvent(t.Context(), auth, ObservedPerformanceEvent{
				Kind: event.Kind, SessionID: "session_observed", ClientRequestID: "request_observed",
				Data: fields,
			})
		case claudeprofile.ObservedEventSourceCrash:
			errBundle = manager.RecordObservedCrashAttachment(t.Context(), auth, ObservedCrashAttachment{
				Filename: "01234567-89ab-cdef-0123-456789abcdef.dmp", Data: []byte("MDMP"), Metadata: map[string]any{"process_type": "renderer"},
			})
		default:
			t.Fatalf("event %q has source %q", event.Kind, event.Source)
		}
		if errBundle != nil {
			t.Fatalf("record %s/%s: %v", event.Source, event.Kind, errBundle)
		}
	}
	if len(events) != 124 || pairs != 162 {
		t.Fatalf("catalog = %d names / %d endpoint pairs, want 124 / 162", len(events), pairs)
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}

	mu.Lock()
	wires := make(map[string]map[string]bool, len(captured))
	for role, requests := range captured {
		wires[role] = make(map[string]bool)
		for _, request := range requests {
			for _, name := range observedWireEventNames(t, role, request) {
				wires[role][name] = true
			}
		}
	}
	mu.Unlock()
	for _, event := range events {
		for _, role := range event.EndpointRoles {
			if !wires[role][event.EventName] {
				t.Errorf("missing delivered pair %s/%s (%s)", role, event.EventName, event.Kind)
			}
		}
	}
	if t.Failed() {
		roles := make([]string, 0, len(wires))
		for role := range wires {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		t.Logf("captured roles: %v", roles)
	}
	coverage := manager.Status().LiveEmitterCoverage
	if coverage.Status != "complete" || coverage.LiveEventNameCount != 231 || coverage.LiveEndpointEventCount != 303 || coverage.UnmodeledEndpointEventCount != 0 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if coverage.PayloadContractStatus != "complete" || coverage.ObservedCompanionEventCount != 124 ||
		coverage.CapturedPayloadContractEventCount != 99 || coverage.SpecializedPayloadContractEventCount != 25 ||
		coverage.EndpointOnlyPayloadContractEventCount != 0 || coverage.UncapturedPayloadContractEventCount != 0 {
		t.Fatalf("payload contract coverage = %+v", coverage)
	}
}

func TestObservedExecutableCatalogRejectsFreeFormAndCredentials(t *testing.T) {
	manager := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	if err := manager.RecordObservedMainProcessEvent(t.Context(), auth, ObservedMainProcessEvent{Kind: "arbitrary_event_name"}); !errorsIsInvalidObservation(err) {
		t.Fatalf("free-form event = %v", err)
	}
	if err := manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
		Kind: "tengu_cli_flags", SessionID: "session", Model: "claude-opus-5", Metadata: map[string]any{"access_token": "secret"},
	}); !errorsIsInvalidObservation(err) {
		t.Fatalf("credential-bearing fields = %v", err)
	}
}

func TestObservedExecutableCatalogEnforcesCapturedPayloadContracts(t *testing.T) {
	manager := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	valid := ObservedRendererEvent{Kind: "mcp_servers_listed", Properties: map[string]any{"server_count": 2}}
	if err := manager.RecordObservedRendererEvent(t.Context(), auth, valid); err != nil {
		t.Fatalf("valid captured renderer payload: %v", err)
	}
	for label, properties := range map[string]map[string]any{
		"missing":    {},
		"wrong_type": {"server_count": "two"},
		"unknown":    {"server_count": 2, "caller_invented": true},
		"owned":      {"server_count": 2, "account_uuid": "caller"},
	} {
		if err := manager.RecordObservedRendererEvent(t.Context(), auth, ObservedRendererEvent{Kind: "mcp_servers_listed", Properties: properties}); !errorsIsInvalidObservation(err) {
			t.Fatalf("%s payload = %v", label, err)
		}
	}
	if err := manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
		Kind: "tengu_cli_flags", SessionID: "session", Model: "claude-opus-5",
		Metadata: map[string]any{"flag_count": 2, "flags": "inputFormat,outputFormat"},
	}); err != nil {
		t.Fatalf("valid captured SDK payload: %v", err)
	}
	if err := manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
		Kind: "tengu_cli_flags", SessionID: "session", Model: "claude-opus-5",
		Metadata: map[string]any{"flag_count": 2, "flags": []any{"inputFormat", "outputFormat"}},
	}); !errorsIsInvalidObservation(err) {
		t.Fatalf("wrong SDK payload type = %v", err)
	}
}

func TestObservedEndpointContractsEnforceFieldsOwnershipAndRoutes(t *testing.T) {
	tests := []struct {
		kind       string
		field      string
		wrongValue any
		validRoute string
		badRoute   string
	}{
		{"chorus_ideas_suggestions_shown", "position", "zero", observedRendererRouteNew, observedRendererRouteShell},
		{"claudeai_cowork_model_update_banner_displayed", "", nil, observedRendererRouteShell, observedRendererRouteNew},
		{"claudeai_cowork_model_update_composer_warning_displayed", "update_available", "false", observedRendererRouteNew, observedRendererRouteShell},
		{"claudeai_desktop_sidebar_design_entry_shown", "tabs", []any{}, observedRendererRouteSession, observedRendererRouteShell},
		{"claudeai_desktop_sidebar_mode_pill_selected", "compact", "false", observedRendererRouteNew, ""},
		{"claudeai_epitaxy_sidebar_group_new_session_clicked", "session_count", "one", observedRendererRouteSession, observedRendererRouteShell},
		{"claudeai_model_selector_model_selected", "model", false, observedRendererRouteShell, observedRendererRouteNew},
		{"claudeai_model_selector_opened", "model_surface", 1, observedRendererRouteShell, observedRendererRouteNew},
		{"claudeai_yukon_gold_activation_checklist_v2_in_viewport", "route_surface", false, observedRendererRouteNew, observedRendererRouteShell},
		{"claudeai_yukon_gold_activation_checklist_v2_shown", "assigned_items", "item", observedRendererRouteNew, observedRendererRouteShell},
		{"claudeai_yukon_gold_enabled", "", nil, observedRendererRouteShell, observedRendererRouteNew},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			event, ok := claudeprofile.V140609ObservedExecutableEvent(test.kind)
			if !ok {
				t.Fatal("event is missing")
			}
			valid := validObservedTestFields(t, event)
			if err := validateObservedEventPayload(event, valid); err != nil {
				t.Fatalf("valid payload: %v", err)
			}
			if test.field != "" {
				missing := cloneObservedTestMap(t, valid)
				delete(missing, test.field)
				if err := validateObservedEventPayload(event, missing); !errorsIsInvalidObservation(err) {
					t.Fatalf("missing %s = %v", test.field, err)
				}
				wrong := cloneObservedTestMap(t, valid)
				wrong[test.field] = test.wrongValue
				if err := validateObservedEventPayload(event, wrong); !errorsIsInvalidObservation(err) {
					t.Fatalf("wrong %s = %v", test.field, err)
				}
			}
			owned := cloneObservedTestMap(t, valid)
			owned["path"] = rendererCopyPathNew
			if err := validateObservedEventPayload(event, owned); !errorsIsInvalidObservation(err) {
				t.Fatalf("caller-owned path = %v", err)
			}
			if _, err := observedRendererCopyPath(event, test.validRoute, "session"); err != nil {
				t.Fatalf("valid route %q: %v", test.validRoute, err)
			}
			if test.badRoute != "" {
				if _, err := observedRendererCopyPath(event, test.badRoute, "session"); !errorsIsInvalidObservation(err) {
					t.Fatalf("bad route %q = %v", test.badRoute, err)
				}
			}
			if _, err := observedRendererCopyPath(event, "caller-path", "session"); !errorsIsInvalidObservation(err) {
				t.Fatalf("free-form route = %v", err)
			}
		})
	}

	sessionEvent, _ := claudeprofile.V140609ObservedExecutableEvent("claudeai_epitaxy_sidebar_group_new_session_clicked")
	if _, err := observedRendererCopyPath(sessionEvent, observedRendererRouteSession, ""); !errorsIsInvalidObservation(err) {
		t.Fatalf("session route without session_id = %v", err)
	}
}

func TestObservedEndpointContractProjectionPreservesCapturedDualFireOrder(t *testing.T) {
	event, ok := claudeprofile.V140609ObservedExecutableEvent("chorus_ideas_suggestions_shown")
	if !ok {
		t.Fatal("chorus suggestions event is missing")
	}
	identity := rendererSessionIdentity{
		AccountUUID: testAccountA, OrganizationUUID: testOrgA, BillingType: "pro", AppVersion: "1.40609.0",
	}
	properties, errProperties := projectObservedRendererProperties(event, identity, map[string]any{
		"idea_id": "idea", "position": 0, "ideas_variant": "control", "list_variant": "list", "client_platform": "desktop_app",
	})
	if errProperties != nil {
		t.Fatal(errProperties)
	}
	wantSegment := `{"account_uuid":"` + testAccountA + `","organization_uuid":"` + testOrgA + `","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","version":1,"idea_id":"idea","position":0,"ideas_variant":"control","list_variant":"list","client_platform":"desktop_app"}`
	if string(properties) != wantSegment {
		t.Fatalf("Segment properties = %s", properties)
	}
	segmentPayload := []byte(`{"properties":` + string(properties) + `,"anonymousId":"anonymous"}`)
	desktopProperties, errDesktop := desktopRendererDualFirePropertiesAt(segmentPayload, rendererCopyPathNew)
	if errDesktop != nil {
		t.Fatal(errDesktop)
	}
	wantDesktop := strings.TrimSuffix(wantSegment, "}") + `,"_dual_fire":true,"anonymous_id":"anonymous","service_name":"claude_ai","path":"/new"}`
	if desktopProperties != wantDesktop {
		t.Fatalf("Desktop properties = %s", desktopProperties)
	}
}

func TestObservedPageContractsPreserveCapturedOrderAndIdentityBoundary(t *testing.T) {
	identity := rendererSessionIdentity{
		AccountUUID: testAccountA, OrganizationUUID: testOrgA, BillingType: "pro", AppVersion: "1.40609.0",
	}
	login, _ := claudeprofile.V140609ObservedExecutableEvent("login")
	loginPayload, errLogin := projectObservedRendererProperties(login, identity, map[string]any{
		"referrer": "https://claude.ai/", "search": "?source=desktop",
	})
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	wantLogin := `{"path":"/login","referrer":"https://claude.ai/","search":"?source=desktop","url":"https://claude.ai/login?source=desktop","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","name":"/login","canonical_path":"/login","canonical_url":"https://claude.ai/login","canonical_referrer":"https://claude.ai/"}`
	if string(loginPayload) != wantLogin || bytes.Contains(loginPayload, []byte(`"account_uuid"`)) || bytes.Contains(loginPayload, []byte(`"version"`)) {
		t.Fatalf("login page payload = %s", loginPayload)
	}

	upgrade, _ := claudeprofile.V140609ObservedExecutableEvent("upgrade")
	upgradePayload, errUpgrade := projectObservedRendererProperties(upgrade, identity, map[string]any{
		"referrer": "https://claude.ai/settings", "search": "",
	})
	if errUpgrade != nil {
		t.Fatal(errUpgrade)
	}
	wantUpgrade := `{"path":"/upgrade","referrer":"https://claude.ai/settings","search":"","url":"https://claude.ai/upgrade","account_uuid":"` + testAccountA + `","organization_uuid":"` + testOrgA + `","billing_type":"pro","surface":"claude-ai","deployment_mode":"1p","app_version":"1.40609.0","name":"/upgrade","canonical_path":"/upgrade","canonical_url":"https://claude.ai/upgrade"}`
	if string(upgradePayload) != wantUpgrade || bytes.Contains(upgradePayload, []byte(`"version"`)) {
		t.Fatalf("upgrade page payload = %s", upgradePayload)
	}
}

func TestObservedSpecializedContractsEnforceNestedFields(t *testing.T) {
	event, ok := claudeprofile.V140609ObservedExecutableEvent("resource")
	if !ok {
		t.Fatal("resource event is missing")
	}
	valid := validObservedTestFields(t, event)
	if err := validateObservedEventPayload(event, valid); err != nil {
		t.Fatalf("valid resource payload: %v", err)
	}
	allowedContext := cloneObservedTestMap(t, valid)
	allowedContext["context"].(map[string]any)["caller_observed_value"] = true
	if err := validateObservedEventPayload(event, allowedContext); err != nil {
		t.Fatalf("resource context additional property: %v", err)
	}

	tests := map[string]func(map[string]any){
		"missing_nested": func(payload map[string]any) {
			delete(payload["resource"].(map[string]any), "protocol")
		},
		"wrong_nested_type": func(payload map[string]any) {
			payload["display"].(map[string]any)["viewport"].(map[string]any)["width"] = "wide"
		},
		"enum_drift": func(payload map[string]any) {
			payload["connectivity"].(map[string]any)["status"] = "offline"
		},
		"constant_drift": func(payload map[string]any) {
			payload["resource"].(map[string]any)["protocol"] = "http/1.1"
		},
		"unknown_nested": func(payload map[string]any) {
			payload["view"].(map[string]any)["caller_invented"] = true
		},
		"nested_program_owned": func(payload map[string]any) {
			payload["_dd"].(map[string]any)["format_version"] = 2
		},
		"top_level_program_owned": func(payload map[string]any) {
			payload["date"] = float64(1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			payload := cloneObservedTestMap(t, valid)
			mutate(payload)
			if err := validateObservedEventPayload(event, payload); !errorsIsInvalidObservation(err) {
				t.Fatalf("payload error = %v", err)
			}
		})
	}

	longTaskEvent, ok := claudeprofile.V140609ObservedExecutableEvent("long_task")
	if !ok {
		t.Fatal("long_task event is missing")
	}
	longTask := validObservedTestFields(t, longTaskEvent)
	if err := validateObservedEventPayload(longTaskEvent, longTask); err != nil {
		t.Fatalf("valid long_task payload: %v", err)
	}
	missingLongTaskField := cloneObservedTestMap(t, longTask)
	delete(missingLongTaskField["long_task"].(map[string]any), "render_start")
	if err := validateObservedEventPayload(longTaskEvent, missingLongTaskField); !errorsIsInvalidObservation(err) {
		t.Fatalf("missing long_task field = %v", err)
	}
	wrongScriptItem := cloneObservedTestMap(t, longTask)
	wrongScriptItem["long_task"].(map[string]any)["scripts"] = []any{"not-an-object"}
	if err := validateObservedEventPayload(longTaskEvent, wrongScriptItem); !errorsIsInvalidObservation(err) {
		t.Fatalf("wrong long_task script item = %v", err)
	}
}

func TestObservedRUMTelemetryVariantsAreExclusive(t *testing.T) {
	event, ok := claudeprofile.V140609ObservedExecutableEvent("telemetry")
	if !ok {
		t.Fatal("telemetry event is missing")
	}
	resolution, ok, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil || !ok {
		t.Fatalf("telemetry contract = %+v, %v", resolution, errContract)
	}
	variants := make(map[string]map[string]claudeprofile.ObservedEventFieldContract)
	for _, variant := range resolution.Contract.Variants {
		variants[variant.Name] = variant.Fields
	}
	for _, name := range []string{"configuration", "usage", "usage-contextual"} {
		payload := validObservedTestObject(t, event, variants[name], true)
		if err := validateObservedEventPayload(event, payload); err != nil {
			t.Fatalf("valid %s payload: %v", name, err)
		}
	}
	usage := validObservedTestObject(t, event, variants["usage"], true)
	usage["action"] = map[string]any{"id": "action"}
	if err := validateObservedEventPayload(event, usage); !errorsIsInvalidObservation(err) {
		t.Fatalf("half-contextual usage payload = %v", err)
	}
	configuration := validObservedTestObject(t, event, variants["configuration"], true)
	configuration["telemetry"].(map[string]any)["type"] = "usage"
	if err := validateObservedEventPayload(event, configuration); !errorsIsInvalidObservation(err) {
		t.Fatalf("mixed configuration/usage payload = %v", err)
	}
}

func TestObservedRUMProjectionKeepsCapturedEnvelopeShapes(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.AuxiliaryTelemetry.DatadogRUM.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()

	resourceEvent, _ := claudeprofile.V140609ObservedExecutableEvent("resource")
	resource := validObservedTestFields(t, resourceEvent)
	resource["version"] = "1.40609.0"
	resource["ddtags"] = "sdk_version:7.6.0"
	if err := manager.RecordObservedPerformanceEvent(t.Context(), auth, ObservedPerformanceEvent{
		Kind: resourceEvent.Kind, SessionID: "resource-session", Data: resource,
	}); err != nil {
		t.Fatal(err)
	}

	telemetryEvent, _ := claudeprofile.V140609ObservedExecutableEvent("telemetry")
	telemetryResolution, _, errContract := claudeprofile.V140609ObservedEventPayloadContract(telemetryEvent.Kind)
	if errContract != nil {
		t.Fatal(errContract)
	}
	for _, variant := range telemetryResolution.Contract.Variants {
		payload := validObservedTestObject(t, telemetryEvent, variant.Fields, true)
		payload["ddtags"] = "sdk_version:7.6.0"
		if anonymousID, ok := payload["anonymous_id"]; ok && anonymousID == "captured" {
			payload["anonymous_id"] = "anonymous-observed"
		}
		if err := manager.RecordObservedPerformanceEvent(t.Context(), auth, ObservedPerformanceEvent{
			Kind: telemetryEvent.Kind, SessionID: "telemetry-" + variant.Name, Data: payload,
		}); err != nil {
			t.Fatalf("record %s telemetry: %v", variant.Name, err)
		}
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}

	var resourcePayload map[string]any
	telemetryPayloads := make(map[string]map[string]any)
	for _, request := range doer.Requests() {
		body := request.Body
		if request.Header.Get("Content-Encoding") == "deflate" {
			body = inflateTestBody(t, body)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
			var payload map[string]any
			if json.Unmarshal(line, &payload) != nil {
				continue
			}
			switch payload["type"] {
			case "resource":
				resourcePayload = payload
			case "telemetry":
				telemetry, _ := payload["telemetry"].(map[string]any)
				variant, _ := telemetry["type"].(string)
				if variant == "usage" {
					if _, contextual := payload["action"]; contextual {
						variant = "usage-contextual"
					}
				}
				telemetryPayloads[variant] = payload
			}
		}
	}
	if resourcePayload == nil || resourcePayload["service"] != "claude-ai" || resourcePayload["version"] != "1.40609.0" || resourcePayload["source"] != "browser" {
		t.Fatalf("resource envelope = %+v", resourcePayload)
	}
	if _, hasDevice := resourcePayload["device"]; hasDevice {
		t.Fatalf("resource envelope synthesized device: %+v", resourcePayload)
	}
	dd, _ := resourcePayload["_dd"].(map[string]any)
	if dd["format_version"] != float64(2) || dd["sdk_name"] != "rum" || dd["discarded"] != false || dd["drift"] != float64(1) {
		t.Fatalf("resource _dd = %+v", dd)
	}
	if _, ok := dd["configuration"].(map[string]any); !ok {
		t.Fatalf("resource _dd configuration = %+v", dd["configuration"])
	}

	configuration := telemetryPayloads["configuration"]
	usage := telemetryPayloads["usage"]
	contextual := telemetryPayloads["usage-contextual"]
	if configuration == nil || usage == nil || contextual == nil {
		t.Fatalf("telemetry variants = %v", telemetryPayloads)
	}
	for name, payload := range telemetryPayloads {
		if payload["service"] != "browser-rum-sdk" || payload["version"] != auxiliaryRUMSDKVersion || payload["source"] != "browser" {
			t.Fatalf("%s telemetry envelope = %+v", name, payload)
		}
		ddFields, _ := payload["_dd"].(map[string]any)
		if len(ddFields) != 1 || ddFields["format_version"] != float64(2) {
			t.Fatalf("%s telemetry _dd = %+v", name, ddFields)
		}
	}
	for _, key := range []string{"session", "application", "anonymous_id"} {
		if _, exists := configuration[key]; !exists {
			t.Fatalf("configuration telemetry omits %q: %+v", key, configuration)
		}
		if _, exists := usage[key]; exists {
			t.Fatalf("base usage telemetry includes %q: %+v", key, usage)
		}
		if _, exists := contextual[key]; !exists {
			t.Fatalf("contextual usage telemetry omits %q: %+v", key, contextual)
		}
	}
	for _, key := range []string{"action", "view"} {
		if _, exists := usage[key]; exists {
			t.Fatalf("base usage telemetry includes %q: %+v", key, usage)
		}
		if _, exists := contextual[key]; !exists {
			t.Fatalf("contextual usage telemetry omits %q: %+v", key, contextual)
		}
	}
}

func TestObservedPluginCollisionProjectsSkillNameAndSampleRate(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	metadata := map[string]any{
		"item_type": "skill", "item_name_hash": "hash", "sources": "user,project", "source_count": float64(2), "winner_source": "project",
	}
	if err := manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
		Kind: "tengu_plugin_name_collision", SessionID: "session", Model: "claude-opus-5", Metadata: metadata,
	}); !errorsIsInvalidObservation(err) {
		t.Fatalf("missing skill_name = %v", err)
	}
	if err := manager.RecordObservedSDKEvent(t.Context(), auth, ObservedSDKEvent{
		Kind: "tengu_plugin_name_collision", SessionID: "session", Model: "claude-opus-5", SkillName: "collision-skill", Metadata: metadata,
	}); err != nil {
		t.Fatal(err)
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	var collision *sdkEventWrapper
	for _, event := range sdkEventsFromRequests(t, doer.Requests()) {
		if event.EventData.EventName == "tengu_plugin_name_collision" {
			copy := event
			collision = &copy
			break
		}
	}
	if collision == nil || collision.EventData.SkillName == nil || *collision.EventData.SkillName != "collision-skill" {
		t.Fatalf("plugin collision event = %+v", collision)
	}
	var projected map[string]any
	if errDecode := json.Unmarshal([]byte(sdkMetadataJSON(t, *collision)), &projected); errDecode != nil {
		t.Fatal(errDecode)
	}
	if projected["sample_rate"] != float64(0.01) {
		t.Fatalf("plugin collision metadata = %+v", projected)
	}
	if _, leaked := projected["skill_name"]; leaked {
		t.Fatalf("skill_name leaked into additional_metadata: %+v", projected)
	}
}

func TestObservedCrashAttachmentUsesCapturedBinaryContract(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.AuxiliaryTelemetry.Sentry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	if manager.bundle.AuxiliaryTelemetry.Sentry.Batch.MaxBytes != 4<<20 {
		t.Fatalf("Sentry max bytes = %d", manager.bundle.AuxiliaryTelemetry.Sentry.Batch.MaxBytes)
	}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	for name, observation := range map[string]ObservedCrashAttachment{
		"filename": {Filename: "crash.dmp", Data: []byte("MDMP")},
		"magic":    {Filename: "01234567-89ab-cdef-0123-456789abcdef.dmp", Data: []byte("NOPE")},
		"size":     {Filename: "01234567-89ab-cdef-0123-456789abcdef.dmp", Data: append([]byte("MDMP"), make([]byte, 3<<20)...)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := manager.RecordObservedCrashAttachment(t.Context(), auth, observation); !errorsIsInvalidObservation(err) {
				t.Fatalf("crash observation = %v", err)
			}
		})
	}
	sample := make([]byte, 2_150_912)
	copy(sample, "MDMP")
	if err := manager.RecordObservedCrashAttachment(t.Context(), auth, ObservedCrashAttachment{
		Filename: "01234567-89ab-cdef-0123-456789abcdef.dmp", Data: sample, Metadata: map[string]any{"process_type": "renderer"},
	}); err != nil {
		t.Fatal(err)
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	attachmentFound := false
	for _, request := range doer.Requests() {
		if request.Header.Get("Content-Type") != "application/x-sentry-envelope" {
			continue
		}
		body := request.Body
		if request.Header.Get("Content-Encoding") == "gzip" {
			body = gunzipTestBody(t, body)
		}
		lines := bytes.Split(bytes.TrimSuffix(body, []byte{'\n'}), []byte{'\n'})
		for index := 1; index+1 < len(lines); index += 2 {
			var header map[string]any
			if errDecode := json.Unmarshal(lines[index], &header); errDecode != nil {
				t.Fatal(errDecode)
			}
			if header["type"] != "attachment" {
				continue
			}
			attachmentFound = true
			if len(header) != 4 || header["filename"] != "01234567-89ab-cdef-0123-456789abcdef.dmp" || header["attachment_type"] != "event.minidump" {
				t.Fatalf("attachment item header = %+v", header)
			}
			if len(lines[index+1]) != len(sample) || !bytes.HasPrefix(lines[index+1], []byte("MDMP")) {
				t.Fatalf("attachment payload bytes = %d", len(lines[index+1]))
			}
		}
	}
	if !attachmentFound {
		t.Fatal("Sentry attachment item was not delivered")
	}
}

func cloneObservedTestMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &result); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return result
}

func errorsIsInvalidObservation(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrInvalidObservation.Error())
}

func validObservedTestFields(t *testing.T, event claudeprofile.ObservedExecutableEvent) map[string]any {
	t.Helper()
	resolution, ok, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		t.Fatal(errContract)
	}
	if !ok {
		t.Fatalf("payload contract resolution is missing for %q", event.Kind)
	}
	if resolution.Classification == claudeprofile.ObservedPayloadContractEndpointOnly {
		return map[string]any{}
	}
	fields := resolution.Contract.Fields
	if len(resolution.Contract.Variants) > 0 {
		fields = resolution.Contract.Variants[0].Fields
	}
	return validObservedTestObject(t, event, fields, true)
}

func validObservedTestRoute(t *testing.T, event claudeprofile.ObservedExecutableEvent) string {
	t.Helper()
	resolution, ok, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		t.Fatal(errContract)
	}
	if !ok {
		t.Fatalf("payload contract resolution is missing for %q", event.Kind)
	}
	path, hasPath := resolution.Contract.Fields["path"]
	if !hasPath || len(path.Enum) == 0 {
		return ""
	}
	switch path.Enum[0] {
	case rendererCopyPathNew:
		return observedRendererRouteNew
	case rendererCopyPathShell:
		return observedRendererRouteShell
	case rendererCopyPathSession:
		return observedRendererRouteSession
	default:
		t.Fatalf("contract %q has unsupported controlled path %q", event.Kind, path.Enum[0])
		return ""
	}
}

func validObservedTestSkillName(t *testing.T, event claudeprofile.ObservedExecutableEvent) string {
	t.Helper()
	resolution, ok, errContract := claudeprofile.V140609ObservedEventPayloadContract(event.Kind)
	if errContract != nil {
		t.Fatal(errContract)
	}
	if !ok || resolution.Classification == claudeprofile.ObservedPayloadContractEndpointOnly {
		return ""
	}
	eventFields := validObservedTestObject(t, event, resolution.Contract.EventFields, false)
	value, _ := eventFields["skill_name"].(string)
	return value
}

func validObservedTestObject(t *testing.T, event claudeprofile.ObservedExecutableEvent, fields map[string]claudeprofile.ObservedEventFieldContract, topLevel bool) map[string]any {
	t.Helper()
	result := make(map[string]any)
	for name, field := range fields {
		if !field.Required || field.Owner == claudeprofile.ObservedFieldOwnerProgram || (topLevel && observedProgramOwnedField(event.Source, name)) {
			continue
		}
		if event.Kind == "desktop_notification_reachability" && name == "authorization" {
			result[name] = "granted"
			continue
		}
		result[name] = observedTestFieldValue(t, event, field)
	}
	return result
}

func observedTestFieldValue(t *testing.T, event claudeprofile.ObservedExecutableEvent, field claudeprofile.ObservedEventFieldContract) any {
	t.Helper()
	if len(field.Const) > 0 {
		var value any
		if errUnmarshal := json.Unmarshal(field.Const, &value); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		return value
	}
	if len(field.Enum) > 0 {
		return field.Enum[0]
	}
	for _, fieldType := range field.Types {
		switch fieldType {
		case "string", "json-string", "base64-json":
			return "captured"
		case "number", "integer":
			return float64(1)
		case "bool", "boolean":
			return true
		case "object":
			fields := field.Fields
			if len(field.Variants) > 0 {
				fields = field.Variants[0].Fields
			}
			return validObservedTestObject(t, event, fields, false)
		case "array":
			return []any{}
		}
	}
	for _, fieldType := range field.Types {
		if fieldType == "null" {
			return nil
		}
	}
	t.Fatalf("unsupported observed field types %v", field.Types)
	return nil
}

func observedWireEventNames(t *testing.T, role string, request recordedRequest) []string {
	t.Helper()
	switch role {
	case "desktop-event-logging", "sdk-event-logging":
		var batch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			t.Fatal(err)
		}
		result := make([]string, 0, len(batch.Events))
		for _, event := range batch.Events {
			result = append(result, event.EventData.EventName)
		}
		return result
	case segmentRole:
		var batch struct {
			Batch []map[string]any `json:"batch"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			t.Fatal(err)
		}
		result := make([]string, 0, len(batch.Batch))
		for _, event := range batch.Batch {
			if name, _ := event["event"].(string); name != "" {
				result = append(result, name)
			} else if name, _ := event["name"].(string); name != "" {
				result = append(result, name)
			}
		}
		return result
	case datadogLogsRole:
		var events []map[string]any
		if err := json.Unmarshal(request.Body, &events); err != nil {
			t.Fatal(err)
		}
		result := make([]string, 0, len(events))
		for _, event := range events {
			if name, _ := event["message"].(string); name != "" {
				result = append(result, name)
			}
		}
		return result
	case datadogRUMRole:
		body := request.Body
		if request.Header.Get("Content-Encoding") == "deflate" {
			body = inflateTestBody(t, body)
		}
		var result []string
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			if name, _ := event["type"].(string); name != "" {
				result = append(result, name)
			}
		}
		return result
	case sentryRole:
		body := request.Body
		if request.Header.Get("Content-Encoding") == "gzip" {
			body = gunzipTestBody(t, body)
		}
		lines := bytes.Split(bytes.TrimSuffix(body, []byte{'\n'}), []byte{'\n'})
		var result []string
		for index := 1; index+1 < len(lines); index += 2 {
			var header map[string]any
			if err := json.Unmarshal(lines[index], &header); err != nil {
				t.Fatal(err)
			}
			if name, _ := header["type"].(string); name != "" {
				result = append(result, name)
			}
		}
		return result
	default:
		return nil
	}
}
