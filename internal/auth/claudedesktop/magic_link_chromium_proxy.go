package claudedesktop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var browserProxyAllowedHosts = []string{
	"anthropic.com",
	"claude.ai",
	"cloudflare.com",
	"cloudflareinsights.com",
	"hcaptcha.com",
}

type browserProxyDialer interface {
	Dial(network, address string) (net.Conn, error)
}

type browserProxyTunnel struct {
	client   net.Conn
	upstream net.Conn
}

type browserConnectProxy struct {
	listener net.Listener
	server   *http.Server
	dialer   browserProxyDialer
	done     chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	tunnels   map[*browserProxyTunnel]struct{}
	lastError error
}

func startBrowserConnectProxy(ctx context.Context, dialer browserProxyDialer) (*browserConnectProxy, error) {
	if dialer == nil {
		return nil, fmt.Errorf("Claude Desktop Chromium proxy dialer is unavailable")
	}
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return nil, fmt.Errorf("start Claude Desktop Chromium proxy bridge: %w", errListen)
	}
	proxy := &browserConnectProxy{
		listener: listener,
		dialer:   dialer,
		done:     make(chan struct{}),
		tunnels:  make(map[*browserProxyTunnel]struct{}),
	}
	proxy.server = &http.Server{
		Handler:           http.HandlerFunc(proxy.handle),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		_ = proxy.server.Serve(listener)
		close(proxy.done)
	}()
	if ctx != nil {
		context.AfterFunc(ctx, func() {
			_ = proxy.Close()
		})
	}
	return proxy, nil
}

func (p *browserConnectProxy) URL() string {
	if p == nil || p.listener == nil {
		return ""
	}
	return "http://" + p.listener.Addr().String()
}

func (p *browserConnectProxy) LastError() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}

func (p *browserConnectProxy) Close() error {
	if p == nil {
		return nil
	}
	var errClose error
	p.closeOnce.Do(func() {
		if p.server != nil {
			errClose = p.server.Close()
			if errors.Is(errClose, http.ErrServerClosed) {
				errClose = nil
			}
		}
		p.mu.Lock()
		for tunnel := range p.tunnels {
			_ = tunnel.client.Close()
			_ = tunnel.upstream.Close()
		}
		p.mu.Unlock()
		select {
		case <-p.done:
		case <-time.After(time.Second):
		}
	})
	return errClose
}

func (p *browserConnectProxy) handle(w http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodConnect || request.Host == "" {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	if errTarget := validateBrowserProxyTarget(request.Host); errTarget != nil {
		http.Error(w, "proxy target rejected", http.StatusForbidden)
		return
	}
	upstream, errDial := dialBrowserProxy(request.Context(), p.dialer, "tcp", request.Host)
	if errDial != nil {
		p.recordError(fmt.Errorf("dial Chromium target through configured proxy: %w", errDial))
		http.Error(w, "proxy connection failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		p.recordError(fmt.Errorf("Chromium proxy response does not support connection hijacking"))
		http.Error(w, "proxy connection failed", http.StatusInternalServerError)
		return
	}
	client, buffered, errHijack := hijacker.Hijack()
	if errHijack != nil {
		_ = upstream.Close()
		p.recordError(fmt.Errorf("accept Chromium proxy tunnel: %w", errHijack))
		return
	}
	clientForRelay := net.Conn(client)
	if buffered.Reader.Buffered() > 0 {
		clientForRelay = &bufferedBrowserConn{Conn: client, reader: buffered.Reader}
	}
	if _, errWrite := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); errWrite != nil {
		_ = client.Close()
		_ = upstream.Close()
		p.recordError(fmt.Errorf("confirm Chromium proxy tunnel: %w", errWrite))
		return
	}
	if errFlush := buffered.Flush(); errFlush != nil {
		_ = client.Close()
		_ = upstream.Close()
		p.recordError(fmt.Errorf("flush Chromium proxy tunnel: %w", errFlush))
		return
	}

	tunnel := &browserProxyTunnel{client: clientForRelay, upstream: upstream}
	p.mu.Lock()
	p.tunnels[tunnel] = struct{}{}
	p.mu.Unlock()
	defer func() {
		_ = clientForRelay.Close()
		_ = upstream.Close()
		p.mu.Lock()
		delete(p.tunnels, tunnel)
		p.mu.Unlock()
	}()

	done := make(chan struct{}, 2)
	go relayBrowserProxy(upstream, clientForRelay, done)
	go relayBrowserProxy(clientForRelay, upstream, done)
	<-done
	_ = clientForRelay.Close()
	_ = upstream.Close()
	<-done
}

func validateBrowserProxyTarget(address string) error {
	host, port, errSplit := net.SplitHostPort(strings.TrimSpace(address))
	if errSplit != nil || port != "443" {
		return fmt.Errorf("Chromium proxy target is not permitted")
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("Chromium proxy target is not permitted")
	}
	for _, allowed := range browserProxyAllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("Chromium proxy target is not permitted")
}

func (p *browserConnectProxy) recordError(err error) {
	if p == nil || err == nil {
		return
	}
	p.mu.Lock()
	p.lastError = err
	p.mu.Unlock()
}

func dialBrowserProxy(ctx context.Context, dialer browserProxyDialer, network, address string) (net.Conn, error) {
	if contextDialer, ok := dialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}); ok {
		return contextDialer.DialContext(ctx, network, address)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		conn, errDial := dialer.Dial(network, address)
		select {
		case resultCh <- result{conn: conn, err: errDial}:
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outcome := <-resultCh:
		return outcome.conn, outcome.err
	}
}

func relayBrowserProxy(destination, source net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(destination, source)
	if closeWriter, ok := destination.(interface{ CloseWrite() error }); ok {
		_ = closeWriter.CloseWrite()
	}
	done <- struct{}{}
}

type bufferedBrowserConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedBrowserConn) Read(buffer []byte) (int, error) {
	if c.reader.Buffered() > 0 {
		return c.reader.Read(buffer)
	}
	return c.Conn.Read(buffer)
}
