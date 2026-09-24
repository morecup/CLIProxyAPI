package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type httpResultTestExecutor struct {
	claudeCancellationTestExecutor
	httpFn func(context.Context) (*http.Response, error)
}

func (e *httpResultTestExecutor) HttpRequest(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	return e.httpFn(ctx)
}

type httpResultErrorReader struct{}

func (httpResultErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type httpProtocolResultReader struct {
	io.Reader
	ctx context.Context
}

func (r *httpProtocolResultReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		cliproxyexecutor.ReportHTTPResult(r.ctx, errors.New("incomplete protocol response"))
	}
	return n, err
}

func TestExecuteHTTPRequestAccountsForCompletedOutcomeOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		requestErr error
		bodyKind   string
		closeEarly bool
		home       bool
		untracked  bool
		wantFailed bool
	}{
		{name: "success", status: 200},
		{name: "http failure", status: 429, wantFailed: true},
		{name: "transport failure", requestErr: errors.New("connection refused"), wantFailed: true},
		{name: "nil response", wantFailed: true},
		{name: "read failure", status: 200, bodyKind: "error", wantFailed: true},
		{name: "protocol failure at EOF", status: 200, bodyKind: "protocol", wantFailed: true},
		{name: "early close", status: 200, closeEarly: true, wantFailed: true},
		{name: "cancellation", requestErr: context.Canceled, wantFailed: true},
		{name: "home success", status: 200, home: true},
		{name: "home failure", status: 503, home: true, wantFailed: true},
		{name: "untracked management call", status: 200, untracked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &resultCaptureHook{}
			manager := NewManager(nil, nil, hook)
			if tc.home {
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			}
			auth, errRegister := manager.Register(t.Context(), &Auth{ID: "raw-auth", Provider: "claude"})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			manager.RegisterExecutor(&httpResultTestExecutor{httpFn: func(ctx context.Context) (*http.Response, error) {
				if tc.requestErr != nil || tc.status == 0 {
					return nil, tc.requestErr
				}
				var body io.Reader = strings.NewReader("raw response")
				switch tc.bodyKind {
				case "error":
					body = httpResultErrorReader{}
				case "protocol":
					body = &httpProtocolResultReader{Reader: body, ctx: ctx}
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(body)}, nil
			}})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test/v1/messages", strings.NewReader("{}"))
			var response *http.Response
			if tc.untracked {
				response, _ = manager.HttpRequest(t.Context(), auth, req)
			} else {
				response, _ = manager.ExecuteHTTPRequest(t.Context(), auth, req, "test-model", cliproxyexecutor.Options{})
			}
			if response != nil {
				if tc.status == 200 && len(hook.Results()) != 0 {
					t.Fatal("success recorded before reading the response")
				}
				if !tc.closeEarly {
					_, _ = io.Copy(io.Discard, response.Body)
				}
				_ = response.Body.Close()
				_ = response.Body.Close()
			}
			results := hook.Results()
			if tc.untracked {
				if len(results) != 0 {
					t.Fatalf("management call recorded %d results", len(results))
				}
			} else if len(results) != 1 || results[0].Success == tc.wantFailed {
				t.Fatalf("results = %#v, want one result with failed=%t", results, tc.wantFailed)
			}
			var wantSuccess, wantFailed int64
			if !tc.home && !tc.untracked {
				if tc.wantFailed {
					wantFailed = 1
				} else {
					wantSuccess = 1
				}
			}
			stored, _ := manager.GetByID(auth.ID)
			if stored.Success != wantSuccess || stored.Failed != wantFailed {
				t.Fatalf("totals = %d/%d, want %d/%d", stored.Success, stored.Failed, wantSuccess, wantFailed)
			}
			var recentSuccess, recentFailed int64
			for _, bucket := range stored.RecentRequestsSnapshot(time.Now()) {
				recentSuccess += bucket.Success
				recentFailed += bucket.Failed
			}
			if recentSuccess != wantSuccess || recentFailed != wantFailed {
				t.Fatalf("recent totals = %d/%d, want %d/%d", recentSuccess, recentFailed, wantSuccess, wantFailed)
			}
			if tc.requestErr == context.Canceled && stored.Unavailable {
				t.Fatal("cancellation must not cool the credential")
			}
		})
	}
}
