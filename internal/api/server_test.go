package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/claude/models"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithOptions(t)
}

func newTestServerWithOptions(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath, opts...)
}

func TestHealthz(t *testing.T) {
	server := newTestServer(t)

	t.Run("GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Status != "ok" {
			t.Fatalf("unexpected response status: got %q want %q", resp.Status, "ok")
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("expected empty body for HEAD request, got %q", rr.Body.String())
		}
	})
}

func TestManagementResponseExposesPluginSupportHeaderForCORS(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if got := rr.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
		t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
	}

	exposedHeaders := make(map[string]struct{})
	for _, headerName := range strings.Split(rr.Header().Get("Access-Control-Expose-Headers"), ",") {
		headerName = strings.ToLower(strings.TrimSpace(headerName))
		if headerName != "" {
			exposedHeaders[headerName] = struct{}{}
		}
	}
	for _, headerName := range corsExposedResponseHeaders {
		if _, ok := exposedHeaders[strings.ToLower(headerName)]; !ok {
			t.Fatalf("Access-Control-Expose-Headers missing %s: %q", headerName, rr.Header().Get("Access-Control-Expose-Headers"))
		}
	}
}

func TestOAuthCallbackRouteSkipsManagementKeyMiddleware(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	state := "server-plugin-oauth-state"
	if errRegister := managementHandlers.RegisterPluginOAuthSession(state, "gemini-cli", nil); errRegister != nil {
		t.Fatalf("register plugin oauth session: %v", errRegister)
	}
	defer managementHandlers.CompleteOAuthSession(state)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state="+state+"&code=test-code", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	callbackPath := filepath.Join(server.cfg.AuthDir, ".oauth-gemini-cli-"+state+".oauth")
	if _, errRead := os.ReadFile(callbackPath); errRead != nil {
		t.Fatalf("expected callback file to be written without management key: %v", errRead)
	}
}

func TestNewServerWithPluginHostInjectsHandlerInterceptors(t *testing.T) {
	host := pluginhost.New()
	server := newTestServerWithOptions(t, WithPluginHost(host))

	if server.handlers == nil {
		t.Fatal("server handlers = nil")
	}
	got, ok := server.handlers.PluginHost.(*pluginhost.Host)
	if !ok || got != host {
		t.Fatalf("handler plugin host = %#v, want configured host", server.handlers.PluginHost)
	}
}

func TestNewServerWithoutPluginHostLeavesHandlerInterceptorsDisabled(t *testing.T) {
	server := newTestServer(t)

	if server.handlers == nil {
		t.Fatal("server handlers = nil")
	}
	if server.handlers.PluginHost != nil {
		t.Fatalf("handler plugin host = %#v, want nil", server.handlers.PluginHost)
	}
}

