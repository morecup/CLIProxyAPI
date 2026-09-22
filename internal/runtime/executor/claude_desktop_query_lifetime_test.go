package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// The transport is synthetic; main ownership is established by the ordinary
// executor entry. Closing that observed host is not a native Desktop UI action.
func newQueryLifetimeTestExecutor(t *testing.T) (*ClaudeExecutor, *cliproxyauth.Auth) {
	t.Helper()
	e, auth := newQueryOwnerTestExecutor(t)
	// Request cancellation must work without optional telemetry. Never let a
	// synthetic inference test use the production telemetry sender.
	e.desktopTelemetry.Close()
	e.desktopTelemetry = nil
	return e, auth
}

func TestClaudeDesktopQueryLifetimeAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream", "count", "http-count"} {
		for _, outcome := range []string{"retire", "caller-cancel", "success"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				e, auth := newQueryLifetimeTestExecutor(t)
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				caller, cancelCaller := context.WithCancel(t.Context())
				defer cancelCaller()
				requests := make(chan *http.Request, 1)
				blocked := make(chan struct{})
				warmup := mode == "count" || mode == "http-count"
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					reader, err := request.GetBody()
					if err != nil {
						return nil, err
					}
					body, err := io.ReadAll(reader)
					_ = reader.Close()
					if err != nil {
						return nil, err
					}
					counting := request.URL.Path == "/v1/messages/count_tokens"
					streaming := gjson.GetBytes(body, "stream").Bool()
					payload := `{"id":"msg_lifetime","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic response"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
					contentType := "application/json"
					if counting {
						payload = `{"input_tokens":10}`
					} else if streaming {
						payload = sdkSessionContentResponse(t, `[{"type":"text","text":"synthetic response"}]`, true)
						contentType = "text/event-stream"
					}
					responseBody := io.ReadCloser(io.NopCloser(strings.NewReader(payload)))
					// A success calibration helper is not the request under test.
					target := !warmup && (counting == (mode == "count" || mode == "http-count"))
					if target {
						requests <- request
						if outcome != "success" {
							prefix := "{\"unfinished\":"
							if streaming {
								prefix = "data: {\"type\":\"ping\"}\n\n"
							}
							responseBody = &queryLifetimeBlockingBody{ctx: request.Context(), prefix: strings.NewReader(prefix), blocked: blocked}
						}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}, "Request-Id": {"req_lifetime"}}, Body: responseBody, ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(caller, "cliproxy.roundtripper", http.RoundTripper(transport))
				if warmup {
					if err := invokeQueryLifetimeEntry(ctx, e, auth, headers, "execute", nil); err != nil {
						t.Fatal("ordinary main setup failed", err)
					}
					warmup = false
				}
				done := make(chan error, 1)
				ready := make(chan struct{})
				go func() { done <- invokeQueryLifetimeEntry(ctx, e, auth, headers, mode, ready) }()
				var request *http.Request
				select {
				case request = <-requests:
				case err := <-done:
					t.Fatal("request stopped before transport", err)
				case <-time.After(3 * time.Second):
					t.Fatal("request did not reach the synthetic transport")
				}
				owner, _ := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
				if owner.host == nil || owner.host.Context().Err() != nil {
					t.Fatal("ordinary request did not bind a live query")
				}
				if outcome != "success" {
					select {
					case <-blocked:
					case <-time.After(3 * time.Second):
						t.Fatal("request never reached its pending body read")
					}
					if mode == "stream" || strings.HasPrefix(mode, "http") {
						select {
						case <-ready:
						case <-time.After(time.Second):
							t.Fatal("response was not handed to its caller")
						}
					}
					if request.Context().Err() != nil {
						t.Fatal("header handoff cancelled a live response")
					}
					if outcome == "retire" {
						owner.host.Close()
					} else {
						cancelCaller()
					}
				}
				select {
				case err := <-done:
					if outcome == "success" && err != nil {
						t.Fatal("request cleanup turned success into cancellation", err)
					}
					if outcome != "success" && !errors.Is(err, context.Canceled) {
						t.Fatal("cancelled request lost its terminal cause", err)
					}
				case <-time.After(3 * time.Second):
					cancelCaller()
					<-done
					t.Fatal("query retirement left model response running")
				}
				if outcome != "retire" && owner.host.Context().Err() != nil {
					t.Fatal("one request completion retired the whole query")
				}
				if request.Context().Err() != context.Canceled {
					t.Fatal("completed request retained its live cancellation subscription")
				}
			})
		}
	}
}

func TestClaudeDesktopQueryLifetimeRejectsDetachedRetiredHelper(t *testing.T) {
	e, auth := newQueryLifetimeTestExecutor(t)
	ctx, session := e.bindClaudeDesktopQueryContext(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain)
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	ctx, release := e.beginClaudeDesktopQueryLifetime(ctx)
	release()
	if owner.host.Context().Err() != nil {
		t.Fatal("completed main retired its query")
	}
	detached := cliproxyexecutor.WithClaudeDesktopSessionBinding(context.WithoutCancel(ctx), cliproxyexecutor.ClaudeDesktopSessionBinding{
		AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: session})
	owner.host.Close()
	for range 2 {
		detached, _ = e.bindClaudeDesktopQueryContext(detached, auth, uuid.NewString(), claudeprofile.RoleCountTokens)
		bound := detached.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if bound.host != nil || !errors.Is(bound.unavailable, errClaudeDesktopQueryOwnerUnavailable) {
			t.Fatal("retired helper regained feature ownership")
		}
		request, release := e.beginClaudeDesktopQueryLifetime(detached)
		if request.Err() != context.Canceled {
			t.Fatal("detached helper escaped query retirement")
		}
		release()
	}
}

func TestClaudeDesktopQueryLifetimeRetirementDuringBinding(t *testing.T) {
	e, auth := newQueryLifetimeTestExecutor(t)
	host := e.desktopATIS.featureHosts.Warm()
	// This callback runs after registry resolution, before owner publication.
	e.desktopATIS.featureStateObserver = func(*cliproxyauth.Auth, string, bool) error {
		host.Close()
		return nil
	}
	ctx, _ := e.bindClaudeDesktopQueryContext(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain)
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	if owner.host != nil || owner.lifetime != host.Context() || !errors.Is(owner.unavailable, errClaudeDesktopQueryOwnerUnavailable) {
		t.Fatal("concurrent retirement discarded cancellation or published a live owner")
	}
	request, release := e.beginClaudeDesktopQueryLifetime(ctx)
	defer release()
	if request.Err() != context.Canceled {
		t.Fatal("request escaped concurrent query retirement")
	}
}

func invokeQueryLifetimeEntry(ctx context.Context, e cliproxyauth.ProviderExecutor, auth *cliproxyauth.Auth, headers http.Header, mode string, ready chan struct{}, metadata ...map[string]any) error {
	body := []byte(`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"synthetic lifetime input"}],"stream":` + map[bool]string{true: "true", false: "false"}[mode == "http-stream"] + `}`)
	request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
	opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
	if len(metadata) > 0 {
		opts.Metadata = metadata[0]
	}
	switch mode {
	case "execute":
		_, err := e.Execute(ctx, auth, request, opts)
		return err
	case "count":
		_, err := e.CountTokens(ctx, auth, request, opts)
		return err
	case "stream":
		stream, err := e.ExecuteStream(ctx, auth, request, opts)
		if err != nil {
			return err
		}
		if ready != nil {
			close(ready)
		}
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
		return err
	default:
		url := "https://api.anthropic.com/v1/messages?beta=true"
		if mode == "http-count" {
			url = "https://api.anthropic.com/v1/messages/count_tokens?beta=true"
		}
		raw, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		raw.Header = headers.Clone()
		response, err := e.HttpRequest(ctx, auth, raw)
		if err != nil {
			return err
		}
		if ready != nil {
			close(ready)
		}
		_, err = io.ReadAll(response.Body)
		return errors.Join(err, response.Body.Close())
	}
}

type queryLifetimeBlockingBody struct {
	ctx     context.Context
	prefix  *strings.Reader
	blocked chan struct{}
	once    sync.Once
}

func (b *queryLifetimeBlockingBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() { close(b.blocked) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *queryLifetimeBlockingBody) Close() error { return nil }
