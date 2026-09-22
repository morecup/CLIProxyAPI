package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ATIS and model transports are synthetic, optional telemetry is disabled,
// and this direct executor has neither startup nor control-plane senders.
func newFirstInputTestExecutor(t *testing.T) (*ClaudeExecutor, *cliproxyauth.Auth) {
	t.Helper()
	e, auth := newQueryLifetimeTestExecutor(t)
	if e.desktopControlPlane != nil || e.desktopStartup != nil || e.desktopTelemetry != nil {
		t.Fatal("first-input fixture unexpectedly enabled an unisolated sender")
	}
	return e, auth
}

func TestClaudeDesktopFirstInputFailureAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, failure := range []string{"planning", "cancel-before-entry", "cancel-during-bind"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				e, auth := newFirstInputTestExecutor(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				warm := e.desktopATIS.featureHosts.Warm()
				var calls atomic.Int32
				var resumed claudeDesktopQueryContext
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					if request.URL.Path == "/v1/messages" {
						resumed = request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
					}
					return executionSessionTestResponse(t, request), nil
				})))
				e.desktopATIS.featureStateObserver = func(*cliproxyauth.Auth, string, bool) error {
					if failure == "planning" {
						// Fail the real planner after eligibility and query binding.
						e.desktopProfileErr = errors.New("synthetic-first-input-plan-failure")
					} else if failure == "cancel-during-bind" {
						cancel()
					}
					return nil
				}
				if failure == "cancel-before-entry" {
					cancel()
				}
				err := invokeQueryLifetimeEntry(ctx, e, auth, headers, mode, nil)
				unclaimed := failure == "cancel-before-entry"
				if err == nil || calls.Load() != 0 || (warm.Context().Err() == nil) != unclaimed {
					t.Fatal("unaccepted input dispatched or violated query claim ownership", err, calls.Load(), unclaimed)
				}
				if failure == "planning" {
					var planning claudeDesktopPlanningError
					if !errors.As(err, &planning) || !strings.Contains(err.Error(), "synthetic-first-input-plan-failure") {
						t.Fatal("cleanup replaced the original planning failure", err)
					}
				} else {
					var scoped interface{ IsRequestScoped() bool }
					if !errors.Is(err, context.Canceled) || !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
						t.Fatal("input cancellation lost request-scoped classification", err)
					}
				}
				if e.desktopATIS.featureHosts.LookupSession(warm.SessionID()) != nil {
					t.Fatal("dropped first input retained a registered query")
				}
				e.desktopProfileErr = nil
				e.desktopATIS.featureStateObserver = nil
				// Resolve checks cancellation before invoking the query factory.
				// Such a caller never claimed the warm Host or persisted an alias;
				// the next real input must still use that original warm Host.
				// Post-claim failures instead retire only the failed generation.
				if err := invokeQueryLifetimeEntry(context.WithoutCancel(ctx), e, auth, headers, mode, nil); err != nil {
					t.Fatal("next main could not resume after a dropped claim", err)
				}
				if resumed.host == nil || (resumed.host == warm) != unclaimed || resumed.host.Context().Err() != nil || resumed.session != warm.SessionID() {
					t.Fatal("next main revived a failed query or discarded its durable alias")
				}
			})
		}
	}
}

type firstInputFailedBody struct{}