func TestManagementUsageRequiresManagementAuthAndPopsArray(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	t.Cleanup(func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	})

	server := newTestServer(t)

	redisqueue.Enqueue([]byte(`{"id":1}`))
	redisqueue.Enqueue([]byte(`{"id":2}`))

	missingKeyReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	missingKeyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(missingKeyRR, missingKeyReq)
	if missingKeyRR.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d body=%s", missingKeyRR.Code, http.StatusUnauthorized, missingKeyRR.Body.String())
	}

	legacyReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage?count=2", nil)
	legacyReq.Header.Set("Authorization", "Bearer test-management-key")
	legacyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(legacyRR, legacyReq)
	if legacyRR.Code != http.StatusNotFound {
		t.Fatalf("legacy usage status = %d, want %d body=%s", legacyRR.Code, http.StatusNotFound, legacyRR.Body.String())
	}

	authReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	authReq.Header.Set("Authorization", "Bearer test-management-key")
	authRR := httptest.NewRecorder()
	server.engine.ServeHTTP(authRR, authReq)
	if authRR.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d body=%s", authRR.Code, http.StatusOK, authRR.Body.String())
	}

	var payload []json.RawMessage
	if errUnmarshal := json.Unmarshal(authRR.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v body=%s", errUnmarshal, authRR.Body.String())
	}
	if len(payload) != 2 {
		t.Fatalf("response records = %d, want 2", len(payload))
	}
	for i, raw := range payload {
		var record struct {
			ID int `json:"id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
			t.Fatalf("unmarshal record %d: %v", i, errUnmarshal)
		}
		if record.ID != i+1 {
			t.Fatalf("record %d id = %d, want %d", i, record.ID, i+1)
		}
	}

	if remaining := redisqueue.PopOldest(1); len(remaining) != 0 {
		t.Fatalf("remaining queue = %q, want empty", remaining)
	}
}

func TestManagementPluginsRouteRegistered(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	enabled := true
	server.cfg.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{
		"sample": {Enabled: &enabled, Priority: 4},
	}
	if errWrite := os.WriteFile(server.configFilePath, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("failed to write config file: %v", errWrite)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/plugins", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var payload struct {
		PluginsEnabled bool  `json:"plugins_enabled"`
		Plugins        []any `json:"plugins"`
	}
	if errUnmarshal := json.Unmarshal(rr.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v body=%s", errUnmarshal, rr.Body.String())
	}
	if payload.Plugins == nil {
		t.Fatalf("plugins field = nil, want array; body=%s", rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/plugins/sample/config", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("config status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var configPayload struct {
		Enabled  bool `json:"enabled"`
		Priority int  `json:"priority"`
	}
	if errUnmarshal := json.Unmarshal(rr.Body.Bytes(), &configPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal config response: %v body=%s", errUnmarshal, rr.Body.String())
	}
	if !configPayload.Enabled || configPayload.Priority != 4 {
		t.Fatalf("plugin config = %#v, want enabled true priority 4", configPayload)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestHomeEnabledHidesManagementEndpointsAndControlPanel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	server.cfg.Home.Enabled = true

	t.Run("management endpoints return 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})

	t.Run("management control panel returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})
}

func TestExampleAPIKeySafeModeShowsWarningAndKeepsManagement(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if err := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>management app</html>"), 0o600); err != nil {
		t.Fatalf("failed to write management asset: %v", err)
	}

	server := newTestServerWithOptions(t, WithExampleAPIKeySafeMode())
	cfg := *server.cfg
	cfg.APIKeys = []string{"your-api-key-1"}
	server.UpdateClients(&cfg)

	t.Run("root warning page includes management link", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		body := rr.Body.String()
		for _, want := range []string{"Example API key detected", "Open Management", `href="/management.html?safe-mode=configure"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("warning page missing %q: %s", want, body)
			}
		}
	})

	t.Run("management html defaults to warning page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "Example API key detected") {
			t.Fatalf("management.html did not show warning page: %s", rr.Body.String())
		}
	})

	t.Run("management html head stops at warning page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/management.html", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("HEAD body length = %d, want 0", rr.Body.Len())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("management button query opens control panel", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/management.html?safe-mode=configure", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "management app") {
			t.Fatalf("management panel body missing: %s", rr.Body.String())
		}
	})

	t.Run("proxy endpoints are blocked", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
		}
		if got := rr.Header().Get("X-CPA-SAFE-MODE"); got != "example-api-key" {
			t.Fatalf("X-CPA-SAFE-MODE = %q, want example-api-key", got)
		}
		if !strings.Contains(rr.Body.String(), "unsafe_example_api_key") {
			t.Fatalf("body missing safe-mode error: %s", rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "management_url") {
			t.Fatalf("body should not include management_url field: %s", rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "/management.html?safe-mode=configure") {
			t.Fatalf("body missing management link in message: %s", rr.Body.String())
		}
		if got := rr.Header().Get(internallogging.CPATraceIDHeader); got != "" {
			t.Fatalf("trace ID = %q, want empty before auth selection", got)
		}
	})

	t.Run("management endpoints still work", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if got := rr.Header().Get(internallogging.CPATraceIDHeader); got != "" {
			t.Fatalf("management trace ID = %q, want empty", got)
		}
	})

	t.Run("safe mode clears after key update", func(t *testing.T) {
		nextCfg := cfg
		nextCfg.APIKeys = []string{"real-key"}
		server.UpdateClients(&nextCfg)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer real-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "unsafe_example_api_key") {
			t.Fatalf("proxy endpoint still blocked after key update: %s", rr.Body.String())
		}
	})
}

