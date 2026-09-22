package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeDesktopRuntimeManagementRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	manager := server.handlers.AuthManager
	accountExecutor := newRouteTestClaudeAccountExecutor(t, server.handlers.AuthManager)
	t.Cleanup(accountExecutor.Close)
	manager.RegisterExecutor(accountExecutor)

	auth := newRouteTestClaudeDesktopAuth(t)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	unauthorized := performClaudeDesktopRuntimeRouteRequest(
		t,
		server,
		http.MethodGet,
		"/v0/management/claude-desktop/runtimes",
		false,
	)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	list := performClaudeDesktopRuntimeRouteRequest(
		t,
		server,
		http.MethodGet,
		"/v0/management/claude-desktop/runtimes",
		true,
	)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), auth.ID) {
		t.Fatalf("runtime list status=%d body=%s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "sk-ant-oat") || strings.Contains(list.Body.String(), "synthetic-route-device-token") {
		t.Fatalf("runtime list exposes credential material: %s", list.Body.String())
	}

	singlePath := "/v0/management/claude-desktop/runtimes/" + auth.ID
	single := performClaudeDesktopRuntimeRouteRequest(t, server, http.MethodGet, singlePath, true)
	if single.Code != http.StatusOK {
		t.Fatalf("runtime get status=%d body=%s", single.Code, single.Body.String())
	}

	stored, exists := manager.GetByID(auth.ID)
	if !exists || stored == nil {
		t.Fatal("registered Claude Desktop auth is missing")
	}
	stored.ProxyURL = "http://127.0.0.1:18091"
	if _, errUpdate := manager.Update(context.Background(), stored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	beforePromotion := waitForClaudeDesktopRuntimeRouteStatus(t, server, singlePath, func(status runtimeexecutor.ClaudeAccountRuntimeStatus) bool {
		return status.CanPromote && status.ApprovedRevision != status.ObservedRevision
	})
	oldRevision := beforePromotion.ApprovedRevision

	promote := performClaudeDesktopRuntimeRouteRequest(t, server, http.MethodPost, singlePath+"/promote", true)
	if promote.Code != http.StatusOK {
		t.Fatalf("runtime promote status=%d body=%s", promote.Code, promote.Body.String())
	}
	promoted := decodeClaudeDesktopRuntimeRouteStatus(t, promote)
	newRevision := promoted.ApprovedRevision
	if newRevision == oldRevision || promoted.PreviousRevision != oldRevision || promoted.CanPromote {
		t.Fatalf("promoted runtime = %+v", promoted)
	}

	stored, exists = manager.GetByID(auth.ID)
	if !exists || stored == nil {
		t.Fatal("promoted Claude Desktop auth is missing")
	}
	stored.ProxyURL = ""
	if _, errUpdate := manager.Update(context.Background(), stored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	waitForClaudeDesktopRuntimeRouteStatus(t, server, singlePath, func(status runtimeexecutor.ClaudeAccountRuntimeStatus) bool {
		return status.CanRollback && status.ObservedRevision == oldRevision
	})

	rollback := performClaudeDesktopRuntimeRouteRequest(t, server, http.MethodPost, singlePath+"/rollback", true)
	if rollback.Code != http.StatusOK {
		t.Fatalf("runtime rollback status=%d body=%s", rollback.Code, rollback.Body.String())
	}
	rolledBack := decodeClaudeDesktopRuntimeRouteStatus(t, rollback)
	if rolledBack.ApprovedRevision != oldRevision || rolledBack.PreviousRevision != newRevision || rolledBack.CanRollback {
		t.Fatalf("rolled-back runtime = %+v", rolledBack)
	}
}

func TestClaudeDesktopSessionRoutesRequireManagementAuthentication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	accountExecutor := newRouteTestClaudeAccountExecutor(t)
	t.Cleanup(accountExecutor.Close)
	server.handlers.AuthManager.RegisterExecutor(accountExecutor)
	auth := newRouteTestClaudeDesktopAuth(t)
	if _, err := server.handlers.AuthManager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	path := "/v0/management/claude-desktop/runtimes/" + auth.ID + "/sessions"
	for _, route := range []struct{ method, path string }{{http.MethodGet, path}, {http.MethodPost, path + "/local_unknown/stop"}, {http.MethodPost, path + "/local_unknown/resume"}} {
		response := performClaudeDesktopRuntimeRouteRequest(t, server, route.method, route.path, false)
		if response.Code != http.StatusUnauthorized {
			t.Fatal("session operation bypassed management authentication", response.Code)
		}
	}
	response := performClaudeDesktopRuntimeRouteRequest(t, server, http.MethodGet, path, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sessions":[]`) {
		t.Fatal("session list route", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, path+"/local_unknown/stop", strings.NewReader(`{"expected_query_id":"unknown"}`))
	request.Header.Set("Authorization", "Bearer test-management-key")
	result := httptest.NewRecorder()
	server.engine.ServeHTTP(result, request)
	if result.Code != http.StatusNotFound {
		t.Fatal("unknown record route", result.Code, result.Body.String())
	}
}

func TestClaudeDesktopTelemetryObservationRoutesAreAuthenticatedAndSourceSplit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	accountExecutor := newRouteTestClaudeAccountExecutor(t, server.handlers.AuthManager)
	t.Cleanup(accountExecutor.Close)
	server.handlers.AuthManager.RegisterExecutor(accountExecutor)
	auth := newRouteTestClaudeDesktopAuth(t)
	if _, err := server.handlers.AuthManager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	stored, ok := server.handlers.AuthManager.GetByID(auth.ID)
	if !ok {
		t.Fatal("registered telemetry route auth is missing")
	}
	if err := accountExecutor.Provision(stored); err != nil {
		t.Fatal(err)
	}
	if status := accountExecutor.AccountStatus(auth.ID); !status.RuntimeLoaded {
		t.Fatalf("telemetry route runtime was not loaded after provision: %+v", status)
	}
	base := "/v0/management/claude-desktop/runtimes/" + auth.ID + "/telemetry/"
	cases := []struct {
		path string
		body string
	}{
		{"renderer", `{"kind":"claudeai_model_selector_opened","properties":{"model_surface":"code"}}`},
		{"main-process", `{"kind":"device_registry_no_tpm_probe_code","metadata":{"no_tpm_probe_code":"no-code-token"}}`},
		{"sdk", `{"kind":"tengu_cli_flags","session_id":"session-route","model":"claude-opus-5","metadata":{"flag_count":1,"flags":"inputFormat"}}`},
		{"performance", `{"kind":"long_task","data":{"_dd":{"drift":0},"view":{"url":"https://claude.ai/epitaxy/session","referrer":"","id":"view-route","name":"/epitaxy/:redacted"},"connectivity":{"status":"connected","effective_type":"4g"},"tab":{"id":"tab-route"},"context":{},"display":{"viewport":{"width":1280,"height":720}},"version":"1.40609.0","long_task":{"id":"long-task-route","entry_type":"long-animation-frame","duration":51,"blocking_duration":1,"first_ui_event_timestamp":1,"render_start":1,"style_and_layout_start":1,"start_time":1,"scripts":[]},"ddtags":"sdk_version:7.6.0"}}`},
		{"crash", `{"filename":"01234567-89ab-cdef-0123-456789abcdef.dmp","data":"TURNUA==","metadata":{"process_type":"renderer"}}`},
	}
	for _, test := range cases {
		unauthorized := performClaudeDesktopTelemetryRouteRequest(t, server, base+test.path, test.body, false)
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("%s bypassed management authentication: %d", test.path, unauthorized.Code)
		}
		authorized := performClaudeDesktopTelemetryRouteRequest(t, server, base+test.path, test.body, true)
		if authorized.Code != http.StatusNoContent {
			t.Fatalf("%s status=%d body=%s", test.path, authorized.Code, authorized.Body.String())
		}
	}
	freeForm := performClaudeDesktopTelemetryRouteRequest(t, server, base+"main-process", `{"kind":"caller_chosen_event","metadata":{}}`, true)
	if freeForm.Code != http.StatusBadRequest {
		t.Fatalf("free-form kind status=%d body=%s", freeForm.Code, freeForm.Body.String())
	}
	callerEndpoint := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"claudeai_model_selector_opened","event_name":"caller_event","properties":{}}`, true)
	if callerEndpoint.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected event_name status=%d body=%s", callerEndpoint.Code, callerEndpoint.Body.String())
	}
	missingCapturedField := performClaudeDesktopTelemetryRouteRequest(t, server, base+"sdk", `{"kind":"tengu_cli_flags","session_id":"session-route","model":"claude-opus-5","metadata":{"flag_count":1}}`, true)
	if missingCapturedField.Code != http.StatusBadRequest {
		t.Fatalf("missing captured field status=%d body=%s", missingCapturedField.Code, missingCapturedField.Body.String())
	}
	wrongCapturedType := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"mcp_servers_listed","properties":{"server_count":"two"}}`, true)
	if wrongCapturedType.Code != http.StatusBadRequest {
		t.Fatalf("wrong captured type status=%d body=%s", wrongCapturedType.Code, wrongCapturedType.Body.String())
	}
	missingSpecializedField := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"claudeai_model_selector_opened","properties":{}}`, true)
	if missingSpecializedField.Code != http.StatusBadRequest {
		t.Fatalf("missing specialized field status=%d body=%s", missingSpecializedField.Code, missingSpecializedField.Body.String())
	}
	wrongSpecializedType := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"claudeai_model_selector_opened","properties":{"model_surface":1}}`, true)
	if wrongSpecializedType.Code != http.StatusBadRequest {
		t.Fatalf("wrong specialized type status=%d body=%s", wrongSpecializedType.Code, wrongSpecializedType.Body.String())
	}
	callerPath := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"claudeai_model_selector_opened","properties":{"model_surface":"code","path":"/caller"}}`, true)
	if callerPath.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected renderer path status=%d body=%s", callerPath.Code, callerPath.Body.String())
	}
	callerRoute := performClaudeDesktopTelemetryRouteRequest(t, server, base+"renderer", `{"kind":"claudeai_model_selector_opened","route":"/caller","properties":{"model_surface":"code"}}`, true)
	if callerRoute.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected renderer route status=%d body=%s", callerRoute.Code, callerRoute.Body.String())
	}
	pluginCollision := performClaudeDesktopTelemetryRouteRequest(t, server, base+"sdk", `{"kind":"tengu_plugin_name_collision","session_id":"session-route","model":"claude-opus-5","skill_name":"collision-skill","metadata":{"item_type":"skill","item_name_hash":"hash","sources":"user,project","source_count":2,"winner_source":"project"}}`, true)
	if pluginCollision.Code != http.StatusNoContent {
		t.Fatalf("plugin collision status=%d body=%s", pluginCollision.Code, pluginCollision.Body.String())
	}
	missingSkillName := performClaudeDesktopTelemetryRouteRequest(t, server, base+"sdk", `{"kind":"tengu_plugin_name_collision","session_id":"session-route","model":"claude-opus-5","metadata":{"item_type":"skill","item_name_hash":"hash","sources":"user,project","source_count":2}}`, true)
	if missingSkillName.Code != http.StatusBadRequest {
		t.Fatalf("missing skill_name status=%d body=%s", missingSkillName.Code, missingSkillName.Body.String())
	}
	callerContentType := performClaudeDesktopTelemetryRouteRequest(t, server, base+"crash", `{"filename":"01234567-89ab-cdef-0123-456789abcdef.dmp","content_type":"application/octet-stream","data":"TURNUA==","metadata":{}}`, true)
	if callerContentType.Code != http.StatusBadRequest {
		t.Fatalf("caller content_type status=%d body=%s", callerContentType.Code, callerContentType.Body.String())
	}

	crashSample := make([]byte, 2_150_912)
	copy(crashSample, "MDMP")
	largeBody, errLargeBody := json.Marshal(map[string]any{
		"filename": "01234567-89ab-cdef-0123-456789abcdef.dmp",
		"data":     crashSample, "metadata": map[string]any{"process_type": "renderer"},
	})
	if errLargeBody != nil {
		t.Fatal(errLargeBody)
	}
	largeCrash := performClaudeDesktopTelemetryRouteRequest(t, server, base+"crash", string(largeBody), true)
	if largeCrash.Code != http.StatusNoContent {
		t.Fatalf("captured-size crash status=%d body=%s", largeCrash.Code, largeCrash.Body.String())
	}
}

func newRouteTestClaudeAccountExecutor(t *testing.T, managers ...*coreauth.Manager) *runtimeexecutor.ClaudeAccountExecutor {
	t.Helper()
	var credentialManager *coreauth.Manager
	if len(managers) > 0 {
		credentialManager = managers[0]
	}
	doer := claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	return runtimeexecutor.NewClaudeAccountExecutorWithOptions(
		&proxyconfig.Config{ClaudeDesktop: proxyconfig.ClaudeDesktopConfig{StatePath: t.TempDir()}},
		runtimeexecutor.ClaudeAccountExecutorOptions{
			CredentialManager: credentialManager,
			StartupDoerFactory: func(context.Context, string, *coreauth.Auth) (claudestartup.HTTPDoer, error) {
				return doer, nil
			},
			TelemetryEndpointDoerFactory: func(string, string, *coreauth.Auth) claudetelemetry.HTTPDoer {
				return doer
			},
		},
	)
}

func newRouteTestClaudeDesktopAuth(t *testing.T) *coreauth.Auth {
	t.Helper()
	identity := claudedesktop.AccountIdentity{
		AccountUUID:      "a1730000-0000-4000-8000-000000000001",
		OrganizationUUID: "b1730000-0000-4000-8000-000000000001",
	}
	device := claudedesktop.TrustedDevice{
		DeviceID:    "c1730000-0000-4000-8000-000000000001",
		DeviceToken: "synthetic-route-device-token",
		DisplayName: "Desktop",
	}
	authID, errAuthID := claudedesktop.StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := claudedesktop.NewEnrollment(authID, identity, device, time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC))
	enrollment, errTransition := claudedesktop.TransitionEnrollment(
		enrollment,
		claudedesktop.EnrollmentActive,
		"",
		time.Date(2026, 9, 4, 15, 0, 1, 0, time.UTC),
	)
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	return &coreauth.Auth{
		ID:       authID,
		Provider: claudedesktop.Provider,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAPIKey:   "sk-ant-oat-route-management-test",
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
		},
		Metadata: map[string]any{
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			claudedesktop.MetadataSessionKeyKey:         "synthetic-route-session-key",
			"account_uuid":                              identity.AccountUUID,
			"organization_uuid":                         identity.OrganizationUUID,
			"subscription_created_at":                   int64(1_700_000_000),
			"subscription_type":                         "pro",
			"claude_device_ids":                         []string{claudedesktop.RequestDeviceID(device.DeviceID)},
			claudedesktop.MetadataTelemetryMaterialsKey: claudedesktop.TelemetryMaterials{
				SegmentWriteKey:         "segment0123456789abcdef01234567",
				DatadogLogsAPIKey:       "datadoglogs0123456789abcdef01234567",
				DatadogRUMClientToken:   "datadogrum0123456789abcdef012345678",
				DatadogRUMApplicationID: "77777777-7777-4777-8777-777777777777",
				SentryPublicKey:         "abcdef0123456789abcdef0123456789",
			},
		},
	}
}

func performClaudeDesktopTelemetryRouteRequest(t *testing.T, server *Server, path, body string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authorized {
		request.Header.Set("Authorization", "Bearer test-management-key")
	}
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	return response
}

func performClaudeDesktopRuntimeRouteRequest(t *testing.T, server *Server, method, path string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if authorized {
		request.Header.Set("Authorization", "Bearer test-management-key")
	}
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	return response
}

func waitForClaudeDesktopRuntimeRouteStatus(
	t *testing.T,
	server *Server,
	path string,
	ready func(runtimeexecutor.ClaudeAccountRuntimeStatus) bool,
) runtimeexecutor.ClaudeAccountRuntimeStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		response := performClaudeDesktopRuntimeRouteRequest(t, server, http.MethodGet, path, true)
		if response.Code != http.StatusOK {
			t.Fatalf("runtime status=%d body=%s", response.Code, response.Body.String())
		}
		status := decodeClaudeDesktopRuntimeRouteStatus(t, response)
		if ready(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime status did not reach expected state: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func decodeClaudeDesktopRuntimeRouteStatus(t *testing.T, response *httptest.ResponseRecorder) runtimeexecutor.ClaudeAccountRuntimeStatus {
	t.Helper()
	var payload struct {
		Runtime runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode runtime response: %v body=%s", errDecode, response.Body.String())
	}
	return payload.Runtime
}
