package startup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSDKFeatureHostsHaveIndependentDispatchWakeAndCancellation(t *testing.T) {
	var mu sync.Mutex
	calls, observations := map[string]int{}, map[string]int{}
	bootstrapCalls := 0
	holdA, aEntered, aCancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	prepare := func(session string) SDKFeaturesObserverFactory {
		return func(*cliproxyauth.Auth) (string, func([]byte) error, error) {
			return session, func([]byte) error { mu.Lock(); observations[session]++; mu.Unlock(); return nil }, nil
		}
	}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t),
		SDKFeaturesObserverFactory: prepare("warm"),
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role != "startup-sdk-eval" {
					mu.Lock()
					bootstrapCalls++
					mu.Unlock()
					return featureResponse(), nil
				}
				if _, ok := request.Context().Deadline(); ok {
					t.Error("host acquired a network deadline")
				}
				body, _ := io.ReadAll(request.Body)
				var payload struct {
					Attributes struct {
						Session string `json:"sessionId"`
					} `json:"attributes"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Error(err)
				}
				session := payload.Attributes.Session
				mu.Lock()
				calls[session]++
				count := calls[session]
				mu.Unlock()
				if session == "a" && count == 2 {
					close(aEntered)
					select {
					case <-request.Context().Done():
						close(aCancelled)
						return nil, request.Context().Err()
					case <-holdA:
					}
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	auth := startupTestAuth(t, true)
	manager.Activate(auth)
	waitForStartupStatus(t, manager, func(s Status) bool { return s.Completed == 2 })
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	longCadence := func(*cliproxyauth.Auth) (time.Duration, bool) { return time.Hour, false }
	a := &SDKFeatureHost{Context: ctxA, Prepare: prepare("a"), Cadence: longCadence}
	b := &SDKFeatureHost{Context: ctxB, Prepare: prepare("b"), Cadence: longCadence}
	for _, host := range []*SDKFeatureHost{a, a, b, b} {
		if err := manager.AddSDKFeatureHost(host); err != nil {
			t.Fatal(err)
		}
	}
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 2 })
	rotated := auth.Clone()
	rotated.Attributes[cliproxyauth.AttributeAPIKey] = "synthetic-new-token"
	manager.Activate(rotated)
	select {
	case <-aEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("A did not receive its token refresh")
	}
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 3 })
	cancelA()
	select {
	case <-aCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing A did not cancel its own request")
	}
	if ctxB.Err() != nil {
		t.Fatal("closing A cancelled B")
	}
	rotatedAgain := rotated.Clone()
	rotatedAgain.Attributes[cliproxyauth.AttributeAPIKey] = "synthetic-newer-token"
	manager.Activate(rotatedAgain)
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 4 })
	manager.Close()
	mu.Lock()
	defer mu.Unlock()
	if calls["warm"] != 1 || calls["a"] != 2 || calls["b"] != 3 || bootstrapCalls != 1 {
		t.Fatal(calls, bootstrapCalls)
	}
	if observations["a"] != 1 || observations["b"] != 3 {
		t.Fatal("stale response or wrong host observer", observations)
	}
}

func TestSDKFeatureHostHealthCannotBeClearedByAnotherHostsSuccess(t *testing.T) {
	manager := NewManager(Options{})
	defer manager.Close()
	a, b := &sdkFeatureRunner{}, &sdkFeatureRunner{}
	manager.recordHostFeatureRefresh(a, false, "http-503")
	manager.recordHostFeatureRefresh(b, true, "")
	manager.recordFeatureRefresh(true, "")
	if manager.Status().FeatureRefreshError != "http-503" {
		t.Fatal("healthy unrelated host cleared failed host")
	}
	manager.recordHostFeatureRefresh(a, true, "")
	if manager.Status().FeatureRefreshError != "" {
		t.Fatal("actual host recovery was not accepted")
	}
}

func TestSDKFeatureHostRejectsRegistrationAfterClose(t *testing.T) {
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t), DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return nil, nil }})
	manager.Close()
	if err := manager.AddSDKFeatureHost(&SDKFeatureHost{Context: t.Context(), Prepare: func(*cliproxyauth.Auth) (string, func([]byte) error, error) { return "session", nil, nil }}); err == nil {
		t.Fatal("closed runtime accepted a new host")
	}
}

func TestSDKFeatureRetryCounterIsNotSatisfiedByAnotherHostsFetch(t *testing.T) {
	var mu sync.Mutex
	calls, ticks := map[string]int{}, map[string]int{}
	bSucceeded := make(chan struct{})
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t), RandomFloat: func() float64 { return 0 },
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role != "startup-sdk-eval" {
					return featureResponse(), nil
				}
				body, _ := io.ReadAll(request.Body)
				var payload struct {
					Attributes struct {
						Session string `json:"sessionId"`
					} `json:"attributes"`
				}
				_ = json.Unmarshal(body, &payload)
				owner := payload.Attributes.Session
				mu.Lock()
				calls[owner]++
				count := calls[owner]
				mu.Unlock()
				if owner == "a" && count == 2 {
					select {
					case <-bSucceeded:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
					return startupResponse(503, ""), nil
				}
				if owner == "b" && count == 2 {
					close(bSucceeded)
				}
				return featureResponse(), nil
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	waitForStartupStatus(t, manager, func(s Status) bool { return s.Completed == 2 })
	for _, owner := range []string{"a", "b"} {
		binding := &SDKFeatureHost{Context: t.Context(),
			Prepare: func(*cliproxyauth.Auth) (string, func([]byte) error, error) {
				return owner, func([]byte) error { return nil }, nil
			},
			Cadence: func(*cliproxyauth.Auth) (time.Duration, bool) {
				mu.Lock()
				defer mu.Unlock()
				ticks[owner]++
				if ticks[owner] == 1 {
					return 5 * time.Millisecond, true
				}
				return time.Hour, false
			},
		}
		if err := manager.AddSDKFeatureHost(binding); err != nil {
			t.Fatal(err)
		}
	}
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshCompleted == 4 && s.FeatureRefreshFailed == 1 })
	manager.Close()
	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 3 || calls["b"] != 2 {
		t.Fatal("another host's successful fetch suppressed A's retry", calls)
	}
}

func TestWarmSDKHostCancellationDoesNotStopApplicationStartupManager(t *testing.T) {
	hostContext, closeHost := context.WithCancel(t.Context())
	defer closeHost()
	started, cancelled := make(chan struct{}), make(chan struct{})
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: featureStartupBundle(t), SDKFeatureContext: hostContext,
		SDKFeatureRefreshCadence: func(*cliproxyauth.Auth) (time.Duration, bool) { return time.Hour, false },
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
				if role != "startup-sdk-eval" {
					return featureResponse(), nil
				}
				close(started)
				<-request.Context().Done()
				close(cancelled)
				return nil, request.Context().Err()
			}), nil
		},
	})
	t.Cleanup(manager.Close)
	manager.Activate(startupTestAuth(t, true))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("warm host did not dispatch")
	}
	closeHost()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("warm host did not cancel")
	}
	if manager.ctx.Err() != nil {
		t.Fatal("warm host cancelled the application")
	}
}

func TestWarmSDKFeatureFaultRetiresWithoutClaimingRecovery(t *testing.T) {
	hostContext, closeHost := context.WithCancel(t.Context())
	defer closeHost()
	manager := NewManager(Options{SDKFeatureContext: hostContext})
	defer manager.Close()
	manager.recordFeatureRefresh(false, "http-503")
	before := manager.Status()
	if before.FeatureRefreshError != "http-503" || before.FeatureRefreshFailed != 1 {
		t.Fatal(before)
	}
	closeHost()
	waitForStartupStatus(t, manager, func(s Status) bool { return s.FeatureRefreshError == "" })
	manager.recordFeatureRefresh(false, "late-http-502")
	manager.recordFeatureRefresh(true, "")
	after := manager.Status()
	if after.FeatureRefreshError != "" || after.FeatureRefreshFailed != before.FeatureRefreshFailed || after.FeatureRefreshCompleted != before.FeatureRefreshCompleted {
		t.Fatal("retired query changed health or fabricated refresh recovery", after)
	}
}
