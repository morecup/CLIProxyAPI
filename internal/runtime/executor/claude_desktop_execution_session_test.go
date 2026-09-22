package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func desktopExecutionMetadata(id string) map[string]any {
	return map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: id}
}

func newExecutionSessionAccountTest(t *testing.T, telemetryFactories ...claudetelemetry.EndpointDoerFactory) (*ClaudeAccountExecutor, []*cliproxyauth.Auth) {
	t.Helper()
	e := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	if len(telemetryFactories) > 0 {
		e.telemetryEndpointDoerFactory = telemetryFactories[0]
	}
	t.Cleanup(e.Close)
	auths := []*cliproxyauth.Auth{
		newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000001", "26100000-0000-4000-8000-000000000001", "37100000-0000-4000-8000-000000000001"),
		newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000002", "26100000-0000-4000-8000-000000000002", "37100000-0000-4000-8000-000000000002"),
	}
	prepareExecutionSessionAccountTest(t, e, auths)
	return e, auths
}

func prepareExecutionSessionAccountTest(t *testing.T, e *ClaudeAccountExecutor, auths []*cliproxyauth.Auth) {
	t.Helper()
	for _, auth := range auths {
		if err := e.Provision(auth); err != nil {
			t.Fatal(err)
		}
		inner := accountRuntimeForAuth(t, e, auth.ID).executor
		inner.desktopControlPlane.Close()
		inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
			StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
			DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
				return nil, errors.New("synthetic control-plane unavailable")
			},
		})
		// Startup and telemetry already have synthetic doers. ATIS is not a
		// startup operation; replace its transport before any model dispatch.
		inner.desktopATIS.doerFactory = func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
			return claudeDesktopATISTestDoerFunc(func(*http.Request) (*http.Response, error) {
				body, err := json.Marshal(map[string]any{"client_data": map[string]any{"atis": "SYNTHETIC-PIN"}, "oauth_account": map[string]any{"account_uuid": auth.Metadata["account_uuid"], "organization_uuid": auth.Metadata["organization_uuid"]}})
				if err != nil {
					return nil, err
				}
				return claudeDesktopATISTestResponse(http.StatusOK, string(body)), nil
			}), nil
		}
	}
}

func executionSessionTestResponse(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	reader, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"id":"msg_session_close","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic response"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
	contentType := "application/json"
	if request.URL.Path == "/v1/messages/count_tokens" {
		payload = `{"input_tokens":10}`
	} else if gjson.GetBytes(body, "stream").Bool() {
		payload = sdkSessionContentResponse(t, `[{"type":"text","text":"synthetic response"}]`, true)
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}, "Request-Id": {"req_session_close"}}, Body: io.NopCloser(strings.NewReader(payload)), ContentLength: -1, Request: request}
}

// These tests enter through the provider router and deliver the existing
// scheduler close notification. No direct Host.Close stands in for the producer.
func TestClaudeAccountExecutionSessionCloseAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream", "count", "http-count"} {
		t.Run(mode, func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			headers := http.Header{"X-Session-Id": {uuid.NewString()}}
			var requests atomic.Int32
			var owners []claudeDesktopQueryContext
			setup := context.WithValue(caller, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				if request.URL.Path == "/v1/messages" {
					owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
				}
				return executionSessionTestResponse(t, request), nil
			})))
			for index, id := range []string{"closing", "sibling", "other-account"} {
				auth := auths[0]
				if index == 2 {
					auth = auths[1]
				}
				if err := invokeQueryLifetimeEntry(setup, e, auth, headers, "execute", nil, desktopExecutionMetadata(id)); err != nil {
					t.Fatal("setup", err)
				}
			}
			if len(owners) != 3 || owners[0].host == nil || owners[0].host == owners[1].host || owners[0].session != owners[1].session || owners[2].session == owners[0].session {
				t.Fatal("ordinary dispatch did not separate connection, query and transcript ownership")
			}
			blocked := make(chan struct{})
			started := make(chan *http.Request, 1)
			ctx := context.WithValue(caller, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				response := executionSessionTestResponse(t, request)
				prefix := "{\"unfinished\":"
				if response.Header.Get("Content-Type") == "text/event-stream" {
					prefix = "data: {\"type\":\"ping\"}\n\n"
				}
				response.Body = &queryLifetimeBlockingBody{ctx: request.Context(), prefix: strings.NewReader(prefix), blocked: blocked}
				started <- request
				return response, nil
			})))
			// Raw HttpRequest has no Options.Metadata argument. Its internal
			// dispatch context carries the same execution handle as helpers do.
			if strings.HasPrefix(mode, "http") {
				var err error
				ctx, err = e.executionSessions.Bind(ctx, desktopExecutionMetadata("closing"))
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				done <- invokeQueryLifetimeEntry(ctx, e, auths[0], headers, mode, nil, desktopExecutionMetadata("closing"))
			}()
			var request *http.Request
			select {
			case request = <-started:
			case err := <-done:
				t.Fatal("stopped before model transport", err)
			case <-time.After(5 * time.Second):
				t.Fatal("model transport was not reached")
			}
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("response body did not block")
			}
			bound := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
			if bound.host != owners[0].host {
				t.Fatal("request did not use its registered query")
			}
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(e)
			manager.CloseExecutionSession("closing")
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("session close did not report cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session close left the response running")
			}
			if caller.Err() != nil || owners[0].host.Context().Err() == nil || owners[1].host.Context().Err() != nil || owners[2].host.Context().Err() != nil {
				t.Fatal("close crossed connection/account scope or cancelled the caller")
			}
			before := requests.Load()
			if err := invokeQueryLifetimeEntry(setup, e, auths[0], headers, "execute", nil, desktopExecutionMetadata("closing")); !errors.Is(err, context.Canceled) || requests.Load() != before {
				t.Fatal("late dispatch resurrected the closed query", err)
			}
			for index, id := range []string{"sibling", "other-account", "resumed"} {
				auth := auths[0]
				if index == 1 {
					auth = auths[1]
				}
				if err := invokeQueryLifetimeEntry(setup, e, auth, headers, "execute", nil, desktopExecutionMetadata(id)); err != nil {
					t.Fatal("surviving dispatch failed", err)
				}
			}
			resumed := owners[len(owners)-1]
			if resumed.host == owners[0].host || resumed.session != owners[0].session {
				t.Fatal("new execution failed to resume the durable transcript")
			}
			for _, auth := range auths {
				runtime := accountRuntimeForAuth(t, e, auth.ID)
				runtime.mu.Lock()
				active, retiring := runtime.active, runtime.retiring
				runtime.mu.Unlock()
				if active != 0 || retiring {
					t.Fatal("individual close retired or leaked the account runtime", active, retiring)
				}
			}
		})
	}
}

