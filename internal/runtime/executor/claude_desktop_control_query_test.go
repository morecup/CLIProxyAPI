package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type desktopControlQueryDoer func(*http.Request) (*http.Response, error)

func (f desktopControlQueryDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

// The public controller must retire the same remote worker created by real
// ordinary inference admission, not a manually installed manager session.
func TestClaudeDesktopStopRetiresActualRemoteWorker(t *testing.T) {
	for _, archiveStatus := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(fmt.Sprint(archiveStatus), func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			type observed struct {
				account, path string
				body          []byte
			}
			var mu sync.Mutex
			var requests []observed
			var nextID int
			install := func(auth *cliproxyauth.Auth) {
				auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
				inner := accountRuntimeForAuth(t, e, auth.ID).executor
				inner.desktopControlPlane.Close()
				inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
					DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
						return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							mu.Lock()
							requests = append(requests, observed{auth.ID, request.URL.Path, body})
							payload, status := `{}`, http.StatusOK
							switch {
							case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
								nextID++
								payload = fmt.Sprintf(`{"session":{"id":"cse_synthetic_%d"}}`, nextID)
							case strings.HasSuffix(request.URL.Path, "/bridge"):
								payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker-token"}`
							case strings.HasSuffix(request.URL.Path, "/archive"):
								status = archiveStatus
							}
							mu.Unlock()
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
						}), nil
					}})
			}
			for _, auth := range auths {
				install(auth)
			}
			headers := http.Header{"X-Session-Id": {uuid.NewString()}}
			var owners []claudeDesktopQueryContext
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1/messages" {
					owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
				}
				return executionSessionTestResponse(t, request), nil
			})))
			for index, connection := range []string{"target", "sibling", "other-account"} {
				auth := auths[0]
				if index == 2 {
					auth = auths[1]
				}
				if err := invokeQueryLifetimeEntry(ctx, e, auth, headers, "execute", nil, desktopExecutionMetadata(connection)); err != nil {
					t.Fatal(err)
				}
			}
			if e.AccountStatus(auths[0].ID).ControlPlane.Active != 2 || nextID != 3 {
				t.Fatal("ordinary main merged remote worker ownership")
			}
			if e.AccountStatus(auths[0].ID).ControlPlane.PlaceholderPending != 2 || e.AccountStatus(auths[1].ID).ControlPlane.PlaceholderPending != 1 {
				t.Fatal("ordinary admission did not register isolated process-owned placeholders")
			}
			operation := cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: owners[0].desktopSessionID, ExpectedQueryID: owners[0].host.ID()}
			stopped, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation)
			if stopped.Running || stopped.SDKSessionID != owners[0].session || (err != nil) != (archiveStatus != http.StatusOK) {
				t.Fatal("local/remote stop result", stopped, err)
			}
			status := e.AccountStatus(auths[0].ID).ControlPlane
			if status.PlaceholderPending != map[bool]int64{false: 1, true: 2}[archiveStatus != http.StatusOK] {
				t.Fatal("terminal/nonterminal archive did not preserve the correct placeholder", status)
			}
			if status.Active != 1 || status.Retiring != 0 || status.Failed != map[bool]int{false: 0, true: 1}[archiveStatus != http.StatusOK] {
				t.Fatal("remote cleanup failure hidden", status)
			}
			if e.AccountStatus(auths[1].ID).ControlPlane.Active != 1 {
				t.Fatal("other account stopped")
			}
			mu.Lock()
			count := len(requests)
			var shutdown, archives int
			for _, request := range requests {
				if strings.HasSuffix(request.path, "/archive") {
					archives++
					if request.account != auths[0].ID || request.path != "/v1/sessions/session_synthetic_1/archive" {
						t.Error("wrong remote archive", request.path)
					}
				}
				if gjson.GetBytes(request.body, "events.0.payload.subtype").String() == "worker_shutting_down" {
					shutdown++
				}
			}
			mu.Unlock()
			if shutdown != 1 || archives != 1 {
				t.Fatal("missing/duplicate remote cleanup", shutdown, archives)
			}
			_, _ = e.StopDesktopSession(t.Context(), auths[0].ID, operation)
			mu.Lock()
			repeated := len(requests)
			mu.Unlock()
			if count != repeated {
				t.Fatal("repeated stop repeated requests")
			}
			if err := invokeQueryLifetimeEntry(ctx, e, auths[0], headers, "execute", nil, desktopExecutionMetadata("target")); err != nil {
				t.Fatal(err)
			}
			next := owners[len(owners)-1]
			if next.desktopSessionID != stopped.ID || next.session != stopped.SDKSessionID || next.host.ID() == stopped.QueryID || nextID != 3 {
				t.Fatal("resume conflated its new query with the saved remote session")
			}
			mu.Lock()
			var rebridges, unarchives int
			for _, request := range requests {
				if request.path == "/v1/code/sessions/cse_synthetic_1/bridge" {
					rebridges++
				}
				if request.path == "/v1/sessions/session_synthetic_1/unarchive" {
					unarchives++
				}
			}
			mu.Unlock()
			if rebridges != 2 || unarchives != 1 {
				t.Fatal("saved bridge did not obtain a fresh worker", rebridges, unarchives)
			}
			if _, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation); !errors.Is(err, claudesessions.ErrStaleQuery) || next.host.Context().Err() != nil {
				t.Fatal("stale stop retired successor", err)
			}
			encoded, _ := json.Marshal(e.AccountStatus(auths[0].ID))
			if strings.Contains(string(encoded), "synthetic-worker-token") || strings.Contains(string(encoded), "cse_synthetic") {
				t.Fatal("status exposed remote identity")
			}
		})
	}
}