func TestModelsDispatchByAnthropicVersionHeader(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-anthropic-version-dispatch"
	modelRegistry.RegisterClient(clientID, "claude", []*registry.ModelInfo{
		{
			ID:                  "claude-sonnet-4-6",
			Object:              "model",
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
		{
			ID:      "gpt-4o",
			Object:  "model",
			OwnedBy: "openai",
			Type:    "openai",
		},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	server := newTestServer(t)

	// Anthropic API request (Anthropic-Version header, non-claude-cli User-Agent) -> Claude format.
	t.Run("anthropic version header routes to claude format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("User-Agent", "Zed/1.0")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Object  string           `json:"object"`
			HasMore *bool            `json:"has_more"`
			Data    []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Object == "list" {
			t.Fatalf("expected Claude format (no object=list), got OpenAI format: %s", rr.Body.String())
		}
		if resp.HasMore == nil {
			t.Fatalf("expected Claude envelope with has_more, got %s", rr.Body.String())
		}

		var claudeModel map[string]any
		var rewrittenModel map[string]any
		for _, m := range resp.Data {
			id, _ := m["id"].(string)
			switch id {
			case "claude-sonnet-4-6":
				claudeModel = m
			case "claude-fable-5-dd-o4-tpg":
				rewrittenModel = m
			case "gpt-4o", "claude-gpt-4o":
				t.Fatalf("expected non-claude model id to be rewritten as claude-fable-5-dd-<reversed>, got %q", id)
			}
		}
		if claudeModel == nil {
			t.Fatalf("expected claude-sonnet-4-6 in response, got %s", rr.Body.String())
		}
		if rewrittenModel == nil {
			t.Fatalf("expected claude-fable-5-dd-o4-tpg in response, got %s", rr.Body.String())
		}
		for _, field := range []string{"max_input_tokens", "max_tokens", "display_name"} {
			if _, ok := claudeModel[field]; !ok {
				t.Fatalf("expected Claude model to include %q, got %v", field, claudeModel)
			}
		}
	})

	// Plain request (no Anthropic-Version, non-claude-cli User-Agent) -> OpenAI format, unaffected.
	t.Run("plain request stays on openai format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("User-Agent", "Mozilla/5.0")

		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Object != "list" {
			t.Fatalf("expected OpenAI format (object=list), got %s", rr.Body.String())
		}
		foundRawGPT := false
		for _, m := range resp.Data {
			if _, ok := m["max_input_tokens"]; ok {
				t.Fatalf("did not expect max_input_tokens in OpenAI format, got %v", m)
			}
			if id, _ := m["id"].(string); id == "gpt-4o" {
				foundRawGPT = true
			}
			if id, _ := m["id"].(string); id == "claude-gpt-4o" || id == "claude-fable-5-dd-o4-tpg" {
				t.Fatalf("did not expect Anthropic id rewrite on OpenAI format models, got %v", m)
			}
		}
		if !foundRawGPT {
			t.Fatalf("expected raw gpt-4o in OpenAI format response, got %s", rr.Body.String())
		}
	})
}

func TestClaudeModelListCloakingConfigHotReload(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-claude-model-list-cloaking-hot-reload"
	const modelID = "gpt-model-list-hot-reload"
	modelRegistry.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID: modelID, Object: "model", OwnedBy: "test", Type: "openai",
	}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	server := newTestServer(t)
	assertModelID := func(want string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}

		var response struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
			t.Fatalf("decode response: %v", errUnmarshal)
		}
		for _, model := range response.Data {
			if model.ID == want {
				return
			}
		}
		t.Fatalf("model %q not found in response: %s", want, recorder.Body.String())
	}

	assertModelID(claudemodels.EnsureClaudeModelIDPrefix(modelID))

	updatedCfg := *server.cfg
	updatedCfg.SDKConfig = server.cfg.SDKConfig
	updatedCfg.ClaudeCode.DisableCloakingModelList = true
	server.UpdateClients(&updatedCfg)

	assertModelID(modelID)
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fallback to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}

