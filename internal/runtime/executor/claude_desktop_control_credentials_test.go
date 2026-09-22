package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClaudeDesktopControlRefreshUsesOwnerStoreAcrossEntries(t *testing.T) {
	scenarios := []string{"query/execute", "query/stream", "query/http", "account/execute", "account/stream", "account/http", "serialized/query/execute", "serialized/query/stream", "serialized/query/http", "serialized/account/execute", "serialized/account/stream", "serialized/account/http"}
	for _, budget := range []string{"budget-199", "budget-200"} {
		for _, stop := range []string{"query", "account"} {
			for _, entry := range []string{"execute", "stream", "http", "http-stream"} {
				scenarios = append(scenarios, budget+"/"+stop+"/"+entry)
			}
		}
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			belowBudget, atBudget := strings.HasPrefix(scenario, "budget-199/"), strings.HasPrefix(scenario, "budget-200/")
			parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(scenario, "serialized/"), "budget-199/"), "budget-200/"), "/")
			retiring, entry := parts[0] == "account", parts[1]
			e, auths := newExecutionSessionAccountTest(t)
			auth := auths[0]
			auth.FileName = "synthetic-desktop.json"
			auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
			auth.Metadata["refresh_token"] = "synthetic-old-refresh"
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(t.TempDir())
			registry := cliproxyauth.NewManager(store, nil, nil)
			registry.RegisterExecutor(e)
			registered, err := registry.Register(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			inner := accountRuntimeForAuth(t, e, auth.ID).executor
			inner.desktopControlPlane.Close()
			var acquisitions atomic.Int32
			credentials := &helps.ClaudeDesktopControlCredentials{Manager: registry, Owner: e, Acquire: func(_ context.Context, current *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
				acquisitions.Add(1)
				current.Metadata["access_token"] = "sk-ant-oat-synthetic-new-access"
				current.Metadata["refresh_token"] = "synthetic-new-refresh"
				if strings.HasPrefix(scenario, "serialized/") {
					encoded, err := json.Marshal(current)
					if err != nil {
						return nil, err
					}
					var decoded cliproxyauth.Auth
					decoder := json.NewDecoder(strings.NewReader(string(encoded)))
					decoder.UseNumber()
					if err = decoder.Decode(&decoded); err != nil {
						return nil, err
					}
					return &decoded, nil
				}
				return current, nil
			}}
			snapshot, err := credentials.Current(registered)
			if err != nil {
				t.Fatal("initial owned credential", err)
			}
			if _, err = credentials.Current(snapshot); err != nil {
				t.Fatal("owned credential snapshot roundtrip", err)
			}
			var archives int
			var clock atomic.Int64
			clock.Store(10000)
			inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true, Credentials: credentials,
				Now: func() time.Time { return time.UnixMilli(clock.Load()) },
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
					return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
						payload, status := `{}`, 200
						switch {
						case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
							payload = `{"session":{"id":"cse_owned_refresh"}}`
						case strings.HasSuffix(request.URL.Path, "/bridge"):
							payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
						case strings.HasSuffix(request.URL.Path, "/archive"):
							archives++
							if archives == 1 {
								status = 401
								if belowBudget {
									clock.Add(1301)
								} else if atBudget {
									clock.Add(1300)
								}
							} else if request.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic-new-access" {
								return nil, fmt.Errorf("archive did not use refreshed credential")
							}
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
					}), nil
				}})
			var owner claudeDesktopQueryContext
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1/messages" {
					owner = request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
				}
				return executionSessionTestResponse(t, request), nil
			})))
			if err = invokeQueryLifetimeEntry(ctx, e, registered, http.Header{"X-Session-Id": {uuid.NewString()}}, entry, nil, desktopExecutionMetadata("owned-refresh")); err != nil {
				t.Fatal(err)
			}
			before := e.AccountStatus(auth.ID)
			if retiring {
				e.CloseAuth(auth.ID)
			} else {
				_, err = e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: owner.desktopSessionID, ExpectedQueryID: owner.host.ID()})
			}
			wantAttempts, wantAcquisitions := 2, int32(1)
			wantToken, wantRefresh := "sk-ant-oat-synthetic-new-access", "synthetic-new-refresh"
			if belowBudget {
				wantAttempts, wantAcquisitions = 1, 0
				wantToken, wantRefresh = claudeCredsToken(registered), "synthetic-old-refresh"
			}
			if (err != nil) != (belowBudget && !retiring) || archives != wantAttempts || acquisitions.Load() != wantAcquisitions {
				t.Fatal("stop/refresh", err, archives, acquisitions.Load())
			}
			current, ok := registry.GetByID(auth.ID)
			if !ok || claudeCredsToken(current) != wantToken {
				t.Fatal("inference credential rotation disagreed with retry eligibility")
			}
			after := e.AccountStatus(auth.ID)
			if after.AppSessionHash != before.AppSessionHash || after.LifecycleGen != before.LifecycleGen || (after.ControlPlane.Failed != 0) != belowBudget {
				t.Fatal("refresh changed application lifetime", after)
			}
			if retiring && after.RuntimeLoaded {
				t.Fatal("cleanup reentered the retiring application")
			}
			if (inner.desktopControlPlane.Status().Failed != 0) != belowBudget {
				t.Fatal("retirement failure hidden")
			}
			path := filepath.Join(filepath.Dir(current.Attributes[cliproxyauth.AttributePath]), auth.FileName)
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "synthetic-new-access") || strings.Contains(string(encoded), "synthetic-new-refresh") {
				t.Fatal("plaintext credentials persisted")
			}
			var metadata map[string]any
			if err = json.Unmarshal(encoded, &metadata); err != nil {
				t.Fatal(err)
			}
			if err = claudedesktop.HydrateMetadata(path, metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["access_token"] != wantToken || metadata["refresh_token"] != wantRefresh {
				t.Fatal("durable credentials disagreed with retry eligibility")
			}
			if retiring {
				// This reader has no live or retired runtime references. A zero
				// result cannot be rescued by the old worker's in-memory status.
				restored := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auth.ID)
				if restored.ControlPlane != after.ControlPlane || restored.RuntimeLoaded || restored.RuntimeStopping || restored.AppSessionHash != after.AppSessionHash {
					t.Fatal("final control-plane status did not survive reconstruction", restored)
				}
			}
			if retiring || belowBudget {
				return
			}
			// A watcher reload changes the randomized envelope, not the owner.
			reloaded := current.Clone()
			reloaded.Metadata = metadata
			if _, err = registry.Update(t.Context(), reloaded); err != nil {
				t.Fatal(err)
			}
			if _, err = credentials.Current(current); err != nil {
				t.Fatal("protected envelope reload changed owner", err)
			}
			if e.AccountStatus(auth.ID).State != claudedesktop.EnrollmentActive || e.AccountStatus(auth.ID).LifecycleGen != before.LifecycleGen {
				t.Fatal("credential reload quarantined or recreated the application")
			}
		})
	}
}

