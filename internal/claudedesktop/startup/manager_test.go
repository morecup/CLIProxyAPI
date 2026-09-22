package startup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type startupDoerFunc func(*http.Request) (*http.Response, error)

func (f startupDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestManagerRunsOnceAndSeparatesOAuthFromSessionCookie(t *testing.T) {
	bundle, errLoad := claudeprofile.Load("")
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	auth := startupTestAuth(t, true)
	var mu sync.Mutex
	requests := make(map[string]*http.Request)
	manager := NewManager(Options{
		StatePath:            t.TempDir(),
		Bundle:               bundle,
		ApplicationSessionID: "app-session-test",
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				requests[role] = request.Clone(context.Background())
				mu.Unlock()
				if role == "startup-update-get" {
					response := startupResponse(http.StatusOK, "")
					response.Body = io.NopCloser(strings.NewReader(noUpdateJSON))
					return response, nil
				}
				return startupResponse(http.StatusOK, ""), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(auth)
	manager.Activate(auth)
	waitForStartupStatus(t, manager, func(status Status) bool { return status.State == "ready" })

	status := manager.Status()
	if status.Attempted != len(bundle.Startup.Endpoints) || status.Completed != len(bundle.Startup.Endpoints) || status.Failed != 0 || status.Skipped != 0 {
		t.Fatalf("unexpected status: %#v", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != len(bundle.Startup.Endpoints) {
		t.Fatalf("request count = %d, want %d", len(requests), len(bundle.Startup.Endpoints))
	}
	for _, endpoint := range bundle.Startup.Endpoints {
		request := requests[endpoint.EndpointRole]
		if request == nil {
			t.Fatalf("request for %q is missing", endpoint.EndpointRole)
		}
		switch endpoint.AuthPolicy {
		case "oauth-bearer":
			if got := request.Header.Get("Authorization"); got != "Bearer test-oauth-token" {
				t.Fatalf("%s Authorization = %q", endpoint.EndpointRole, got)
			}
			if got := request.Header.Get("Cookie"); got != "" {
				t.Fatalf("%s unexpectedly received Cookie %q", endpoint.EndpointRole, got)
			}
		case "session-cookie":
			if got := request.Header.Get("Cookie"); got != "sessionKey=test-session-key" {
				t.Fatalf("%s Cookie = %q", endpoint.EndpointRole, got)
			}
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("%s unexpectedly received Authorization", endpoint.EndpointRole)
			}
		case "none":
			if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
				t.Fatalf("%s unexpectedly received credentials", endpoint.EndpointRole)
			}
		}
	}
	if got := requests["startup-bootstrap"].URL.RawQuery; got != "entrypoint=claude-desktop&model=claude-sonnet-5" {
		t.Fatalf("bootstrap query order = %q", got)
	}
	if got := requests["startup-code-sessions"].URL.RawQuery; got != "limit=100&statuses=active&statuses=paused" {
		t.Fatalf("code sessions query order = %q", got)
	}
}

func TestManagerMissingSessionKeyIsVisibleAndDoesNotBlockOAuthStartup(t *testing.T) {
	bundle, errLoad := claudeprofile.Load("")
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	var calls atomic.Int64
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				response := startupResponse(http.StatusOK, "")
				if role == "startup-update-get" {
					response.Body = io.NopCloser(strings.NewReader(noUpdateJSON))
				}
				return response, nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, false))
	waitForStartupStatus(t, manager, func(status Status) bool {
		return status.State == "missing-session-key" && status.Completed+status.Failed == status.Attempted
	})
	status := manager.Status()
	if status.Skipped == 0 || status.LastErrorCategory != "missing-session-key" || calls.Load() == 0 {
		t.Fatalf("missing-session-key status = %#v, calls = %d", status, calls.Load())
	}
}

func TestManagerFailureDegradesOnlyStartupHealth(t *testing.T) {
	bundle := startupTestBundle("bounded")
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		DoerFactory: func(_ context.Context, _ string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(*http.Request) (*http.Response, error) {
				return startupResponse(http.StatusBadGateway, ""), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	waitForStartupStatus(t, manager, func(status Status) bool { return status.State == "degraded" })
	status := manager.Status()
	if status.Failed != 1 || status.LastErrorCategory != "upstream-5xx" {
		t.Fatalf("failure status = %#v", status)
	}
}

func TestManagerCloseCancelsStartupStream(t *testing.T) {
	bundle := startupTestBundle("stream")
	started := make(chan struct{})
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		DoerFactory: func(_ context.Context, _ string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: &blockingStartupBody{
						ctx: request.Context(), started: started,
					},
				}, nil
			}), nil
		},
	})
	manager.Activate(startupTestAuth(t, true))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stream body was not read")
	}
	done := make(chan struct{})
	go func() {
		manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the startup stream")
	}
	if status := manager.Status(); status.State != "stopped" {
		t.Fatalf("state after Close = %q", status.State)
	}
}

