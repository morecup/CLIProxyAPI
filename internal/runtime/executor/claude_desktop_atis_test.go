package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const testClaudeDesktopATIS = "0123456789abcdef"

type claudeDesktopATISTestDoerFunc func(*http.Request) (*http.Response, error)

func (f claudeDesktopATISTestDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestClaudeDesktopATISBootstrapBindsAndPersistsSession(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"11000000-0000-4000-8000-000000000001",
		"22000000-0000-4000-8000-000000000001",
		"33000000-0000-4000-8000-000000000001",
	)
	statePath := t.TempDir()
	var requests atomic.Int32
	doer := claudeDesktopATISTestDoerFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/api/claude_cli/bootstrap" {
			t.Fatalf("bootstrap request = %s %s", request.Method, request.URL.String())
		}
		if request.URL.Query().Get("entrypoint") != "claude-desktop" || request.URL.Query().Get("model") != "claude-sonnet-5" {
			t.Fatalf("bootstrap query = %s", request.URL.RawQuery)
		}
		wantHeaders := map[string]string{
			"Accept":                    "application/json, text/plain, */*",
			"Content-Type":              "application/json",
			"User-Agent":                "claude-code/2.1.275",
			"Authorization":             "Bearer " + auth.Attributes[cliproxyauth.AttributeAPIKey],
			"anthropic-beta":            "oauth-2025-04-20",
			"anthropic-client-platform": "desktop_app",
			"anthropic-client-version":  "2.2553.1",
			"Accept-Encoding":           "gzip, compress, deflate, br",
			"Connection":                "close",
		}
		for name, want := range wantHeaders {
			if got := request.Header.Get(name); got != want {
				t.Fatalf("bootstrap header %s = %q, want %q", name, got, want)
			}
		}
		return claudeDesktopATISTestResponse(http.StatusOK, fmt.Sprintf(`{"client_data":{"experimentKey":"claude_code_canal_canopy_experiment","atis":"%s"},"oauth_account":{"account_uuid":"%s","organization_uuid":"%s"}}`, testClaudeDesktopATIS, auth.Metadata["account_uuid"], auth.Metadata["organization_uuid"])), nil
	})
	factory := func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) { return doer, nil }
	manager := newClaudeDesktopATISManager(statePath, "2.1.247", factory)
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	manager.client = claudeDesktopATISClientFromProfile(bundle)

	atis, errAssignment := manager.Assignment(context.Background(), auth, "session-one", "claude-sonnet-5", "claude-sonnet-5", claudeprofile.RoleMain)
	if errAssignment != nil || atis != testClaudeDesktopATIS {
		t.Fatalf("main assignment = %q, %v", atis, errAssignment)
	}
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleSecurityMonitor, claudeprofile.RoleCountTokens} {
		atis, errAssignment = manager.Assignment(context.Background(), auth, "session-one", "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", role)
		if errAssignment != nil || atis != testClaudeDesktopATIS {
			t.Fatalf("%s inherited assignment = %q, %v", role, atis, errAssignment)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("bootstrap requests = %d, want 1", got)
	}

	stateFile := manager.filePath()
	encoded, errRead := os.ReadFile(stateFile)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(encoded), testClaudeDesktopATIS) || strings.Contains(string(encoded), "session-one") {
		t.Fatal("ATIS runtime state persisted plaintext assignment or session identity")
	}

	restartedCalls := atomic.Int32{}
	restarted := newClaudeDesktopATISManager(statePath, "2.1.247", func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
		restartedCalls.Add(1)
		return nil, fmt.Errorf("unexpected bootstrap after persisted restore")
	})
	atis, errAssignment = restarted.Assignment(context.Background(), auth, "session-one", "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", claudeprofile.RoleTitle)
	if errAssignment != nil || atis != testClaudeDesktopATIS {
		t.Fatalf("restored assignment = %q, %v", atis, errAssignment)
	}
	if restartedCalls.Load() != 0 {
		t.Fatal("persisted session binding triggered a bootstrap request")
	}
}

