package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type sessionControllerFixture struct {
	*runtimeexecutor.ClaudeAccountExecutor
	calls      []cliproxyexecutor.ClaudeDesktopSessionStop
	authID     string
	err        error
	starts     []cliproxyexecutor.ClaudeDesktopRemoteStart
	resumes    []cliproxyexecutor.ClaudeDesktopSessionResume
	heartbeats int
}

func (f *sessionControllerFixture) ResumeDesktopSession(_ context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionResume) (cliproxyexecutor.ClaudeDesktopSession, error) {
	f.authID = authID
	f.resumes = append(f.resumes, operation)
	return cliproxyexecutor.ClaudeDesktopSession{ID: operation.SessionID, Generation: operation.ExpectedGeneration}, f.err
}

func TestDesktopResumeHandlerUsesOnlyDurableGeneration(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	manager := coreauth.NewManager(nil, nil, nil)
	fixture := &sessionControllerFixture{ClaudeAccountExecutor: newManagementClaudeAccountExecutor(t)}
	t.Cleanup(fixture.Close)
	manager.RegisterExecutor(fixture)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.POST("/runtimes/:auth_id/sessions/:session_id/resume", handler.ResumeClaudeDesktopSession)
	request := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/runtimes/"+auth.ID+"/sessions/local_saved/resume", strings.NewReader(body)))
		return response
	}
	const generation = "12345678-1234-4234-8234-123456789abc"
	for _, body := range []string{`null`, `{}`, `{"expected_generation":null}`, `{"expected_generation":1}`, `{"expected_generation":"query"}`, `{"expected_generation":"00000000-0000-0000-0000-000000000000"}`, `{"expected_generation":"` + generation + `","system":"graft"}`, `{"expected_generation":"` + generation + `","messages":[]}`, `{"expected_generation":"` + generation + `"}{}`} {
		if response := request(body); response.Code != 400 {
			t.Fatal("invalid resume reached runtime", response.Code)
		}
	}
	if len(fixture.resumes) != 0 {
		t.Fatal("invalid request caused resume")
	}
	body := `{"expected_generation":"` + generation + `"}`
	if response := request(body); response.Code != 200 || len(fixture.resumes) != 1 || fixture.authID != auth.ID || fixture.resumes[0].SessionID != "local_saved" || fixture.resumes[0].ExpectedGeneration != generation {
		t.Fatal("resume lost exact owner", response.Code)
	}
	fixture.err = claudesessions.ErrStaleQuery
	if response := request(body); response.Code != 409 {
		t.Fatal("stale resume was not a conflict", response.Code)
	}
	fixture.err = claudesessions.ErrResumeUnverified
	if response := request(body); response.Code != 503 {
		t.Fatal("unverified history was not visible", response.Code)
	}
}

func (f *sessionControllerFixture) StartDesktopRemoteSession(_ context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopRemoteStart) (cliproxyexecutor.ClaudeDesktopSession, error) {
	f.authID = authID
	f.starts = append(f.starts, operation)
	return cliproxyexecutor.ClaudeDesktopSession{ID: "local_remote", QueryID: "query_remote", Running: true, RemoteState: "attached"}, f.err
}
func (f *sessionControllerFixture) AttachDesktopRemoteSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionStop) (cliproxyexecutor.ClaudeDesktopSession, error) {
	return f.StopDesktopSession(ctx, authID, operation)
}

