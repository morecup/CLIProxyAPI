package startup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func featureStartupBundle(t *testing.T) *claudeprofile.Bundle {
	t.Helper()
	bundle, err := claudeprofile.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var endpoints []claudeprofile.StartupEndpointProfile
	for _, p := range bundle.Startup.Endpoints {
		if p.EndpointRole == "startup-sdk-eval" || p.EndpointRole == "startup-bootstrap" {
			endpoints = append(endpoints, p)
		}
	}
	bundle.Startup.Endpoints = endpoints
	if len(endpoints) != 2 {
		t.Fatal("test requires real SDK eval and bootstrap profiles")
	}
	return bundle
}

func featureResponse() *http.Response {
	response := startupResponse(200, "")
	response.Body = io.NopCloser(strings.NewReader(`{"features":{"flag":{"value":false}}}`))
	return response
}

func TestSDKFeatureAuthenticatedRouteAndFallback(t *testing.T) {
	for _, variant := range []string{"disabled", "success", "http-failure", "network-failure", "invalid-json", "both-fail"} {
		t.Run(variant, func(t *testing.T) {
			var paths, bodies, tokens []string
			var gateReads, observations, healthFailures atomic.Int64
			manager := NewManager(Options{
				StatePath: t.TempDir(), Bundle: featureStartupBundle(t),
				SDKFeatureAuthedEvaluation: func(*cliproxyauth.Auth) bool { gateReads.Add(1); return variant != "disabled" },
				SDKFeatureFailureObserver:  func(*cliproxyauth.Auth) error { healthFailures.Add(1); return nil },
				SDKFeaturesObserver: func(_ *cliproxyauth.Auth, payload []byte) error {
					if !json.Valid(payload) {
						return errors.New("synthetic invalid evaluation payload")
					}
					observations.Add(1)
					return nil
				},
				DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
					return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
						if role != "startup-sdk-eval" {
							return featureResponse(), nil
						}
						if _, has := request.Context().Deadline(); has {
							t.Error("alternate evaluation acquired a network deadline")
						}
						body, _ := io.ReadAll(request.Body)
						paths, bodies, tokens = append(paths, request.URL.Path), append(bodies, string(body)), append(tokens, request.Header.Get("Authorization"))
						if strings.HasPrefix(request.URL.Path, "/api/eval-authed/") {
							switch variant {
							case "http-failure", "both-fail":
								return startupResponse(502, ""), nil
							case "network-failure":
								return nil, errors.New("synthetic eval transport failure")
							case "invalid-json":
								response := featureResponse()
								response.Body = io.NopCloser(strings.NewReader(`{"truncated":`))
								return response, nil
							}
						}
						if variant == "both-fail" {
							return startupResponse(503, ""), nil
						}
						return featureResponse(), nil
					}), nil
				},
			})
			t.Cleanup(manager.Close)
			manager.Activate(startupTestAuth(t, true))
			waitForStartupStatus(t, manager, func(s Status) bool { return s.Completed+s.Failed == 2 })
			manager.Close()
			wantCalls := 1
			if variant == "http-failure" || variant == "network-failure" || variant == "both-fail" {
				wantCalls = 2
			}
			if len(paths) != wantCalls || gateReads.Load() != 1 {
				t.Fatalf("route count=%d gate reads=%d", len(paths), gateReads.Load())
			}
			first := "/api/eval-authed/"
			if variant == "disabled" {
				first = "/api/eval/"
			}
			if !strings.HasPrefix(paths[0], first) {
				t.Fatalf("wrong initial route %q", paths[0])
			}
			if wantCalls == 2 && (!strings.HasPrefix(paths[1], "/api/eval/") || bodies[0] != bodies[1] || tokens[0] != tokens[1]) {
				t.Fatal("fallback changed original payload, auth or route")
			}
			wantObserved, wantFetchOK := int64(1), uint64(1)
			if variant == "invalid-json" || variant == "both-fail" {
				wantObserved = 0
			}
			if variant == "both-fail" {
				wantFetchOK = 0
			}
			if observations.Load() != wantObserved || manager.sdkFetchOK != wantFetchOK || manager.Status().Attempted != 2 {
				t.Fatalf("fallback inflated startup/counter or parsed failed response: observed=%d fetch=%d status=%+v", observations.Load(), manager.sdkFetchOK, manager.Status())
			}
			wantHealthFailures := int64(0)
			if variant == "invalid-json" || variant == "both-fail" {
				wantHealthFailures = 1
			}
			if healthFailures.Load() != wantHealthFailures {
				t.Fatal("query health did not follow the final evaluation/fallback outcome", healthFailures.Load())
			}
		})
	}
}

