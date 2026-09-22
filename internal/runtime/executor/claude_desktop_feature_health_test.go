package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeDesktopFeatureHealthFollowsHostAcrossReidentityAndRotation(t *testing.T) {
	m, auth := nativeATISTestManager(t, func(string) any { return "PIN" })
	defer m.Close()
	var mu sync.Mutex
	active := make(map[string]bool)
	registrations, retirements := 0, 0
	m.featureHostObserverFactory = func(_ *cliproxyauth.Auth, owner string) (func(bool) error, func(), error) {
		mu.Lock()
		registrations++
		mu.Unlock()
		return func(healthy bool) error {
				mu.Lock()
				defer mu.Unlock()
				active[owner] = !healthy
				return nil
			}, func() {
				mu.Lock()
				defer mu.Unlock()
				delete(active, owner)
				retirements++
			}, nil
	}
	host := m.featureHosts.Warm()
	owner := "sdk-query:" + host.ID()
	_, oldResponse, err := m.prepareSDKFeatureHost(auth, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldResponse([]byte(`invalid`)); err == nil {
		t.Fatal("invalid feature response accepted")
	}
	if err := host.Reidentify(uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	rotated := auth.Clone()
	rotated.Attributes[cliproxyauth.AttributeAPIKey] = "sk-ant-oat01-synthetic-health-rotated"
	_, freshResponse, err := m.prepareSDKFeatureHost(rotated, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := freshResponse([]byte(`{"features":{"flag":{"value":true}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := oldResponse([]byte(`invalid`)); err == nil {
		t.Fatal("old credential response accepted")
	}
	mu.Lock()
	if registrations != 1 || active[owner] || len(active) != 1 {
		t.Fatal("reidentity/rotation leaked a stale health owner", registrations, active)
	}
	mu.Unlock()
	m.Close()
	if err := freshResponse([]byte(`invalid`)); err == nil {
		t.Fatal("closed query accepted late evaluation")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(active) != 0 || retirements != 1 {
		t.Fatal("close did not drain exact health ownership", active, retirements)
	}
}

func TestClaudeAccountDefaultFeatureHealthRetiresWithoutErasingCacheFault(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t, uuid.NewString(), uuid.NewString(), uuid.NewString())
	prepareH73RuntimeAuth(auth)
	account := NewClaudeAccountExecutorWithOptions(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}}, ClaudeAccountExecutorOptions{
		StartupDoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
			return claudeAccountStartupDoerFunc(func(*http.Request) (*http.Response, error) {
				payload := `{}`
				if role == "startup-sdk-eval" {
					payload = `invalid`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			}), nil
		},
		TelemetryEndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
			return claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
				major, proto := 2, "HTTP/2.0"
				if role == "sdk-event-logging" {
					major, proto = 1, "HTTP/1.1"
				}
				return &http.Response{StatusCode: 204, Proto: proto, ProtoMajor: major, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
		},
	})
	t.Cleanup(account.Close)
	if err := account.Provision(auth); err != nil {
		t.Fatal(err)
	}
	e := accountRuntimeForAuth(t, account, auth.ID).executor
	if err := e.Activate(auth); err != nil {
		t.Fatal(err)
	}
	waitIssues := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			count := 0
			for _, status := range e.desktopTelemetry.Status().Accounts {
				if status.EndpointRole == "sdk-event-logging" && status.FactIssues != nil {
					count += status.FactIssues.Unresolved
				}
			}
			if count == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("default SDK health has %d unresolved faults, want %d", count, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// Startup and the normal early-main binding own the health; this test does
	// not inject a telemetry observer or pre-create either query's feature state.
	waitIssues(1)
	firstCaller := uuid.NewString()
	first := e.resolveClaudeDesktopSession(t.Context(), auth, firstCaller, claudeprofile.RoleMain)
	second := e.resolveClaudeDesktopSession(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain)
	if first == second {
		t.Fatal("independent roots shared a native query")
	}
	waitIssues(2)
	if err := e.desktopTelemetry.ObserveSDKFeatureState(auth, "feature-cache:synthetic-durable-failure", false); err != nil {
		t.Fatal(err)
	}
	waitIssues(3)
	firstHost := e.desktopATIS.featureHosts.LookupSession(first)
	secondHost := e.desktopATIS.featureHosts.LookupSession(second)
	if firstHost == nil || secondHost == nil || firstHost == secondHost {
		t.Fatal("default query owners are missing")
	}
	firstHost.Close()
	waitIssues(2)
	resumed := e.resolveClaudeDesktopSession(t.Context(), auth, firstCaller, claudeprofile.RoleMain)
	replacement := e.desktopATIS.featureHosts.LookupSession(resumed)
	if resumed != first || replacement == nil || replacement.ID() == firstHost.ID() || replacement.Context().Err() != nil {
		t.Fatal("later main reused a closed query instead of restarting its transcript")
	}
	waitIssues(3)
	secondHost.Close()
	waitIssues(2)
	replacement.Close()
	waitIssues(1)
	if e.desktopStartup.Status().State == "stopped" {
		t.Fatal("query retirement stopped the application")
	}
}