func TestManagerRetriesCapturedSessionsWatch502WithoutInflatingStartupCounts(t *testing.T) {
	bundle := startupTestBundle("stream")
	bundle.Startup.Endpoints[0].EndpointRole = startupCodeSessionsWatchRole
	bundle.Startup.Endpoints[0].Endpoint = "https://claude.ai/v1/code/sessions/watch"
	bundle.Startup.Endpoints[0].AuthPolicy = "session-cookie"
	bundle.Startup.Endpoints[0].RequiredFacts = []string{"session_key", "organization_uuid"}
	bundle.Startup.HeaderProfiles["test"] = claudeprofile.StartupHeaderProfile{
		HeaderOrder: []string{"Cookie", "User-Agent"},
		Headers:     []claudeprofile.TelemetryHeader{{Name: "User-Agent", Value: "test-agent"}},
	}
	var calls atomic.Int64
	var mu sync.Mutex
	var observed []int
	var delays []int
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		DoerFactory: func(_ context.Context, _ string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("Cookie") != "sessionKey=test-session-key" {
					t.Fatalf("watch request lost session cookie: %q", request.Header.Get("Cookie"))
				}
				if calls.Add(1) <= 3 {
					return startupResponse(http.StatusBadGateway, ""), nil
				}
				return startupResponse(http.StatusOK, ""), nil
			}), nil
		},
		SessionsWatchRetryDelay: func(attempt int) time.Duration {
			mu.Lock()
			delays = append(delays, attempt)
			mu.Unlock()
			return 0
		},
		SessionsWatchRetryObserver: func(auth *cliproxyauth.Auth, status int) error {
			if auth == nil || auth.ID == "" {
				t.Fatal("retry observation lost its account owner")
			}
			mu.Lock()
			observed = append(observed, status)
			mu.Unlock()
			return nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	waitForStartupStatus(t, manager, func(status Status) bool { return status.State == "ready" })
	status := manager.Status()
	if calls.Load() != 4 || status.Attempted != 1 || status.Completed != 1 || status.Failed != 0 {
		t.Fatalf("watch retry status = %#v, requests = %d", status, calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 3 || observed[0] != http.StatusBadGateway || observed[1] != http.StatusBadGateway || observed[2] != http.StatusBadGateway {
		t.Fatalf("retry observations = %v", observed)
	}
	if len(delays) != 3 || delays[0] != 0 || delays[1] != 1 || delays[2] != 2 {
		t.Fatalf("retry delay attempts = %v", delays)
	}
}

func TestManagerDoesNotApplyV140609WatchRetryToUnknownDesktopVersion(t *testing.T) {
	bundle := startupTestBundle("stream")
	bundle.DesktopVersion = "2.2553.1"
	bundle.Startup.Endpoints[0].EndpointRole = startupCodeSessionsWatchRole
	var calls atomic.Int64
	var observed atomic.Int64
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle,
		DoerFactory: func(_ context.Context, _ string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return startupResponse(http.StatusBadGateway, ""), nil
			}), nil
		},
		SessionsWatchRetryDelay: func(int) time.Duration { return 0 },
		SessionsWatchRetryObserver: func(*cliproxyauth.Auth, int) error {
			observed.Add(1)
			return nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	waitForStartupStatus(t, manager, func(status Status) bool { return status.State == "degraded" })
	if calls.Load() != 1 || observed.Load() != 0 {
		t.Fatalf("unknown version inherited watch retry: requests=%d observations=%d", calls.Load(), observed.Load())
	}
}

func TestManagerStatusAndIdentityPersistenceAreSecretFree(t *testing.T) {
	statePath := t.TempDir()
	bundle := startupTestBundle("bounded")
	manager := NewManager(Options{
		StatePath: statePath, Bundle: bundle,
		DoerFactory: func(_ context.Context, _ string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(*http.Request) (*http.Response, error) {
				return startupResponse(http.StatusOK, "secret-etag-value"), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	auth := startupTestAuth(t, true)
	manager.Activate(auth)
	waitForStartupStatus(t, manager, func(status Status) bool { return status.State == "ready" })

	statusPayload, errMarshal := json.Marshal(manager.Status())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, secret := range []string{"test-oauth-token", "test-session-key", "api.anthropic.com", "10000000-0000-4000-8000-000000000001"} {
		if strings.Contains(string(statusPayload), secret) {
			t.Fatalf("status leaks %q: %s", secret, statusPayload)
		}
	}
	identity, errIdentity := readIdentity(manager.identityPath())
	if errIdentity != nil {
		t.Fatal(errIdentity)
	}
	persisted, errRead := os.ReadFile(manager.identityPath())
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, plaintext := range []string{identity.InstallationID, "secret-etag-value"} {
		if strings.Contains(string(persisted), plaintext) {
			t.Fatalf("encrypted identity state contains plaintext %q", plaintext)
		}
	}
	identity.ETags["startup-test"] = "replacement-etag"
	if errSave := saveIdentity(manager.identityPath(), identity); errSave != nil {
		t.Fatalf("replace existing identity: %v", errSave)
	}
}

type blockingStartupBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *blockingStartupBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*blockingStartupBody) Close() error { return nil }

func startupResponse(status int, etag string) *http.Response {
	header := make(http.Header)
	if etag != "" {
		header.Set("ETag", etag)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

func startupTestBundle(responseMode string) *claudeprofile.Bundle {
	return &claudeprofile.Bundle{
		DesktopVersion: "1.40609.0.0",
		CodeVersion:    "2.1.247",
		Startup: claudeprofile.StartupProfile{
			SchemaVersion: 1, RequestTimeoutMS: 1000, MaxResponseBytes: 1024,
			HeaderProfiles: map[string]claudeprofile.StartupHeaderProfile{
				"test": {
					HeaderOrder: []string{"Authorization", "User-Agent"},
					Headers:     []claudeprofile.TelemetryHeader{{Name: "User-Agent", Value: "test-agent"}},
				},
			},
			Endpoints: []claudeprofile.StartupEndpointProfile{{
				EndpointRole: "startup-test", Method: http.MethodGet, Endpoint: "https://api.anthropic.com/test",
				HeaderProfile: "test", AuthPolicy: "oauth-bearer", ResponseMode: responseMode,
				RequiredFacts: []string{"oauth_access_token"},
			}},
		},
	}
}

func startupTestAuth(t *testing.T, includeSessionKey bool) *cliproxyauth.Auth {
	t.Helper()
	accountUUID := "10000000-0000-4000-8000-000000000001"
	organizationUUID := "20000000-0000-4000-8000-000000000001"
	deviceID := "30000000-0000-4000-8000-000000000001"
	authID, errAuthID := claudedesktop.StableAuthID(accountUUID, organizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	device := claudedesktop.TrustedDevice{DeviceID: deviceID, DeviceToken: "trusted-device-token", DisplayName: "Desktop"}
	enrollment := claudedesktop.NewEnrollment(authID, claudedesktop.AccountIdentity{
		AccountUUID: accountUUID, OrganizationUUID: organizationUUID,
	}, device, time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC))
	enrollment, errTransition := claudedesktop.TransitionEnrollment(enrollment, claudedesktop.EnrollmentActive, "", time.Date(2026, time.September, 4, 12, 0, 1, 0, time.UTC))
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	metadata := map[string]any{
		claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
		claudedesktop.MetadataEnrollmentKey:         enrollment,
		claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
		"account_uuid":            accountUUID,
		"organization_uuid":       organizationUUID,
		"claude_device_ids":       []string{claudedesktop.RequestDeviceID(deviceID)},
		"subscription_created_at": int64(1_700_000_000),
		"subscription_type":       "pro",
	}
	if includeSessionKey {
		metadata[claudedesktop.MetadataSessionKeyKey] = "test-session-key"
	}
	return &cliproxyauth.Auth{
		ID: authID, Provider: claudedesktop.Provider, Status: cliproxyauth.StatusActive,
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey:   "test-oauth-token",
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: metadata,
	}
}

func waitForStartupStatus(t *testing.T, manager *Manager, ready func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if ready(status) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	status := manager.Status()
	t.Fatalf("startup status did not settle: %#v", status)
	return Status{}
}