func TestClaudeDesktopATISEmptyAssignmentIsCachedAndInherited(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"12000000-0000-4000-8000-000000000001",
		"23000000-0000-4000-8000-000000000001",
		"34000000-0000-4000-8000-000000000001",
	)
	var requests atomic.Int32
	doer := claudeDesktopATISTestDoerFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return claudeDesktopATISTestResponse(http.StatusOK, fmt.Sprintf(`{"client_data":{"cedar_basin":"2027-08-31"},"oauth_account":{"account_uuid":"%s","organization_uuid":"%s"}}`, auth.Metadata["account_uuid"], auth.Metadata["organization_uuid"])), nil
	})
	manager := newClaudeDesktopATISManager(t.TempDir(), "2.1.247", func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) { return doer, nil })

	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleCountTokens} {
		atis, errAssignment := manager.Assignment(context.Background(), auth, "opus-session", "claude-opus-5", "claude-opus-5", role)
		if errAssignment != nil || atis != "" {
			t.Fatalf("%s assignment = %q, %v", role, atis, errAssignment)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("bootstrap requests = %d, want 1", got)
	}
}

func TestClaudeDesktopATISBootstrapDeduplicatesConcurrentModelFetch(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"13000000-0000-4000-8000-000000000001",
		"24000000-0000-4000-8000-000000000001",
		"35000000-0000-4000-8000-000000000001",
	)
	var requests atomic.Int32
	release := make(chan struct{})
	doer := claudeDesktopATISTestDoerFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		<-release
		return claudeDesktopATISTestResponse(http.StatusOK, fmt.Sprintf(`{"client_data":{"experimentKey":"experiment","atis":"%s"},"oauth_account":{"account_uuid":"%s","organization_uuid":"%s"}}`, testClaudeDesktopATIS, auth.Metadata["account_uuid"], auth.Metadata["organization_uuid"])), nil
	})
	manager := newClaudeDesktopATISManager(t.TempDir(), "2.1.247", func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) { return doer, nil })

	const workers = 12
	var wait sync.WaitGroup
	wait.Add(workers)
	errorsCh := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer wait.Done()
			atis, errAssignment := manager.Assignment(context.Background(), auth, fmt.Sprintf("session-%d", index), "claude-sonnet-5", "claude-sonnet-5", claudeprofile.RoleMain)
			if errAssignment != nil || atis != testClaudeDesktopATIS {
				errorsCh <- fmt.Errorf("assignment = %q, %v", atis, errAssignment)
			}
		}(index)
	}
	deadline := time.Now().Add(2 * time.Second)
	for requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()
	close(errorsCh)
	for errAssignment := range errorsCh {
		t.Error(errAssignment)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("bootstrap requests = %d, want 1", got)
	}
}

func TestClaudeDesktopATISBootstrapFailureDegradesWithoutRejectingRequest(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"14000000-0000-4000-8000-000000000001",
		"25000000-0000-4000-8000-000000000001",
		"36000000-0000-4000-8000-000000000001",
	)
	executor := NewClaudeExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	executor.desktopATIS = newClaudeDesktopATISManager(t.TempDir(), "2.1.247", func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
		return claudeDesktopATISTestDoerFunc(func(*http.Request) (*http.Response, error) {
			return claudeDesktopATISTestResponse(http.StatusBadGateway, `{}`), nil
		}), nil
	})
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive","display":"updates"}}`)
	plan, errPlan := executor.planClaudeDesktopRequest(body, claudeprofile.RoleMain)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
	if errHeaders := executor.applyClaudeHeadersWithProfile(request, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], true, nil, body, plan, nil, "degraded-session"); errHeaders != nil {
		t.Fatalf("apply headers rejected request after bootstrap failure: %v", errHeaders)
	}
	if got := request.Header.Get("x-cc-atis"); got != "" {
		t.Fatalf("degraded request ATIS = %q, want omitted", got)
	}
}

func claudeDesktopATISTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