func TestFormatHomeClaudeModelIncludesAnthropicSchemaFields(t *testing.T) {
	withMetadata := formatHomeClaudeModel(homeModelEntry{
		id:                  "claude-sonnet-4-6",
		created:             1771372800,
		ownedBy:             "anthropic",
		displayName:         "Claude 4.6 Sonnet",
		contextLength:       200000,
		maxCompletionTokens: 64000,
	})
	if got := withMetadata["created_at"]; got != "2026-02-18T00:00:00Z" {
		t.Fatalf("created_at = %v, want RFC3339 timestamp", got)
	}
	if got := withMetadata["type"]; got != "model" {
		t.Fatalf("type = %v, want model", got)
	}
	if got := withMetadata["display_name"]; got != "Claude 4.6 Sonnet" {
		t.Fatalf("display_name = %v, want Claude 4.6 Sonnet", got)
	}
	if got := withMetadata["max_input_tokens"]; got != 200000 {
		t.Fatalf("max_input_tokens = %v, want 200000", got)
	}
	if got := withMetadata["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %v, want 64000", got)
	}

	withDefaults := formatHomeClaudeModel(homeModelEntry{id: "claude-no-limits"})
	if got := withDefaults["display_name"]; got != "claude-no-limits" {
		t.Fatalf("display_name fallback = %v, want claude-no-limits", got)
	}

	customModel := formatHomeClaudeModel(homeModelEntry{id: "gpt-4o", displayName: "GPT-4o"})
	if got := customModel["id"]; got != "gpt-4o" {
		t.Fatalf("id = %v, want gpt-4o", got)
	}
	if got := customModel["display_name"]; got != "GPT-4o" {
		t.Fatalf("display_name = %v, want GPT-4o", got)
	}
	if got := withDefaults["max_input_tokens"]; got != registry.DefaultClaudeMaxInputTokens {
		t.Fatalf("max_input_tokens fallback = %v, want %d", got, registry.DefaultClaudeMaxInputTokens)
	}
	if got := withDefaults["max_tokens"]; got != registry.DefaultClaudeMaxOutputTokens {
		t.Fatalf("max_tokens fallback = %v, want %d", got, registry.DefaultClaudeMaxOutputTokens)
	}
	if _, ok := withDefaults["created_at"]; ok {
		t.Fatalf("created_at should be omitted when source created is missing, got %v", withDefaults)
	}
}

func TestDecodeHomeModelsKeepsTokenMetadata(t *testing.T) {
	entries, errDecode := decodeHomeModels([]byte(`{
		"claude": [
			{
				"id": "claude-sonnet-4-6",
				"created": 1771372800,
				"owned_by": "anthropic",
				"context_length": 200000,
				"max_completion_tokens": 64000
			}
		],
		"gemini": [
			{
				"name": "models/gemini-3-pro",
				"inputTokenLimit": 1048576,
				"outputTokenLimit": 65536
			}
		]
	}`))
	if errDecode != nil {
		t.Fatalf("decodeHomeModels returned error: %v", errDecode)
	}

	byID := make(map[string]homeModelEntry, len(entries))
	for _, entry := range entries {
		byID[entry.id] = entry
	}
	claudeEntry, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("expected claude-sonnet-4-6 entry, got %v", byID)
	}
	if claudeEntry.contextLength != 200000 || claudeEntry.maxCompletionTokens != 64000 {
		t.Fatalf("claude token metadata = %d/%d, want 200000/64000", claudeEntry.contextLength, claudeEntry.maxCompletionTokens)
	}
	geminiEntry, ok := byID["gemini-3-pro"]
	if !ok {
		t.Fatalf("expected gemini-3-pro entry, got %v", byID)
	}
	if geminiEntry.contextLength != 1048576 || geminiEntry.maxCompletionTokens != 65536 {
		t.Fatalf("gemini token metadata = %d/%d, want 1048576/65536", geminiEntry.contextLength, geminiEntry.maxCompletionTokens)
	}
}

func TestHomeModelsAuthStatus(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantStatus  int
		wantHandled bool
	}{
		{"no credentials", `{"error":{"type":"no_credentials","message":"Missing API key"}}`, http.StatusUnauthorized, true},
		{"invalid credential", `{"error":{"type":"invalid_credential","message":"Invalid API key"}}`, http.StatusUnauthorized, true},
		{"internal error maps to bad gateway", `{"error":{"type":"internal_error","message":"boom"}}`, http.StatusBadGateway, true},
		{"models payload not an error", `{"openai":[{"id":"gpt-5.5"}]}`, 0, false},
		{"empty payload not an error", `{}`, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, handled := homeModelsAuthStatus([]byte(tc.raw))
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v (status=%d)", handled, tc.wantHandled, status)
			}
			if handled && status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

func TestHomeModelsErrorMessage(t *testing.T) {
	if msg := homeModelsErrorMessage([]byte(`{"error":{"type":"invalid_credential","message":"Invalid API key"}}`)); msg != "Invalid API key" {
		t.Fatalf("message = %q, want %q", msg, "Invalid API key")
	}
	if msg := homeModelsErrorMessage([]byte(`{"openai":[]}`)); msg != "home models request failed" {
		t.Fatalf("default message = %q, want fallback", msg)
	}
}
