package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestImportClaudeSessionSavesCredentialWithoutEchoingSessionKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sessionKey = "sk-ant-sid-user-provided-session-key"
	store := &memoryAuthStore{}
	handler := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	handler.tokenStore = store
	handler.claudeSessionKeyLogin = func(_ context.Context, _ *config.Config, got string) (*coreauth.Auth, error) {
		if got != sessionKey {
			t.Fatalf("sessionKey was changed before import")
		}
		return &coreauth.Auth{
			ID:       "claude-desktop-import.json",
			Provider: "claude",
			FileName: "claude-desktop-import.json",
			Metadata: map[string]any{"email": "person@example.test"},
		}, nil
	}
	router := gin.New()
	router.POST("/claude-desktop/session-key", handler.ImportClaudeSession)

	request := httptest.NewRequest(http.MethodPost, "/claude-desktop/session-key", strings.NewReader(`{"session_key":"`+sessionKey+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), sessionKey) {
		t.Fatal("response echoed the Claude sessionKey")
	}
	var started struct {
		State string `json:"state"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &started); errDecode != nil || started.State == "" {
		t.Fatalf("invalid start response: %s", response.Body.String())
	}
	waitForOAuthSessionStatus(t, started.State, true)
	items, errList := store.List(context.Background())
	if errList != nil || len(items) != 1 || items[0].ID != "claude-desktop-import.json" {
		t.Fatalf("stored items = %#v, error=%v", items, errList)
	}
}

func TestImportClaudeSessionUsesAndPersistsLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		sessionKey  = "sk-ant-sid-user-provided-session-key"
		globalProxy = "http://global-proxy.example.test:8080"
		loginProxy  = "https://user:password@proxy.example.test:8443"
	)
	store := &memoryAuthStore{}
	handler := NewHandler(&config.Config{
		AuthDir:   t.TempDir(),
		SDKConfig: config.SDKConfig{ProxyURL: globalProxy},
	}, "", nil)
	handler.tokenStore = store
	observedProxy := make(chan string, 1)
	handler.claudeSessionKeyLogin = func(_ context.Context, cfg *config.Config, got string) (*coreauth.Auth, error) {
		if got != sessionKey {
			return nil, errors.New("sessionKey was changed before import")
		}
		observedProxy <- cfg.ProxyURL
		return &coreauth.Auth{
			ID:       "claude-desktop-session-proxy.json",
			Provider: "claude",
			FileName: "claude-desktop-session-proxy.json",
			Metadata: map[string]any{"email": "person@example.test"},
		}, nil
	}
	router := gin.New()
	router.POST("/claude-desktop/session-key", handler.ImportClaudeSession)

	body := `{"session_key":"` + sessionKey + `","proxy_url":"` + loginProxy + `"}`
	request := httptest.NewRequest(http.MethodPost, "/claude-desktop/session-key", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "password") {
		t.Fatal("response echoed proxy credentials")
	}
	var started struct {
		State string `json:"state"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &started); errDecode != nil || started.State == "" {
		t.Fatalf("invalid start response: %s", response.Body.String())
	}
	waitForOAuthSessionStatus(t, started.State, true)
	if got := <-observedProxy; got != loginProxy {
		t.Fatalf("login proxy = %q, want %q", got, loginProxy)
	}
	if handler.cfg.ProxyURL != globalProxy {
		t.Fatalf("global proxy changed to %q", handler.cfg.ProxyURL)
	}
	items, errList := store.List(context.Background())
	if errList != nil || len(items) != 1 {
		t.Fatalf("stored items = %#v, error=%v", items, errList)
	}
	if items[0].ProxyURL != loginProxy || items[0].Metadata["proxy_url"] != loginProxy {
		t.Fatalf("stored proxy = %q metadata=%#v", items[0].ProxyURL, items[0].Metadata["proxy_url"])
	}
}

func TestImportClaudeSessionRejectsInvalidAndDoesNotEchoFailedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const failedKey = "sk-ant-sid-expired-user-session"
	handler := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	handler.tokenStore = &memoryAuthStore{}
	handler.claudeSessionKeyLogin = func(context.Context, *config.Config, string) (*coreauth.Auth, error) {
		return nil, errors.New("expired session " + failedKey)
	}
	router := gin.New()
	router.POST("/claude-desktop/session-key", handler.ImportClaudeSession)

	for _, body := range []string{
		`{}`,
		`{"session_key":"value","extra":true}`,
		`{"session_key":"value","proxy_url":"1.2.3.4:1080"}`,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/claude-desktop/session-key", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, response.Code)
		}
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/claude-desktop/session-key", strings.NewReader(`{"session_key":"`+failedKey+`"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), failedKey) {
		t.Fatal("error response echoed the failed Claude sessionKey")
	}
	var started struct {
		State string `json:"state"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &started); errDecode != nil || started.State == "" {
		t.Fatalf("invalid start response: %s", response.Body.String())
	}
	status := waitForOAuthSessionStatus(t, started.State, false)
	if strings.Contains(status, failedKey) {
		t.Fatal("OAuth status echoed the failed Claude sessionKey")
	}
	if !strings.Contains(status, "[redacted]") {
		t.Fatalf("OAuth status did not redact the failed Claude sessionKey: %q", status)
	}
}

func waitForOAuthSessionStatus(t *testing.T, state string, wantComplete bool) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, status, _, _, completed, ok := GetOAuthSessionDetails(state)
		if ok && (completed || status != "") {
			if completed != wantComplete {
				t.Fatalf("OAuth session completed=%t status=%q, want completed=%t", completed, status, wantComplete)
			}
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("OAuth session %q did not finish", state)
	return ""
}
