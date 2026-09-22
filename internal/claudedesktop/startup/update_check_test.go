package startup

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const noUpdateJSON = `{"currentRelease":"1.40609.0","releases":[]}`

func TestUpdateCheckOrdersRequestsAndObservesOnlyCompleteOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, body, failureRole string
		status                  int
		wantOutcome             bool
		observerFailure         bool
		readError, closeError   bool
	}{
		{name: "no-update", body: noUpdateJSON, wantOutcome: true},
		{name: "invalid-json", body: `{"releases":`},
		{name: "empty-body"},
		{name: "complete-json-with-read-error", body: noUpdateJSON, readError: true},
		{name: "complete-json-with-close-error", body: noUpdateJSON, closeError: true},
		{name: "missing-releases", body: `{"currentRelease":"1.40609.0"}`},
		{name: "null-releases", body: `{"currentRelease":"1.40609.0","releases":null}`},
		{name: "available-update", body: `{"currentRelease":"1.40609.0","releases":[{"version":"1.40610.0"}]}`},
		{name: "different-current", body: `{"currentRelease":"1.40610.0","releases":[]}`},
		{name: "head-http-error", failureRole: "startup-update-head", status: 502},
		{name: "get-http-error", failureRole: "startup-update-get", status: 500},
		{name: "partial-content", body: noUpdateJSON, status: 206},
		{name: "head-network-error", failureRole: "startup-update-head"},
		{name: "get-network-error", failureRole: "startup-update-get"},
		{name: "telemetry-failure", body: noUpdateJSON, wantOutcome: true, observerFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := claudeprofile.Load("")
			if err != nil {
				t.Fatal(err)
			}
			var endpoints []claudeprofile.StartupEndpointProfile
			for _, ep := range bundle.Startup.Endpoints {
				if ep.EndpointRole == "startup-update-head" || ep.EndpointRole == "startup-update-get" {
					endpoints = append(endpoints, ep)
				}
			}
			// The evidence-backed dependency must not rely on profile array order.
			bundle.Startup.Endpoints = []claudeprofile.StartupEndpointProfile{endpoints[1], endpoints[0]}
			var mu sync.Mutex
			var order []string
			appendStep := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }
			manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle,
				UpdateCheckObserver: func(_ *cliproxyauth.Auth, fact string) error {
					appendStep(fact)
					if tc.observerFailure {
						return errors.New("PRIVATE_TELEMETRY_ERROR")
					}
					return nil
				},
				DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
					return startupDoerFunc(func(request *http.Request) (*http.Response, error) {
						appendStep(request.Method)
						if role == tc.failureRole {
							if tc.status == 0 {
								return nil, errors.New("PRIVATE_NETWORK_ERROR")
							}
							return startupResponse(tc.status, ""), nil
						}
						response := startupResponse(http.StatusOK, "")
						if request.Method == http.MethodGet {
							response.Body = &updateTestBody{Reader: bytes.NewReader([]byte(tc.body)), readError: tc.readError, closeError: tc.closeError}
							if tc.status != 0 {
								response.StatusCode = tc.status
							}
						}
						return response, nil
					}), nil
				}})
			t.Cleanup(manager.Close)
			auth := startupTestAuth(t, true)
			manager.Activate(auth)
			manager.Activate(auth)
			status := waitForStartupStatus(t, manager, func(s Status) bool { return s.State == "ready" || s.State == "degraded" })
			want := []string{claudetelemetry.FactUpdateCheckStarted, "HEAD"}
			if tc.failureRole != "startup-update-head" {
				want = append(want, "GET")
			}
			if tc.wantOutcome {
				want = append(want, claudetelemetry.FactUpdateNotAvailable)
			}
			mu.Lock()
			got := append([]string(nil), order...)
			mu.Unlock()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("order=%v want %v", got, want)
			}
			if tc.wantOutcome && !tc.observerFailure {
				if status.State != "ready" || status.Completed != 2 {
					t.Fatalf("status=%+v", status)
				}
			} else if status.State != "degraded" {
				t.Fatalf("status=%+v", status)
			}
			if tc.observerFailure && (status.TelemetryErrorCategory == "" || status.Failed != 0 || status.Completed != 2) {
				t.Fatalf("telemetry failure disrupted HTTP accounting: %+v", status)
			}
		})
	}
}

func TestReadUpdateResultCompressionAndBounds(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "deflate", "br", "zstd", "gzip, br"} {
		t.Run(encoding, func(t *testing.T) {
			encoded := []byte(noUpdateJSON)
			for _, layer := range strings.Split(encoding, ", ") {
				var buffer bytes.Buffer
				var writer io.WriteCloser
				switch layer {
				case "identity":
					continue
				case "gzip":
					writer = gzip.NewWriter(&buffer)
				case "deflate":
					writer = zlib.NewWriter(&buffer)
				case "br":
					writer = brotli.NewWriter(&buffer)
				case "zstd":
					var err error
					writer, err = zstd.NewWriter(&buffer)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := writer.Write(encoded); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				encoded = buffer.Bytes()
			}
			response := startupResponse(200, "")
			response.Header.Set("Content-Encoding", encoding)
			response.Body = io.NopCloser(bytes.NewReader(encoded))
			ok, err := readUpdateResult(response, 4096, "1.40609.0")
			if err != nil || !ok {
				t.Fatalf("ok=%v error=%v", ok, err)
			}
		})
	}
	for _, tc := range []struct {
		name, body, encoding  string
		limit                 int64
		readError, closeError bool
	}{
		{name: "encoded-too-large", body: noUpdateJSON, limit: 8},
		{name: "decoded-too-large", body: noUpdateJSON + strings.Repeat(" ", 8192), encoding: "gzip", limit: 1024},
		{name: "unsupported-codec", body: noUpdateJSON, encoding: "unknown", limit: 4096},
		{name: "truncated-body", body: noUpdateJSON, limit: 4096, readError: true},
		{name: "close-failure", body: noUpdateJSON, limit: 4096, closeError: true},
		{name: "invalid-trailing-json", body: noUpdateJSON + "{}", limit: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := []byte(tc.body)
			if tc.encoding == "gzip" {
				var buf bytes.Buffer
				writer := gzip.NewWriter(&buf)
				_, _ = writer.Write(encoded)
				_ = writer.Close()
				encoded = buf.Bytes()
			}
			response := startupResponse(200, "")
			response.Header.Set("Content-Encoding", tc.encoding)
			body := &updateTestBody{Reader: bytes.NewReader(encoded), readError: tc.readError, closeError: tc.closeError}
			response.Body = body
			ok, err := readUpdateResult(response, tc.limit, "1.40609.0")
			if ok || err == nil || !body.closed {
				t.Fatalf("ok=%v error=%v closed=%v", ok, err, body.closed)
			}
		})
	}
}

type updateTestBody struct {
	*bytes.Reader
	readError, closeError, closed bool
}

func (b *updateTestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.readError {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (b *updateTestBody) Close() error {
	b.closed = true
	if b.closeError {
		return errors.New("synthetic close failure")
	}
	return nil
}
