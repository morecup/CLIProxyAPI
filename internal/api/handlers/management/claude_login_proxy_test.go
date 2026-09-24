package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRequestAnthropicTokenUsesAndPersistsLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		globalProxy = "http://global-proxy.example.test:8080"
		loginProxy  = "socks5h://user:password@1.2.3.4:1080"
	)
	store := &memoryAuthStore{}
	handler := NewHandler(&config.Config{
		AuthDir:   t.TempDir(),
		SDKConfig: config.SDKConfig{ProxyURL: globalProxy},
	}, "", nil)
	handler.tokenStore = store
	type loginObservation struct {
		ProxyURL string
		State    string
	}
	observed := make(chan loginObservation, 1)
	handler.claudeLogin = func(_ context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
		state := ""
		if opts != nil {
			state = opts.Metadata[claudedesktop.InteractiveSessionMetadataKey]
		}
		observed <- loginObservation{ProxyURL: cfg.ProxyURL, State: state}
		return &coreauth.Auth{
			ID:       "claude-desktop-proxy.json",
			Provider: "claude",
			FileName: "claude-desktop-proxy.json",
			Metadata: map[string]any{"email": "person@example.test"},
		}, nil
	}

	router := gin.New()
	router.POST("/anthropic-auth-url", handler.RequestAnthropicToken)
	request := httptest.NewRequest(http.MethodPost, "/anthropic-auth-url?is_webui=true", strings.NewReader(`{"proxy_url":"`+loginProxy+`"}`))
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

	login := <-observed
	if login.ProxyURL != loginProxy {
		t.Fatalf("login proxy = %q, want %q", login.ProxyURL, loginProxy)
	}
	if login.State != started.State {
		t.Fatalf("login state = %q, want %q", login.State, started.State)
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

func TestRequestAnthropicTokenRejectsInvalidLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	called := make(chan struct{}, 1)
	handler.claudeLogin = func(context.Context, *config.Config, *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
		called <- struct{}{}
		return nil, nil
	}

	router := gin.New()
	router.POST("/anthropic-auth-url", handler.RequestAnthropicToken)
	request := httptest.NewRequest(http.MethodPost, "/anthropic-auth-url", strings.NewReader(`{"proxy_url":"1.2.3.4:1080"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	select {
	case <-called:
		t.Fatal("Claude login started with an invalid proxy")
	default:
	}
}