func TestSDKFeatureAuthenticatedCloseDoesNotDispatchFallback(t *testing.T) {
	started := make(chan struct{})
	var evals atomic.Int64
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: featureStartupBundle(t),
		SDKFeatureAuthedEvaluation: func(*cliproxyauth.Auth) bool { return true },
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role != "startup-sdk-eval" {
					return featureResponse(), nil
				}
				evals.Add(1)
				close(started)
				<-request.Context().Done()
				return nil, request.Context().Err()
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated evaluation did not start")
	}
	manager.Close()
	if evals.Load() != 1 {
		t.Fatal("closed host dispatched fallback")
	}
}

func TestSDKFeatureRefreshTokenRotationRejectsOldCompletionWithoutStartupReplay(t *testing.T) {
	auth := startupTestAuth(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var applied, headers, sessions []string
	var evals, bootstrap, healthFailures atomic.Int64
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: featureStartupBundle(t), ApplicationSessionID: "renderer-session",
		SDKFeatureFailureObserver: func(*cliproxyauth.Auth) error { healthFailures.Add(1); return nil },
		SDKFeaturesObserverFactory: func(owned *cliproxyauth.Auth) (string, func([]byte) error, error) {
			token := oauthAccessToken(owned)
			return "owned-sdk-session", func([]byte) error { mu.Lock(); defer mu.Unlock(); applied = append(applied, token); return nil }, nil
		},
		SDKFeatureRefreshCadence: func(*cliproxyauth.Auth) (time.Duration, bool) { return 6 * time.Hour, false },
		DoerFactory: func(_ context.Context, role string, owned *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role != "startup-sdk-eval" {
					bootstrap.Add(1)
					return featureResponse(), nil
				}
				n := evals.Add(1)
				if _, ok := request.Context().Deadline(); ok {
					t.Error("SDK feature evaluation acquired a network deadline")
				}
				var p struct {
					Attributes map[string]any `json:"attributes"`
				}
				body, _ := io.ReadAll(request.Body)
				if json.Unmarshal(body, &p) != nil {
					t.Error("bad eval JSON")
				}
				mu.Lock()
				if p.Attributes["id"] != p.Attributes["deviceID"] || p.Attributes["id"] != claudedesktop.RequestDeviceID("30000000-0000-4000-8000-000000000001") {
					t.Error("SDK eval randomized identity or inherited Renderer installation ID")
				}
				headers = append(headers, request.Header.Get("Authorization"))
				sessions = append(sessions, p.Attributes["sessionId"].(string))
				mu.Unlock()
				if n == 1 {
					close(started)
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}
				if request.Header.Get("Authorization") != "Bearer "+oauthAccessToken(owned) {
					t.Error("request auth differed from factory snapshot")
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(auth)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial evaluation did not start")
	}
	rotated := auth.Clone()
	rotated.Attributes[cliproxyauth.AttributeAPIKey] = "rotated-token"
	manager.Activate(rotated)
	rotated.Attributes[cliproxyauth.AttributeAPIKey] = "mutated-after-activation"
	close(release)
	status := waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 1 })
	manager.Activate(auth.Clone())
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 2 })
	manager.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 2 || applied[0] != "rotated-token" || applied[1] != "test-oauth-token" {
		t.Fatalf("stale response applied or snapshot lost: %v", applied)
	}
	if len(headers) != 3 || headers[1] != "Bearer rotated-token" || sessions[0] != "owned-sdk-session" || sessions[1] != "owned-sdk-session" {
		t.Fatalf("dispatch snapshot incorrect: %v %v", headers, sessions)
	}
	if bootstrap.Load() != 1 || status.Attempted != 2 || status.FeatureRefreshAttempted != 1 {
		t.Fatalf("token rotation replayed startup or inflated startup counts: %v bootstrap=%d", status, bootstrap.Load())
	}
	if healthFailures.Load() != 0 {
		t.Fatal("old credential completion created a new live query health fault")
	}
}