func TestClaudeDesktopSlowRemoteStopJoinsSameRecordBeforeReattach(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auths[0].Metadata["access_token"] = auths[0].Attributes[cliproxyauth.AttributeAPIKey]
	inner := accountRuntimeForAuth(t, e, auths[0].ID).executor
	inner.desktopControlPlane.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var enteredOnce sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var creates int
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				payload := `{}`
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
					creates++
					payload = fmt.Sprintf(`{"session":{"id":"cse_slow_%d"}}`, creates)
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-token"}`
				case request.URL.Path == "/v1/sessions/session_slow_1/archive":
					enteredOnce.Do(func() { close(entered) })
					<-release
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
			}), nil
		}})
	header := http.Header{"X-Session-Id": {uuid.NewString()}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, e, auths[0], header, "execute", nil, desktopExecutionMetadata("reused")); err != nil {
		t.Fatal(err)
	}
	list, err := e.ListDesktopSessions(auths[0].ID)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.StopDesktopSession(t.Context(), auths[0].ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: list[0].ID, ExpectedQueryID: list[0].QueryID})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive did not start")
	}
	resumed := make(chan error, 1)
	go func() {
		resumed <- invokeQueryLifetimeEntry(ctx, e, auths[0], header, "execute", nil, desktopExecutionMetadata("reused"))
	}()
	// The successor will reuse this bridge, so an old archive must finish
	// before reattachment. Unrelated records must remain independently usable.
	sibling := make(chan error, 1)
	go func() {
		sibling <- invokeQueryLifetimeEntry(ctx, e, auths[0], header, "execute", nil, desktopExecutionMetadata("independent"))
	}()
	select {
	case err := <-sibling:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote wait held record admission lock")
	}
	select {
	case err := <-resumed:
		t.Fatal("same bridge was reattached while its old archive was pending", err)
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("successor did not resume after old bridge retirement")
	}
	next, err := e.ListDesktopSessions(auths[0].ID)
	if err != nil || len(next) != 2 || creates != 2 {
		t.Fatal("old retirement overwrote successor", next, err)
	}
	for _, value := range next {
		if value.ID == list[0].ID && (!value.Running || value.QueryID == list[0].QueryID) {
			t.Fatal("stopped generation was not replaced", value)
		}
	}
}
