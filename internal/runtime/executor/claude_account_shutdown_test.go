package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type shutdownCredentialSource struct{ refreshes atomic.Int32 }

func (*shutdownCredentialSource) Current(auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth.Clone(), nil
}

func TestClaudeAccountQuarantineInterruptsFinalTelemetryFlush(t *testing.T) {
	started := make(chan *http.Request, 1)
	canceled, release, closed, quarantined := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	closing, quarantining := false, false
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	protocols := map[string]string{"sdk-event-logging": "http/1.1", "desktop-event-logging": "http/2"}
	for _, delivery := range bundle.AuxiliaryTelemetry.All() {
		protocols[delivery.EndpointRole] = delivery.Protocol
	}
	e, auths := newExecutionSessionAccountTest(t, func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if role == "sdk-event-logging" {
				for _, event := range gjson.GetBytes(body, "events").Array() {
					if event.Get("event_data.event_name").String() == "tengu_bridge_repl_teardown" {
						started <- request
						select {
						case <-request.Context().Done():
							close(canceled)
						case <-release:
						}
						<-release
						return nil, request.Context().Err()
					}
				}
			}
			response := &http.Response{StatusCode: 204, Proto: "HTTP/2.0", ProtoMajor: 2, Body: io.NopCloser(strings.NewReader(""))}
			if protocols[role] == "http/1.1" {
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
			}
			return response, nil
		})
	})
	t.Cleanup(func() {
		unblock()
		if closing {
			awaitAccountShutdown(t, closed, "final flush cleanup")
		}
		if quarantining {
			awaitAccountShutdown(t, quarantined, "final flush quarantine cleanup")
		}
	})
	auth := auths[0]
	auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	inner.desktopControlPlane.Close()
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
		StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				payload := `{}`
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
					payload = `{"session":{"id":"cse_FinalFlush"}}`
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
			}), nil
		},
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, "execute", nil, desktopExecutionMetadata("final-flush")); err != nil {
		t.Fatal(err)
	}
	closing = true
	go func() { defer close(closed); e.CloseAuth(auth.ID) }()
	select {
	case request := <-started:
		if _, hasDeadline := request.Context().Deadline(); hasDeadline {
			t.Fatal("application final flush installed a network deadline")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("actual teardown event did not reach final delivery")
	}
	quarantining = true
	go func() { defer close(quarantined); e.QuarantineAuth(auth.ID) }()
	awaitAccountShutdown(t, canceled, "final flush cancellation")
	if during := e.AccountStatus(auth.ID); !during.RuntimeStopping || during.LifecycleState != claudeDesktopAppRunning {
		t.Fatal("application published completion before delivery joined", during)
	}
	unblock()
	awaitAccountShutdown(t, closed, "final flush close")
	awaitAccountShutdown(t, quarantined, "final flush quarantine")
	after := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auth.ID)
	if after.LifecycleState != claudeDesktopAppQuarantined || after.RuntimeLoaded || after.RuntimeStopping {
		t.Fatal("late quarantine lost its durable disposition", after)
	}
	if sibling := e.AccountStatus(auths[1].ID); !sibling.RuntimeLoaded || sibling.State != claudedesktop.EnrollmentActive {
		t.Fatal("final flush quarantine affected another account", sibling)
	}
}

func (s *shutdownCredentialSource) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	s.refreshes.Add(1)
	return auth.Clone(), nil
}

func awaitAccountShutdown(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal(name + " did not join")
	}
}