func TestDesktopRemoteHandlersCreationAndExactAttachment(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	manager := coreauth.NewManager(nil, nil, nil)
	fixture := &sessionControllerFixture{ClaudeAccountExecutor: newManagementClaudeAccountExecutor(t)}
	t.Cleanup(fixture.Close)
	manager.RegisterExecutor(fixture)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.POST("/runtimes/:auth_id/sessions/remote", handler.StartClaudeDesktopRemoteSession)
	router.POST("/runtimes/:auth_id/sessions/:session_id/attach", handler.AttachClaudeDesktopRemoteSession)
	request := func(suffix, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/runtimes/"+auth.ID+"/sessions/"+suffix, strings.NewReader(body)))
		return recorder
	}
	for _, body := range []string{`null`, `{}`, `{"remote_session_id":"cse_x","folder":"C:/code","model":"m","session_id":"existing"}`, `{"remote_session_id":"cse_path/traversal","folder":"C:/code","model":"m"}`, `{"remote_session_id":"cse_x","folder":"C:/code","model":"m"}{}`} {
		if response := request("remote", body); response.Code != 400 {
			t.Fatal("invalid creation accepted", response.Code)
		}
	}
	if len(fixture.starts) != 0 {
		t.Fatal("invalid remote create reached runtime")
	}
	if response := request("remote", `{"remote_session_id":"session_x","folder":"C:/code","model":"m"}`); response.Code != 200 || len(fixture.starts) != 1 || fixture.authID != auth.ID {
		t.Fatal("creation failed", response.Code)
	}
	for _, body := range []string{`{}`, `{"expected_query_id":"q","remote_session_id":"cse_graft"}`} {
		if response := request("local_remote/attach", body); response.Code != 400 {
			t.Fatal("attachment accepted graft or missing generation", response.Code)
		}
	}
	if response := request("local_remote/attach", `{"expected_query_id":"query_remote"}`); response.Code != 200 || fixture.calls[0].SessionID != "local_remote" {
		t.Fatal("attachment lost owner", response.Code)
	}
	for _, err := range []error{claudesessions.ErrRemoteBound, claudesessions.ErrRemoteDetached, claudesessions.ErrRemoteMismatch, claudesessions.ErrStaleQuery} {
		fixture.err = err
		if response := request("local_remote/attach", `{"expected_query_id":"query_remote"}`); response.Code != 409 {
			t.Fatal("ownership conflict was hidden", response.Code)
		}
	}
}

func (f *sessionControllerFixture) ListDesktopSessions(string) ([]cliproxyexecutor.ClaudeDesktopSession, error) {
	return []cliproxyexecutor.ClaudeDesktopSession{{ID: "local_fixture", QueryID: "query-fixture", Running: true}}, nil
}
func (f *sessionControllerFixture) CheckDesktopSessionHeartbeats(_ context.Context, authID string) error {
	f.authID = authID
	f.heartbeats++
	return f.err
}
func (f *sessionControllerFixture) StopDesktopSession(_ context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionStop) (cliproxyexecutor.ClaudeDesktopSession, error) {
	f.authID = authID
	f.calls = append(f.calls, operation)
	return cliproxyexecutor.ClaudeDesktopSession{ID: operation.SessionID, QueryID: operation.ExpectedQueryID}, f.err
}

func TestDesktopSessionHandlersValidateExplicitOperation(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	manager := coreauth.NewManager(nil, nil, nil)
	fixture := &sessionControllerFixture{ClaudeAccountExecutor: newManagementClaudeAccountExecutor(t)}
	t.Cleanup(fixture.Close)
	manager.RegisterExecutor(fixture)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.GET("/runtimes/:auth_id/sessions", handler.GetClaudeDesktopSessions)
	router.POST("/runtimes/:auth_id/sessions/heartbeat-check", handler.CheckClaudeDesktopSessionHeartbeats)
	router.POST("/runtimes/:auth_id/sessions/:session_id/stop", handler.StopClaudeDesktopSession)
	path := "/runtimes/" + auth.ID + "/sessions/local_fixture/stop"
	request := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return recorder
	}
	for _, body := range []string{`{}`, `null`, `{"expected_query_id":""}`, `{"expected_query_id":"query-fixture","trigger":"app_quit"}`, `{"expected_query_id":"query-fixture"}{}`} {
		if response := request(body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid operation accepted: %s %d", body, response.Code)
		}
	}
	if len(fixture.calls) != 0 {
		t.Fatal("invalid operation reached provider")
	}
	response := request(`{"expected_query_id":"query-fixture"}`)
	if response.Code != http.StatusOK || len(fixture.calls) != 1 || fixture.authID != auth.ID || fixture.calls[0].SessionID != "local_fixture" || fixture.calls[0].ExpectedQueryID != "query-fixture" {
		t.Fatalf("typed operation lost: %d %+v", response.Code, fixture.calls)
	}
	fixture.err = claudesessions.ErrStaleQuery
	if response := request(`{"expected_query_id":"query-fixture"}`); response.Code != http.StatusConflict {
		t.Fatal("stale query did not return conflict", response.Code)
	}
	fixture.err = nil
	heartbeat := httptest.NewRecorder()
	router.ServeHTTP(heartbeat, httptest.NewRequest(http.MethodPost, "/runtimes/"+auth.ID+"/sessions/heartbeat-check", nil))
	if heartbeat.Code != http.StatusNoContent || fixture.heartbeats != 1 || fixture.authID != auth.ID {
		t.Fatalf("heartbeat tick lost server ownership: status=%d calls=%d auth=%q", heartbeat.Code, fixture.heartbeats, fixture.authID)
	}
}