func TestClaudeAccountExecutionSessionCloseBeforeRuntimeCreation(t *testing.T) {
	e := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(e.Close)
	e.CloseExecutionSession("queued")
	// Session closure wins before even auth validation/runtime construction.
	if _, err := e.Execute(t.Context(), nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{Metadata: desktopExecutionMetadata("queued")}); !errors.Is(err, context.Canceled) {
		t.Fatal("queued dispatch did not retain the close boundary", err)
	}
	if len(e.runtimes) != 0 {
		t.Fatal("closed dispatch provisioned an account")
	}
}

func TestClaudeDesktopExecutionSessionUnknownHelperCannotBorrowSibling(t *testing.T) {
	e, auth := newQueryLifetimeTestExecutor(t)
	caller := uuid.NewString()
	ctx, native := e.bindClaudeDesktopQueryContext(t.Context(), auth, caller, claudeprofile.RoleMain, desktopExecutionMetadata("main"))
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	helper, session := e.bindClaudeDesktopQueryContext(t.Context(), auth, caller, claudeprofile.RoleCountTokens, desktopExecutionMetadata("helper-without-main"))
	if session != native || helper.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext).host != nil {
		t.Fatal("unknown helper borrowed the sibling query")
	}
	if _, err := e.desktopATIS.Assignment(helper, auth, session, "claude-opus-5", "claude-opus-5", claudeprofile.RoleCountTokens); !errors.Is(err, errClaudeDesktopQueryOwnerUnavailable) {
		t.Fatal("unowned helper lost its explicit diagnostic", err)
	}
	if owner.host.Context().Err() != nil {
		t.Fatal("unknown helper retired main")
	}
}

func TestClaudeAccountExecutionSessionAbandonedStreamStillReleasesRuntime(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocked := make(chan struct{})
	var upstream *http.Request
	ctx := context.WithValue(caller, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response := executionSessionTestResponse(t, request)
		upstream = request
		// Enough complete events to fill the one-slot outer relay, then block
		// its forwarder while the original caller deliberately reads nothing.
		prefix := sdkSessionContentResponse(t, `[{"type":"text","text":"synthetic response"}]`, true)
		prefix, _, _ = strings.Cut(prefix, "data: {\"type\":\"message_delta\"")
		response.Body = &queryLifetimeBlockingBody{ctx: request.Context(), prefix: strings.NewReader(prefix), blocked: blocked}
		return response, nil
	})))
	stream, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"synthetic stream"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: desktopExecutionMetadata("abandoned")})
	if err != nil {
		t.Fatal(err)
	}
	e.CloseExecutionSession("abandoned")
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		runtime.mu.Lock()
		active := runtime.active
		runtime.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("abandoned downstream pinned account request lifetime")
		}
	}
	if caller.Err() != nil || upstream.Context().Err() == nil {
		t.Fatal("execution close lost its independent cancellation source")
	}
	var final error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			final = chunk.Err
		}
	}
	if !errors.Is(final, context.Canceled) {
		t.Fatal("abandoned stream lost terminal cancellation", final)
	}
}