// A graceful close must remain addressable by the public account quarantine
// entry until its actual network cleanup and final checkpoint have joined.
func TestClaudeAccountQuarantineDuringShutdownAcrossEntries(t *testing.T) {
	for _, entry := range []string{"execute", "stream", "http", "http-stream"} {
		for _, provider := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/provider-%t", entry, provider), func(t *testing.T) {
				e, auths := newExecutionSessionAccountTest(t)
				auth := auths[0]
				auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
				runtimeRef := accountRuntimeForAuth(t, e, auth.ID)
				inner := runtimeRef.executor
				inner.desktopControlPlane.Close()
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				closed, quarantined := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				closing, quarantining := false, false
				t.Cleanup(func() {
					unblock()
					if closing {
						awaitAccountShutdown(t, closed, "close cleanup")
					}
					if quarantining {
						awaitAccountShutdown(t, quarantined, "quarantine cleanup")
					}
				})
				credentials := &shutdownCredentialSource{}
				var requests, archives atomic.Int32
				inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
					StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true, Credentials: credentials,
					DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
						return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
							requests.Add(1)
							payload, status := `{}`, 200
							switch {
							case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
								payload = `{"session":{"id":"cse_ShutdownQuarantine"}}`
							case strings.HasSuffix(request.URL.Path, "/bridge"):
								payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
							case strings.HasSuffix(request.URL.Path, "/archive"):
								if archives.Add(1) == 1 {
									close(started)
									select {
									case <-request.Context().Done():
										close(canceled)
									case <-release:
									}
									// Cancellation is not proof that a transport has joined.
									<-release
								}
								status = 401
							}
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
						}), nil
					},
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					return executionSessionTestResponse(t, request), nil
				})))
				if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, entry, nil, desktopExecutionMetadata("shutdown-quarantine")); err != nil {
					t.Fatal(err)
				}
				before := e.AccountStatus(auth.ID)
				closing = true
				go func() {
					defer close(closed)
					if provider {
						e.Close()
					} else {
						e.CloseAuth(auth.ID)
					}
				}()
				awaitAccountShutdown(t, started, "archive admission")
				beforeRequests := requests.Load()
				beforeQueue := h73TelemetryObligations(inner.desktopTelemetry.Status())
				quarantining = true
				go func() { defer close(quarantined); e.QuarantineAuth(auth.ID) }()
				select {
				case <-canceled:
				case <-quarantined:
					t.Fatal("public quarantine lost the draining runtime")
				case <-time.After(10 * time.Second):
					t.Fatal("public quarantine did not cancel cleanup")
				}
				during := e.AccountStatus(auth.ID)
				if !during.RuntimeStopping || during.RuntimeLoaded || during.LifecycleState != claudeDesktopAppRunning || during.State != claudedesktop.EnrollmentQuarantined {
					t.Fatal("cancellation was mistaken for terminal cleanup", during)
				}
				if acquired, err := e.acquireRuntime(auth); err == nil || acquired != nil {
					if acquired != nil {
						acquired.release()
					}
					t.Fatal("quarantined lifetime admitted a successor")
				}
				unblock()
				awaitAccountShutdown(t, closed, "close")
				awaitAccountShutdown(t, quarantined, "quarantine")
				after := e.AccountStatus(auth.ID)
				if after.RuntimeStopping || after.RuntimeLoaded || after.LifecycleState != claudeDesktopAppQuarantined || after.AppSessionHash != before.AppSessionHash || after.LifecycleGen != before.LifecycleGen {
					t.Fatal("terminal disposition or application identity was lost", after)
				}
				if requests.Load() != beforeRequests || archives.Load() != 1 || credentials.refreshes.Load() != 0 {
					t.Fatal("quarantine issued new teardown traffic or refreshed credentials")
				}
				if afterQueue := h73TelemetryObligations(inner.desktopTelemetry.Status()); afterQueue != beforeQueue {
					t.Fatal("quarantine changed durable obligations with a synthetic quit", beforeQueue, afterQueue)
				}
				restored := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auth.ID)
				if restored.ControlPlane != after.ControlPlane || restored.LifecycleState != claudeDesktopAppQuarantined {
					t.Fatal("terminal quarantine was not durable", restored)
				}
				if !provider {
					sibling := e.AccountStatus(auths[1].ID)
					if !sibling.RuntimeLoaded || sibling.RuntimeStopping || sibling.State != claudedesktop.EnrollmentActive {
						t.Fatal("quarantine disrupted the other account", sibling)
					}
					if err := invokeQueryLifetimeEntry(ctx, e, auths[1], http.Header{"X-Session-Id": {uuid.NewString()}}, entry, nil, desktopExecutionMetadata("unaffected-account")); err != nil {
						t.Fatal("other account stopped serving", err)
					}
				}
				// Repeated notifications cannot rewrite the exact terminal lifetime.
				e.QuarantineAuth(auth.ID)
				e.CloseAuth(auth.ID)
				if last := e.AccountStatus(auth.ID); last.StoppedAt != after.StoppedAt || last.LifecycleState != after.LifecycleState {
					t.Fatal("repeated shutdown rewrote terminal history", last)
				}
			})
		}
	}
}