func TestClaudeDesktopAccountCloseWaitsForFinalControlCheckpoint(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	inner.desktopControlPlane.Close()
	archiveStarted, releaseArchive, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(releaseArchive)
		}
	}
	closingStarted := false
	t.Cleanup(func() {
		unblock()
		if closingStarted {
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Error("application close did not join")
			}
		}
	})
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				payload, status := `{}`, 200
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
					payload = `{"session":{"id":"cse_FinalCheckpoint"}}`
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
				case strings.HasSuffix(request.URL.Path, "/archive"):
					close(archiveStarted)
					<-releaseArchive
					status = 502
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
			}), nil
		}})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, "execute", nil, desktopExecutionMetadata("final-checkpoint")); err != nil {
		t.Fatal(err)
	}
	before := e.AccountStatus(auth.ID)
	closingStarted = true
	go func() { defer close(closed); e.CloseAuth(auth.ID) }()
	select {
	case <-archiveStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("archive did not start")
	}
	during := e.AccountStatus(auth.ID)
	if !during.RuntimeStopping || during.RuntimeLoaded || during.ControlPlane.Retiring != 1 || during.LifecycleState != claudeDesktopAppRunning {
		t.Fatal("Close entry was mistaken for completed cleanup", during)
	}
	if acquired, err := e.acquireRuntime(auth); err == nil || acquired != nil {
		if acquired != nil {
			acquired.release()
		}
		t.Fatal("successor started while old application cleanup was pending", err)
	}
	if sibling := e.AccountStatus(auths[1].ID); !sibling.RuntimeLoaded || sibling.RuntimeStopping {
		t.Fatal("other account was retired")
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not finish")
	}
	after := e.AccountStatus(auth.ID)
	if after.RuntimeStopping || after.RuntimeLoaded || after.ControlPlane.Failed != 1 || after.ControlPlane.Retiring != 0 || after.AppSessionHash != before.AppSessionHash {
		t.Fatal("joined cleanup lost the failure or application identity", after)
	}
	restored := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auth.ID)
	if restored.ControlPlane != after.ControlPlane || restored.LifecycleState != claudeDesktopAppStopped {
		t.Fatal("final failure was not checkpointed", restored)
	}
}