func TestSDKFeaturePeriodicRefreshExtraAttemptAndClose(t *testing.T) {
	for _, flagged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unflagged", true: "flagged"}[flagged], func(t *testing.T) {
			var calls, cadence atomic.Int64
			manager := NewManager(Options{
				StatePath: t.TempDir(), Bundle: featureStartupBundle(t), RandomFloat: func() float64 { return 0 },
				SDKFeaturesObserver: func(*cliproxyauth.Auth, []byte) error { return nil },
				SDKFeatureRefreshCadence: func(*cliproxyauth.Auth) (time.Duration, bool) {
					if cadence.Add(1) == 1 {
						return time.Millisecond, flagged
					}
					return 6 * time.Hour, flagged
				},
				DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
					return startupDoerFunc(func(*http.Request) (*http.Response, error) {
						if role == "startup-sdk-eval" && calls.Add(1) == 2 {
							return startupResponse(502, ""), nil
						}
						return featureResponse(), nil
					}), nil
				},
			})
			t.Cleanup(manager.Close)
			manager.Activate(startupTestAuth(t, true))
			want := 1
			if flagged {
				want = 2
			}
			status := waitForStartupStatus(t, manager, func(s Status) bool {
				return s.FeatureRefreshAttempted == want && s.FeatureRefreshCompleted+s.FeatureRefreshFailed == want
			})
			manager.Close()
			if calls.Load() != int64(want+1) || status.FeatureRefreshFailed != 1 || status.FeatureRefreshCompleted != want-1 {
				t.Fatalf("wrong native extra retry: calls=%d status=%+v", calls.Load(), status)
			}
			if flagged && status.FeatureRefreshError != "" {
				t.Fatal("successful current refresh did not clear failure")
			}
			if !flagged && status.FeatureRefreshError == "" {
				t.Fatal("failed refresh hidden")
			}
		})
	}
}

func TestSDKFeatureCloseCancelsInflightRefresh(t *testing.T) {
	started := make(chan struct{})
	var evals atomic.Int64
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: featureStartupBundle(t),
		SDKFeaturesObserver:      func(*cliproxyauth.Auth, []byte) error { return nil },
		SDKFeatureRefreshCadence: func(*cliproxyauth.Auth) (time.Duration, bool) { return time.Millisecond, false },
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role == "startup-sdk-eval" && evals.Add(1) == 2 {
					close(started)
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	done := make(chan struct{})
	go func() { manager.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel SDK refresh")
	}
	manager.Activate(startupTestAuth(t, true))
	if evals.Load() != 2 {
		t.Fatal("closed startup restarted")
	}
}

func TestSDKFeatureMissingOptionalSubscriptionDoesNotSkipEvaluation(t *testing.T) {
	auth := startupTestAuth(t, true)
	delete(auth.Metadata, "subscription_created_at")
	delete(auth.Metadata, "subscription_type")
	var body []byte
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t), SDKFeaturesObserver: func(*cliproxyauth.Auth, []byte) error { return nil },
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role == "startup-sdk-eval" {
					body, _ = io.ReadAll(request.Body)
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(auth)
	waitForStartupStatus(t, manager, func(s Status) bool { return s.Completed == 2 })
	manager.Close()
	var payload struct {
		Attributes map[string]any `json:"attributes"`
	}
	if json.Unmarshal(body, &payload) != nil {
		t.Fatal("feature request missing")
	}
	for _, key := range []string{"subscriptionCreatedAt", "subscriptionType", "rateLimitTier", "email", "organizationRole"} {
		if _, exists := payload.Attributes[key]; exists {
			t.Fatalf("invented optional attribute %s", key)
		}
	}
	auth.Metadata["subscription_created_at"] = "2026-09-06T18:00:00"
	if got := sdkSubscriptionCreatedAt(auth); got != 1788717600000 {
		t.Fatalf("native subscription timestamp=%d", got)
	}
}

func TestSDKFeaturePersistenceFailureDoesNotInventNetworkRetry(t *testing.T) {
	var calls, cadence, observations atomic.Int64
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t), RandomFloat: func() float64 { return 0 },
		SDKFeaturesObserver: func(*cliproxyauth.Auth, []byte) error {
			if observations.Add(1) > 1 {
				return errors.New("synthetic persistence failure")
			}
			return nil
		},
		SDKFeatureRefreshCadence: func(*cliproxyauth.Auth) (time.Duration, bool) {
			if cadence.Add(1) == 1 {
				return time.Millisecond, true
			}
			return 6 * time.Hour, true
		},
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(*http.Request) (*http.Response, error) {
				if role == "startup-sdk-eval" {
					calls.Add(1)
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshFailed == 1 && cadence.Load() >= 2 })
	manager.Close()
	if calls.Load() != 2 || manager.Status().FeatureRefreshAttempted != 1 {
		t.Fatal("successful HTTP fetch was retried due to local persistence failure")
	}
}