func TestClaudeAccountExecutionSessionAccountClosePreservesDrain(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "provider"}[all], func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			auth := auths[0]
			started, finish := make(chan *http.Request, 1), make(chan struct{})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1/messages" {
					started <- request
					<-finish
				}
				return executionSessionTestResponse(t, request), nil
			})))
			done := make(chan error, 1)
			go func() {
				done <- invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, "execute", nil, desktopExecutionMetadata("draining"))
			}()
			var request *http.Request
			select {
			case request = <-started:
			case <-time.After(5 * time.Second):
				close(finish)
				t.Fatal("request did not start")
			}
			runtime := accountRuntimeForAuth(t, e, auth.ID)
			if all {
				e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
			} else {
				e.CloseAuth(auth.ID)
			}
			if request.Context().Err() != nil {
				close(finish)
				t.Fatal("account drain was changed to immediate query cancellation")
			}
			runtime.mu.Lock()
			retiring, closed := runtime.retiring, runtime.closed
			runtime.mu.Unlock()
			if !retiring || closed {
				close(finish)
				t.Fatal("active request was not held for graceful drain")
			}
			close(finish)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("draining request failed", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("drain never completed")
			}
			runtime.mu.Lock()
			closed, active := runtime.closed, runtime.active
			runtime.mu.Unlock()
			if !closed || active != 0 {
				t.Fatal("finished request did not complete account retirement")
			}
		})
	}
}

func TestClaudeDesktopExecutionSessionDeadlineRemainsAnError(t *testing.T) {
	e, auth := newQueryLifetimeTestExecutor(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, mode := range []string{"execute", "stream", "count", "http", "http-stream", "http-count"} {
		if err := invokeQueryLifetimeEntry(ctx, e, auth, nil, mode, nil, desktopExecutionMetadata("expired")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline became an empty successful response", mode, err)
		}
	}
	router := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(router.Close)
	if _, err := router.Execute(ctx, auth, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("router lost caller deadline", err)
	}
}

func TestClaudeAccountExecutionSessionCloseRetiresOnlyRegisteredQueries(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	var owners []claudeDesktopQueryContext
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages" {
			owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
		}
		return executionSessionTestResponse(t, request), nil
	})))
	for index, auth := range []*cliproxyauth.Auth{auths[0], auths[0], auths[1], auths[1]} {
		id := "account-retry-connection"
		if index == 3 {
			id = "unrelated-connection"
		}
		if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, "execute", nil, desktopExecutionMetadata(id)); err != nil {
			t.Fatal(err)
		}
	}
	e.CloseExecutionSession("account-retry-connection")
	if len(owners) != 4 {
		t.Fatal("missing query dispatch")
	}
	for index, owner := range owners {
		if (owner.host.Context().Err() != nil) != (index < 3) {
			t.Fatal("close missed an actual owner or guessed another one", index)
		}
	}
}

func TestClaudeAccountExecutionSessionRawEOFReleasesWithoutClosingQuery(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	var owner claudeDesktopQueryContext
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages" {
			owner = request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		}
		return executionSessionTestResponse(t, request), nil
	})))
	ctx, err := e.executionSessions.Bind(ctx, desktopExecutionMetadata("raw"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewBufferString(`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"synthetic raw input"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := e.HttpRequest(nil, auth, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	runtime.mu.Lock()
	active := runtime.active
	runtime.mu.Unlock()
	if active != 0 || owner.host == nil || owner.host.Context().Err() != nil {
		t.Fatal("EOF leaked a request or closed its query", active)
	}
	e.CloseExecutionSession("raw")
	if owner.host.Context().Err() == nil {
		t.Fatal("nil explicit context discarded raw request's execution binding")
	}
}

func TestClaudeDesktopExecutionSessionDirectExecutorClose(t *testing.T) {
	e, auth := newQueryLifetimeTestExecutor(t)
	ctx, native := e.bindClaudeDesktopQueryContext(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain, desktopExecutionMetadata("direct"))
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	e.CloseExecutionSession("direct")
	if owner.host.Context().Err() == nil {
		t.Fatal("direct executor ignored close")
	}
	helper := cliproxyexecutor.WithClaudeDesktopSessionBinding(context.WithoutCancel(ctx), cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: native})
	helper, _ = e.bindClaudeDesktopQueryContext(helper, auth, native, claudeprofile.RoleCountTokens)
	bound, release := e.beginClaudeDesktopQueryLifetime(helper)
	defer release()
	if bound.Err() != context.Canceled {
		t.Fatal("direct close allowed a late helper to restart")
	}
}