func (firstInputFailedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (firstInputFailedBody) Close() error             { return nil }

func TestClaudeDesktopFirstInputAcceptedBeforeAPIFailure(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, failure := range []string{"api-502", "network", "response-body"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				e, auth := newFirstInputTestExecutor(t)
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				var owner claudeDesktopQueryContext
				failed := true
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path == "/v1/messages" {
						bound := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
						if owner.host != nil && owner.host != bound.host {
							t.Error("retry switched query generation after accepted input")
						}
						owner = bound
						lease, err := e.beginClaudeDesktopInput(request.Context(), claudeprofile.RoleMain)
						if lease != nil || err != nil {
							t.Error("API dispatch preceded first input admission", err)
						}
					}
					if failed && failure == "network" {
						return nil, errors.New("synthetic-first-input-network-failure")
					}
					response := executionSessionTestResponse(t, request)
					if failed {
						if failure == "api-502" {
							response.StatusCode = http.StatusBadGateway
							response.Header.Set("Content-Type", "application/json")
							response.Body = io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"server_error","message":"synthetic failure"}}`))
						} else {
							response.Body = firstInputFailedBody{}
						}
					}
					return response, nil
				})))
				err := invokeQueryLifetimeEntry(ctx, e, auth, headers, mode, nil)
				// Raw HTTP preserves the upstream status instead of making it a Go error.
				if !(failure == "api-502" && strings.HasPrefix(mode, "http")) && err == nil {
					t.Fatal("synthetic model failure was not exercised")
				}
				if owner.host == nil || owner.host.Context().Err() != nil {
					t.Fatal("post-admission API failure retired the query", err)
				}
				failed = false
				if err := invokeQueryLifetimeEntry(ctx, e, auth, headers, mode, nil); err != nil {
					t.Fatal("accepted query could not continue after API failure", err)
				}
				// A later pre-admission planning failure is not a first-input drop.
				e.desktopATIS.featureStateObserver = func(*cliproxyauth.Auth, string, bool) error {
					e.desktopProfileErr = errors.New("synthetic-later-plan-failure")
					return nil
				}
				if err := invokeQueryLifetimeEntry(ctx, e, auth, headers, mode, nil); err == nil || owner.host.Context().Err() != nil {
					t.Fatal("later planning failure dropped an already accepted query", err)
				}
			})
		}
	}
}

func TestClaudeDesktopFirstInputHelpersDoNotClaimOrDropMain(t *testing.T) {
	e, auth := newFirstInputTestExecutor(t)
	ctx, _ := e.bindClaudeDesktopQueryContext(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain)
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleCompaction, claudeprofile.RoleSubagent, claudeprofile.RoleCountTokens} {
		lease, err := e.beginClaudeDesktopInput(ctx, role)
		if err != nil || lease != nil {
			t.Fatal("helper claimed a main input", role, err)
		}
		lease.Close()
	}
	lease, err := e.beginClaudeDesktopInput(ctx, claudeprofile.RoleMain)
	if err != nil || lease == nil || owner.host.Context().Err() != nil {
		t.Fatal("helper failure consumed or retired the provisional main", err)
	}
	lease.Close()
}

func TestClaudeDesktopFirstInputFailedCountDoesNotConsumePrewarm(t *testing.T) {
	for _, mode := range []string{"count", "http-count"} {
		t.Run(mode, func(t *testing.T) {
			e, auth := newFirstInputTestExecutor(t)
			warm := e.desktopATIS.featureHosts.Warm()
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("synthetic-count-failure")
			})))
			if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, mode, nil); err == nil {
				t.Fatal("count failure was not exercised")
			}
			if warm.Context().Err() != nil {
				t.Fatal("failed count retired the warm query")
			}
			lease, err := warm.BeginInput(t.Context(), nil)
			if err != nil || lease == nil {
				t.Fatal("token count consumed first input", err)
			}
			lease.Close()
		})
	}
}

func TestClaudeAccountFirstInputDropIsolatesConnectionsAndAccounts(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	inner := accountRuntimeForAuth(t, e, auths[0].ID).executor
	headers := http.Header{"X-Session-Id": {uuid.NewString()}}
	var owners []claudeDesktopQueryContext
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages" {
			owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
		}
		return executionSessionTestResponse(t, request), nil
	})))
	for i, execution := range []string{"sibling", "other-account"} {
		if err := invokeQueryLifetimeEntry(ctx, e, auths[i], headers, "execute", nil, desktopExecutionMetadata(execution)); err != nil {
			t.Fatal("sibling setup failed", err)
		}
	}
	if len(owners) != 2 || owners[0].host == owners[1].host {
		t.Fatal("test did not establish independent account queries")
	}
	var failed *claudefeatures.Host
	inner.desktopATIS.startFeatureHost = func(host *claudefeatures.Host) error {
		if failed == nil {
			failed = host
		}
		return nil
	}
	inner.desktopATIS.featureStateObserver = func(*cliproxyauth.Auth, string, bool) error {
		inner.desktopProfileErr = errors.New("synthetic-unaccepted-input")
		return nil
	}
	err := invokeQueryLifetimeEntry(ctx, e, auths[0], headers, "execute", nil, desktopExecutionMetadata("failed-first-input"))
	if err == nil || failed == nil || failed.Context().Err() == nil || len(owners) != 2 || failed.SessionID() != owners[0].session {
		t.Fatal("failed connection did not drop only its unaccepted query", err)
	}
	for _, owner := range owners {
		if owner.host.Context().Err() != nil {
			t.Fatal("failed input retired a sibling connection or account")
		}
	}
	inner.desktopProfileErr = nil
	inner.desktopATIS.featureStateObserver = nil
	if err := invokeQueryLifetimeEntry(ctx, e, auths[0], headers, "execute", nil, desktopExecutionMetadata("failed-first-input")); err != nil {
		t.Fatal("input drop incorrectly tombstoned the execution connection", err)
	}
	if len(owners) != 3 || owners[2].host == failed || owners[2].host == owners[0].host || owners[2].session != owners[0].session {
		t.Fatal("new input did not get a fresh query on its own resumable transcript")
	}
}
