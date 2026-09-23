package claudedesktop

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeMagicLinkAttestationPayload(t *testing.T) {
	raw, errMarshal := json.Marshal(browserAttestationPayload{
		HCaptchaToken: "  browser-token  ",
		Locale:        " en-US ",
		UserAgent:     " Chromium test ",
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	attestation, errDecode := decodeMagicLinkAttestationPayload(string(raw))
	if errDecode != nil {
		t.Fatalf("decodeMagicLinkAttestationPayload() error = %v", errDecode)
	}
	if attestation.HCaptchaToken != "browser-token" || attestation.Locale != "en-US" || attestation.UserAgent != "Chromium test" {
		t.Fatalf("unexpected attestation: %#v", attestation)
	}

	tests := []string{
		"",
		"not-json",
		`{"hcaptcha_token":""}`,
		`{"hcaptcha_token":"token","locale":"` + strings.Repeat("x", 65) + `"}`,
		`{"hcaptcha_token":"` + strings.Repeat("x", maxAttestationSize+1) + `"}`,
	}
	for _, test := range tests {
		if _, errDecode = decodeMagicLinkAttestationPayload(test); errDecode == nil {
			t.Fatalf("decodeMagicLinkAttestationPayload(%d bytes) succeeded, want rejection", len(test))
		}
	}
}

func TestMagicLinkAnonymousCookieHookEscapesValue(t *testing.T) {
	value := `anon-";window.injected=true;//`
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	hook := magicLinkAnonymousCookieHook(value)
	if !strings.Contains(hook, "encodeURIComponent("+string(encoded)+")") {
		t.Fatalf("anonymous id was not JSON encoded in hook: %s", hook)
	}
	if strings.Contains(hook, "encodeURIComponent("+value+")") {
		t.Fatal("anonymous id was inserted into hook without encoding")
	}
}

func TestBrowserConnectProxyForwardsHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	dialer := &recordingBrowserDialer{rewriteAddress: strings.TrimPrefix(server.URL, "https://")}
	bridge, errBridge := startBrowserConnectProxy(t.Context(), dialer)
	if errBridge != nil {
		t.Fatalf("startBrowserConnectProxy() error = %v", errBridge)
	}
	defer func() {
		if errClose := bridge.Close(); errClose != nil {
			t.Errorf("close browser proxy bridge: %v", errClose)
		}
	}()
	proxyURL, errParse := url.Parse(bridge.URL())
	if errParse != nil {
		t.Fatal(errParse)
	}
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Test server certificate.
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, errGet := client.Get("https://claude.ai/")
	if errGet != nil {
		t.Fatalf("request through browser proxy bridge: %v", errGet)
	}
	body, errRead := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if errRead != nil {
		t.Fatal(errRead)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("proxy response = HTTP %d %q", response.StatusCode, body)
	}
	if got := dialer.LastAddress(); got != "claude.ai:443" {
		t.Fatalf("proxy dial target = %q, want Claude origin", got)
	}
}

func TestValidateBrowserProxyTarget(t *testing.T) {
	for _, target := range []string{"claude.ai:443", "api.hcaptcha.com:443", "newassets.hcaptcha.com:443", "a.claude.ai:443"} {
		if errValidate := validateBrowserProxyTarget(target); errValidate != nil {
			t.Errorf("validateBrowserProxyTarget(%q) error = %v", target, errValidate)
		}
	}
	for _, target := range []string{"claude.ai:80", "127.0.0.1:443", "evil.example:443", "claude.ai.evil.example:443", "missing-port"} {
		if errValidate := validateBrowserProxyTarget(target); errValidate == nil {
			t.Errorf("validateBrowserProxyTarget(%q) succeeded, want rejection", target)
		}
	}
}

type recordingBrowserDialer struct {
	mu             sync.Mutex
	address        string
	rewriteAddress string
}

func (d *recordingBrowserDialer) Dial(network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.address = address
	rewriteAddress := d.rewriteAddress
	d.mu.Unlock()
	if rewriteAddress != "" {
		address = rewriteAddress
	}
	return net.DialTimeout(network, address, 5*time.Second)
}

func (d *recordingBrowserDialer) LastAddress() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.address
}
