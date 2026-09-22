package helps

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackedReadCloser struct {
	io.Reader
	closeCount int
	closeErr   error
	onClose    func()
}

func (r *trackedReadCloser) Close() error {
	r.closeCount++
	if r.onClose != nil {
		r.onClose()
	}
	return r.closeErr
}

type contextDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f contextDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func (f contextDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

type trackedNetConn struct {
	net.Conn
	closeCount atomic.Int32
}

func (c *trackedNetConn) Close() error {
	c.closeCount.Add(1)
	return c.Conn.Close()
}

func TestCloseConnectionBodyClosesConnectionBeforeBodyOnce(t *testing.T) {
	bodyErr := errors.New("body close failed")
	connectionErr := errors.New("connection close failed")
	var closeOrder []string
	body := &trackedReadCloser{
		Reader:   strings.NewReader("response"),
		closeErr: bodyErr,
		onClose: func() {
			closeOrder = append(closeOrder, "body")
		},
	}
	connectionCloseCount := 0
	wrapped := &closeConnectionBody{
		ReadCloser: body,
		closeConnection: func() error {
			connectionCloseCount++
			closeOrder = append(closeOrder, "connection")
			return connectionErr
		},
	}

	payload, errRead := io.ReadAll(wrapped)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if got, want := string(payload), "response"; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}

	errClose := wrapped.Close()
	if !errors.Is(errClose, bodyErr) {
		t.Fatalf("close error = %v, want body close error", errClose)
	}
	if !errors.Is(errClose, connectionErr) {
		t.Fatalf("close error = %v, want connection close error", errClose)
	}
	if errCloseAgain := wrapped.Close(); errCloseAgain != errClose {
		t.Fatalf("second close error = %v, want %v", errCloseAgain, errClose)
	}
	if body.closeCount != 1 {
		t.Fatalf("body close count = %d, want 1", body.closeCount)
	}
	if connectionCloseCount != 1 {
		t.Fatalf("connection close count = %d, want 1", connectionCloseCount)
	}
	if want := []string{"connection", "body"}; !reflect.DeepEqual(closeOrder, want) {
		t.Fatalf("close order = %v, want %v", closeOrder, want)
	}
}

func TestUtlsRoundTripperDialUsesRequestContext(t *testing.T) {
	dialStarted := make(chan struct{})
	roundTripper := &utlsRoundTripper{dialer: contextDialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})}
	ctx, cancel := context.WithCancel(t.Context())
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	roundTripDone := make(chan error, 1)
	go func() {
		resp, errRoundTrip := roundTripper.RoundTrip(req)
		if resp != nil && resp.Body != nil {
			errRoundTrip = errors.Join(errRoundTrip, resp.Body.Close())
		}
		roundTripDone <- errRoundTrip
	}()

	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancel()
	select {
	case errRoundTrip := <-roundTripDone:
		if !errors.Is(errRoundTrip, context.Canceled) {
			t.Fatalf("RoundTrip error = %v, want context canceled", errRoundTrip)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip did not stop after context cancellation")
	}
}

func TestUtlsRoundTripperHandshakeUsesRequestContext(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if errClose := clientConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) && !errors.Is(errClose, io.ErrClosedPipe) {
			t.Errorf("close client connection: %v", errClose)
		}
		if errClose := serverConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) && !errors.Is(errClose, io.ErrClosedPipe) {
			t.Errorf("close server connection: %v", errClose)
		}
	})

	trackedConn := &trackedNetConn{Conn: clientConn}
	dialDone := make(chan struct{})
	roundTripper := &utlsRoundTripper{dialer: contextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		close(dialDone)
		return trackedConn, nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	connectionDone := make(chan error, 1)
	go func() {
		h2Conn, errConnect := roundTripper.createConnection(ctx, "chatgpt.com", "chatgpt.com:443")
		if h2Conn != nil {
			errConnect = errors.Join(errConnect, h2Conn.Close())
		}
		connectionDone <- errConnect
	}()

	select {
	case <-dialDone:
	case <-time.After(time.Second):
		t.Fatal("dial did not complete")
	}
	cancel()
	select {
	case errConnect := <-connectionDone:
		if !errors.Is(errConnect, context.Canceled) {
			t.Fatalf("createConnection error = %v, want context canceled", errConnect)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake did not stop after context cancellation")
	}
	if got := trackedConn.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestFallbackRoundTripperSelectsProviderTransport(t *testing.T) {
	t.Parallel()

	route := func(label string) http.RoundTripper {
		return utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"X-Test-Route": []string{label}},
				Body:       io.NopCloser(strings.NewReader("{}")),
				Request:    req,
			}, nil
		})
	}
	roundTripper := &fallbackRoundTripper{
		anthropic: route("anthropic"),
		chrome:    route("chrome"),
		fallback:  route("fallback"),
	}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "Anthropic HTTPS", url: "https://api.anthropic.com/v1/messages", want: "anthropic"},
		{name: "Anthropic explicit HTTPS port", url: "https://api.anthropic.com:443/v1/messages", want: "anthropic"},
		{name: "Anthropic custom port", url: "https://api.anthropic.com:8443/v1/messages", want: "fallback"},
		{name: "Anthropic userinfo", url: "https://caller@api.anthropic.com/v1/messages", want: "fallback"},
		{name: "Anthropic lookalike", url: "https://api.anthropic.com.example/v1/messages", want: "fallback"},
		{name: "ChatGPT HTTPS", url: "https://chatgpt.com/backend-api/codex/responses", want: "chrome"},
		{name: "Other HTTPS", url: "https://example.com/v1/messages", want: "fallback"},
		{name: "Anthropic HTTP", url: "http://api.anthropic.com/v1/messages", want: "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, errRequest := http.NewRequest(http.MethodGet, tt.url, nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			resp, errRoundTrip := roundTripper.RoundTrip(req)
			if errRoundTrip != nil {
				t.Fatal(errRoundTrip)
			}
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					t.Errorf("close response body: %v", errClose)
				}
			}()
			if got := resp.Header.Get("X-Test-Route"); got != tt.want {
				t.Fatalf("route = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewUtlsHTTPClientUsesStandardTransportForAnthropic(t *testing.T) {
	t.Parallel()

	client := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	router, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("transport type = %T, want *fallbackRoundTripper", client.Transport)
	}
	if router.anthropic != http.DefaultTransport {
		t.Fatalf("Anthropic transport = %T, want http.DefaultTransport", router.anthropic)
	}
	if router.fallback != http.DefaultTransport {
		t.Fatalf("fallback transport = %T, want http.DefaultTransport", router.fallback)
	}
	if _, okChrome := router.chrome.(*utlsRoundTripper); !okChrome {
		t.Fatalf("ChatGPT transport = %T, want *utlsRoundTripper", router.chrome)
	}
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Parallel()

	for _, targetURL := range []string{
		"https://api.anthropic.com/v1/messages",
		"https://chatgpt.com/backend-api/codex/responses",
	} {
		t.Run(targetURL, func(t *testing.T) {
			called := false
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}))

			client := NewUtlsHTTPClient(ctx, nil, nil, 0)
			resp, err := client.Get(targetURL)
			if err != nil {
				t.Fatalf("client.Get returned error: %v", err)
			}
			if errClose := resp.Body.Close(); errClose != nil {
				t.Fatalf("response body close returned error: %v", errClose)
			}
			if !called {
				t.Fatal("expected context RoundTripper to handle protected host request")
			}
		})
	}
}
