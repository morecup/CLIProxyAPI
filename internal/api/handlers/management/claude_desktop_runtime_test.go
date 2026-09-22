package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeDesktopRuntimeManagementPromotionAndRollback(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	auth.Attributes = map[string]string{
		coreauth.AttributeAPIKey:   "sk-ant-oat-management-runtime-test",
		coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	accountExecutor := newManagementClaudeAccountExecutor(t)
	t.Cleanup(accountExecutor.Close)
	manager.RegisterExecutor(accountExecutor)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.GET("/runtimes", handler.GetClaudeDesktopRuntimes)
	router.GET("/runtimes/:auth_id", handler.GetClaudeDesktopRuntime)
	router.POST("/runtimes/:auth_id/promote", handler.PromoteClaudeDesktopRuntime)
	router.POST("/runtimes/:auth_id/rollback", handler.RollbackClaudeDesktopRuntime)

	list := performClaudeDesktopRuntimeRequest(t, router, http.MethodGet, "/runtimes")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), auth.ID) || strings.Contains(list.Body.String(), "sk-ant-oat") {
		t.Fatalf("runtime list status=%d body=%s", list.Code, list.Body.String())
	}

	stored, exists := manager.GetByID(auth.ID)
	if !exists {
		t.Fatal("registered auth is missing")
	}
	stored.ProxyURL = "http://127.0.0.1:18091"
	if _, errUpdate := manager.Update(context.Background(), stored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	beforePromotion := performClaudeDesktopRuntimeRequest(t, router, http.MethodGet, "/runtimes/"+auth.ID)
	var beforePayload struct {
		Runtime runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
	}
	if errDecode := json.Unmarshal(beforePromotion.Body.Bytes(), &beforePayload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !beforePayload.Runtime.CanPromote || beforePayload.Runtime.ApprovedRevision == beforePayload.Runtime.ObservedRevision {
		t.Fatalf("pre-promotion runtime = %+v", beforePayload.Runtime)
	}
	oldRevision := beforePayload.Runtime.ApprovedRevision

	promote := performClaudeDesktopRuntimeRequest(t, router, http.MethodPost, "/runtimes/"+auth.ID+"/promote")
	if promote.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s", promote.Code, promote.Body.String())
	}
	var promotePayload struct {
		Runtime runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
	}
	if errDecode := json.Unmarshal(promote.Body.Bytes(), &promotePayload); errDecode != nil {
		t.Fatal(errDecode)
	}
	newRevision := promotePayload.Runtime.ApprovedRevision
	if newRevision == oldRevision || promotePayload.Runtime.PreviousRevision != oldRevision || promotePayload.Runtime.CanPromote {
		t.Fatalf("promoted runtime = %+v", promotePayload.Runtime)
	}

	stored, _ = manager.GetByID(auth.ID)
	stored.ProxyURL = ""
	if _, errUpdate := manager.Update(context.Background(), stored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	beforeRollback := performClaudeDesktopRuntimeRequest(t, router, http.MethodGet, "/runtimes/"+auth.ID)
	var rollbackReady struct {
		Runtime runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
	}
	if errDecode := json.Unmarshal(beforeRollback.Body.Bytes(), &rollbackReady); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !rollbackReady.Runtime.CanRollback || rollbackReady.Runtime.ObservedRevision != oldRevision {
		t.Fatalf("pre-rollback runtime = %+v", rollbackReady.Runtime)
	}

	rollback := performClaudeDesktopRuntimeRequest(t, router, http.MethodPost, "/runtimes/"+auth.ID+"/rollback")
	if rollback.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rollback.Code, rollback.Body.String())
	}
	var rollbackPayload struct {
		Runtime runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
	}
	if errDecode := json.Unmarshal(rollback.Body.Bytes(), &rollbackPayload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if rollbackPayload.Runtime.ApprovedRevision != oldRevision || rollbackPayload.Runtime.PreviousRevision != newRevision || rollbackPayload.Runtime.CanRollback {
		t.Fatalf("rolled-back runtime = %+v", rollbackPayload.Runtime)
	}
}

func TestClaudeDesktopRuntimeManagementRejectsUnknownAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	accountExecutor := newManagementClaudeAccountExecutor(t)
	t.Cleanup(accountExecutor.Close)
	manager.RegisterExecutor(accountExecutor)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.POST("/runtimes/:auth_id/promote", handler.PromoteClaudeDesktopRuntime)

	response := performClaudeDesktopRuntimeRequest(t, router, http.MethodPost, "/runtimes/missing/promote")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown auth status=%d body=%s", response.Code, response.Body.String())
	}
}

type managementBackgroundDoerFunc func(*http.Request) (*http.Response, error)

func (f managementBackgroundDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newManagementClaudeAccountExecutor(t *testing.T) *runtimeexecutor.ClaudeAccountExecutor {
	t.Helper()
	doer := managementBackgroundDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	return runtimeexecutor.NewClaudeAccountExecutorWithOptions(
		&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}},
		runtimeexecutor.ClaudeAccountExecutorOptions{
			StartupDoerFactory: func(context.Context, string, *coreauth.Auth) (claudestartup.HTTPDoer, error) {
				return doer, nil
			},
			TelemetryEndpointDoerFactory: func(string, string, *coreauth.Auth) claudetelemetry.HTTPDoer {
				return doer
			},
		},
	)
}

func performClaudeDesktopRuntimeRequest(t *testing.T, router http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
