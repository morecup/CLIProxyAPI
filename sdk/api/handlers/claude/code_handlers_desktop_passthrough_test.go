package claude

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type desktopCodeHTTPPassthroughCaptureExecutor struct {
	mu sync.Mutex

	httpCalls    int
	executeCalls int
	authID       string
	method       string
	targetURL    string
	headers      http.Header
	body         []byte
	response     []byte
}

func (*desktopCodeHTTPPassthroughCaptureExecutor) Identifier() string { return "claude" }

func (e *desktopCodeHTTPPassthroughCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls++
	e.mu.Unlock()
	return coreexecutor.Response{}, nil
}

func (e *desktopCodeHTTPPassthroughCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.executeCalls++
	e.mu.Unlock()
	return nil, nil
}

func (*desktopCodeHTTPPassthroughCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*desktopCodeHTTPPassthroughCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *desktopCodeHTTPPassthroughCaptureExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		return nil, errRead
	}
	_ = req.Body.Close()

	e.mu.Lock()
	e.httpCalls++
	if auth != nil {
		e.authID = auth.ID
	}
	e.method = req.Method
	e.targetURL = req.URL.String()
	e.headers = req.Header.Clone()
	e.body = bytes.Clone(body)
	response := bytes.Clone(e.response)
	e.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":       {"text/event-stream"},
			"X-Upstream-Trace":   {"desktop-code-direct"},
			"X-Litellm-Provider": {"must-not-reach-client"},
			"Content-Encoding":   {"identity"},
		},
		Body: io.NopCloser(bytes.NewReader(response)),
	}, nil
}

func (e *desktopCodeHTTPPassthroughCaptureExecutor) snapshot() (httpCalls, executeCalls int, authID, method, targetURL string, headers http.Header, body []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.httpCalls, e.executeCalls, e.authID, e.method, e.targetURL, e.headers.Clone(), bytes.Clone(e.body)
}

func TestClaudeMessagesDesktopCodePassthroughPreservesRawRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &desktopCodeHTTPPassthroughCaptureExecutor{
		response: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
	}
	manager.RegisterExecutor(executor)
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient("desktop-code-oauth", "claude", []*registry.ModelInfo{{ID: "claude-sonnet-4-5-20250929"}})
	t.Cleanup(func() { registryRef.UnregisterClient("desktop-code-oauth") })
	if _, errRegister := manager.Register(t.Context(), &coreauth.Auth{
		ID:       "desktop-code-oauth",
		Provider: "claude",
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
		},
		Metadata: map[string]any{"access_token": "test-access-token"},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}

	handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{AuthManager: manager, Cfg: &sdkconfig.SDKConfig{}})
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)

	rawRequest := []byte("{\n  \"model\": \"claude-sonnet-4-5-20250929\",\n  \"stream\": true,\n  \"system\": [{\"type\": \"text\", \"text\": \"synthetic test context\", \"cache_control\": {\"type\": \"ephemeral\"}}],\n  \"messages\": [{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"ping\", \"cache_control\": {\"type\": \"ephemeral\"}}]}],\n  \"tools\": [{\"name\": \"synthetic_tool\", \"input_schema\": {\"type\": \"object\", \"properties\": {}}}]\n}\n")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(rawRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.275")
	req.Header.Set("anthropic-client-platform", "desktop_app")
	req.Header.Set("anthropic-client-version", "2.2553.1")
	req.Header.Set("x-claude-code-request-class", "main")
	req.Header.Set("x-claude-code-session-id", "synthetic-session")
	req.Header.Set("x-cc-atis", "synthetic-atis")
	req.Header.Set("Authorization", "Bearer downstream-secret")
	req.Header.Set("x-api-key", "downstream-key")
	req.Header.Set("X-Caller-Only", "must-not-forward")

	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Header().Get("X-Upstream-Trace"); got != "desktop-code-direct" {
		t.Fatalf("X-Upstream-Trace = %q", got)
	}
	if got := response.Header().Get("X-Litellm-Provider"); got != "" {
		t.Fatalf("gateway header leaked to client: %q", got)
	}
	if got := response.Header().Get("Content-Encoding"); got != "identity" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	if !bytes.Equal(response.Body.Bytes(), executor.response) {
		t.Fatal("response body was not relayed unchanged")
	}

	httpCalls, executeCalls, authID, method, targetURL, headers, body := executor.snapshot()
	if httpCalls != 1 {
		t.Fatalf("HttpRequest calls = %d, want 1", httpCalls)
	}
	if executeCalls != 0 {
		t.Fatalf("generic Execute/ExecuteStream calls = %d, want 0", executeCalls)
	}
	if authID != "desktop-code-oauth" {
		t.Fatalf("selected auth = %q", authID)
	}
	if method != http.MethodPost {
		t.Fatalf("upstream method = %q", method)
	}
	if targetURL != claudeDesktopCodeMessagesURL {
		t.Fatalf("upstream URL = %q", targetURL)
	}
	if !bytes.Equal(body, rawRequest) {
		t.Fatal("Desktop Code request body was changed before HttpRequest")
	}
	if got := headers.Get("Anthropic-Client-Platform"); got != "desktop_app" {
		t.Fatalf("Anthropic-Client-Platform = %q", got)
	}
	if got := headers.Get("Anthropic-Client-Version"); got != "2.2553.1" {
		t.Fatalf("Anthropic-Client-Version = %q", got)
	}
	if got := headers.Get("X-Claude-Code-Request-Class"); got != "main" {
		t.Fatalf("X-Claude-Code-Request-Class = %q", got)
	}
	for _, blocked := range []string{"Authorization", "X-Api-Key", "X-Caller-Only"} {
		if got := headers.Get(blocked); got != "" {
			t.Fatalf("caller-only header %s leaked to upstream: %q", blocked, got)
		}
	}
}
