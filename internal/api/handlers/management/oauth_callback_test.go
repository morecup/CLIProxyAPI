package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPostOAuthCallbackCreatesMissingAuthDir(t *testing.T) {

	authDir := filepath.Join(t.TempDir(), "missing-auth")
	state := "test-antigravity-state"
	RegisterOAuthSession(state, "antigravity")
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)

	body := `{"provider":"antigravity","redirect_url":"http://localhost:59788/oauth-callback?state=test-antigravity-state&code=test-code"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, w.Code, w.Body.String())
	}

	callbackPath := filepath.Join(authDir, ".oauth-antigravity-"+state+".oauth")
	data, errRead := os.ReadFile(callbackPath)
	if errRead != nil {
		t.Fatalf("expected callback file to be written: %v", errRead)
	}

	var payload oauthCallbackFilePayload
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode callback payload: %v", errUnmarshal)
	}
	if payload.State != state || payload.Code != "test-code" || payload.Error != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}

func TestGetOAuthCallbackWritesPluginProviderCallback(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "missing-auth")
	state := "test-geminicli-state"
	if errRegister := RegisterPluginOAuthSession(state, "gemini-cli", nil); errRegister != nil {
		t.Fatalf("register plugin oauth session: %v", errRegister)
	}
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.GET("/v0/management/oauth-callback", h.GetOAuthCallback)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state="+state+"&code=test-code", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, w.Code, w.Body.String())
	}

	callbackPath := filepath.Join(authDir, ".oauth-gemini-cli-"+state+".oauth")
	data, errRead := os.ReadFile(callbackPath)
	if errRead != nil {
		t.Fatalf("expected callback file to be written: %v", errRead)
	}

	var payload oauthCallbackFilePayload
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode callback payload: %v", errUnmarshal)
	}
	if payload.State != state || payload.Code != "test-code" || payload.Error != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}

func TestGetOAuthCallbackDoesNotAliasPluginProvider(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "missing-auth")
	state := "test-openai-plugin-state"
	if errRegister := RegisterPluginOAuthSession(state, "openai", nil); errRegister != nil {
		t.Fatalf("register plugin oauth session: %v", errRegister)
	}
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.GET("/v0/management/oauth-callback", h.GetOAuthCallback)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state="+state+"&code=test-code", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, w.Code, w.Body.String())
	}

	callbackPath := filepath.Join(authDir, ".oauth-openai-"+state+".oauth")
	if _, errRead := os.ReadFile(callbackPath); errRead != nil {
		t.Fatalf("expected plugin callback provider to stay openai: %v", errRead)
	}
	if _, errRead := os.ReadFile(filepath.Join(authDir, ".oauth-codex-"+state+".oauth")); errRead == nil {
		t.Fatal("unexpected codex callback file for openai plugin provider")
	}
}

func TestWriteOAuthCallbackFileForPendingSessionCreatesMissingAuthDirForCallbackProviders(t *testing.T) {
	// Anthropic uses an emailed magic link and xAI uses device code; neither
	// writes callback files.
	providers := []string{"codex", "gemini", "antigravity"}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			authDir := filepath.Join(t.TempDir(), "missing-auth")
			state := provider + "-state"
			RegisterOAuthSession(state, provider)
			defer CompleteOAuthSession(state)

			path, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, provider, state, "code-"+provider, "")
			if errWrite != nil {
				t.Fatalf("expected callback file write to succeed: %v", errWrite)
			}

			data, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("expected callback file to be written: %v", errRead)
			}

			var payload oauthCallbackFilePayload
			if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
				t.Fatalf("failed to decode callback payload: %v", errUnmarshal)
			}
			if payload.State != state || payload.Code != "code-"+provider || payload.Error != "" {
				t.Fatalf("unexpected callback payload: %+v", payload)
			}
		})
	}
}

func TestWriteOAuthCallbackFileRejectsAnthropic(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("anthropic-state", "anthropic")

	_, errWrite := WriteOAuthCallbackFileForPendingSession(t.TempDir(), "anthropic", "anthropic-state", "legacy-code", "")
	if errWrite == nil || !strings.Contains(errWrite.Error(), "email magic link") {
		t.Fatalf("Anthropic callback write error = %v, want magic-link rejection", errWrite)
	}
}

func TestPostOAuthCallbackSubmitsAnthropicMagicLinkInMemory(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	authDir := filepath.Join(t.TempDir(), "auth")
	state := "anthropic-magic-state"
	store.Register(state, "anthropic")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)
	body := `{"provider":"anthropic","state":"anthropic-magic-state","magic_link":"claude://claude.ai/magic-link#example"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("magic-link callback status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	value, errWait := WaitOAuthSessionInput(context.Background(), state, "anthropic")
	if errWait != nil || value != "claude://claude.ai/magic-link#example" {
		t.Fatalf("WaitOAuthSessionInput() = %q, %v", value, errWait)
	}
	if _, errStat := os.Stat(authDir); !os.IsNotExist(errStat) {
		t.Fatalf("Anthropic magic link unexpectedly created a callback file directory: %v", errStat)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate magic-link callback status = %d, want %d", w.Code, http.StatusConflict)
	}
}
