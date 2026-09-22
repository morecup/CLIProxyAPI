package helps

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestClaudeDesktopTransportCaptureProbe is an opt-in network probe used by
// the protected recorder to compare CLIProxyAPI's client-side TLS and HTTP/2
// wire shape with the official Desktop. It uses only synthetic credentials and
// payloads; the recorder scenario label remains the sole correlation key.
func TestClaudeDesktopTransportCaptureProbe(t *testing.T) {
	if os.Getenv("CLIPROXY_CLAUDE_DESKTOP_TRANSPORT_CAPTURE") != "1" {
		t.Skip("set CLIPROXY_CLAUDE_DESKTOP_TRANSPORT_CAPTURE=1 to run the live transport probe")
	}
	proxyURL := strings.TrimSpace(os.Getenv("CLIPROXY_CLAUDE_DESKTOP_TRANSPORT_PROXY"))
	if proxyURL != "http://127.0.0.1:18081" {
		t.Fatalf("capture proxy = %q, want protected recorder proxy", proxyURL)
	}
	bundle, errBundle := claudeprofile.Load("")
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	registry := NewClaudeDesktopTransportRegistry()
	t.Cleanup(registry.CloseAll)
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxyURL}}
	auth := &cliproxyauth.Auth{ID: "claude-desktop-transport-candidate"}

	for _, role := range []claudeprofile.RequestRole{
		claudeprofile.RoleMain,
		claudeprofile.RoleTitle,
		claudeprofile.RoleLightHelper,
		claudeprofile.RoleWebSearchHelper,
		claudeprofile.RoleCompaction,
		claudeprofile.RoleSubagent,
		claudeprofile.RoleSecurityMonitor,
		claudeprofile.RoleCountTokens,
	} {
		role := role
		t.Run("request-role-"+string(role), func(t *testing.T) {
			client, errClient := registry.Client(context.Background(), cfg, auth, bundle, role)
			if errClient != nil {
				t.Fatal(errClient)
			}
			profile, errProfile := bundle.TransportForRole(role)
			if errProfile != nil {
				t.Fatal(errProfile)
			}
			path := "/v1/messages"
			if role == claudeprofile.RoleCountTokens {
				path = "/v1/messages/count_tokens"
			}
			probeRoundTrip(t, client, profile, http.MethodPost, "https://api.anthropic.com"+path, []byte(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":"transport probe"}]}`), nil)
		})
	}

	endpoints := []struct {
		role    string
		method  string
		url     string
		body    []byte
		headers map[string]string
	}{
		{bundle.Telemetry.EndpointRole, http.MethodPost, bundle.Telemetry.Endpoint, []byte(`{"events":[]}`), nil},
		{bundle.SDKTelemetry.EndpointRole, http.MethodPost, bundle.SDKTelemetry.Endpoint, []byte(`{"events":[]}`), map[string]string{"connection": "close"}},
		{bundle.AuxiliaryTelemetry.Segment.EndpointRole, http.MethodPost, bundle.AuxiliaryTelemetry.Segment.Endpoint, []byte(`{"batch":[],"writeKey":"transport-probe"}`), nil},
		{bundle.AuxiliaryTelemetry.DatadogLogs.EndpointRole, http.MethodPost, bundle.AuxiliaryTelemetry.DatadogLogs.Endpoint, []byte(`[]`), map[string]string{"connection": "close"}},
		{bundle.AuxiliaryTelemetry.DatadogRUM.EndpointRole, http.MethodPost, bundle.AuxiliaryTelemetry.DatadogRUM.Endpoint + "?_dd.api=transport-probe&batch_time=1&dd-api-key=transport-probe&dd-evp-origin=electron&dd-evp-origin-version=1.40609.0&dd-request-id=transport-probe&ddsource=browser", []byte("{}\n"), nil},
		{bundle.AuxiliaryTelemetry.Sentry.EndpointRole, http.MethodPost, bundle.AuxiliaryTelemetry.Sentry.Endpoint + "?sentry_client=sentry.javascript.electron%2F7.12.0&sentry_key=transport-probe&sentry_version=7", []byte("{}\n{\"type\":\"session\"}\n{}\n"), nil},
		{"atis-bootstrap", http.MethodGet, "https://api.anthropic.com/api/claude_cli/bootstrap?entrypoint=claude-desktop&model=claude-sonnet-5", nil, map[string]string{"connection": "close"}},
		{"control-account-json", http.MethodPost, "https://api.anthropic.com/v1/code/sessions", []byte(`{}`), map[string]string{"connection": "close"}},
		{"control-worker-stream", http.MethodGet, "https://api.anthropic.com/v1/code/sessions/cse_transport_probe/worker/events/stream", nil, map[string]string{"connection": "keep-alive"}},
		{"control-worker-read", http.MethodGet, "https://api.anthropic.com/v1/code/sessions/cse_transport_probe/worker", nil, map[string]string{"connection": "keep-alive"}},
		{"control-worker-json", http.MethodPut, "https://api.anthropic.com/v1/code/sessions/cse_transport_probe/worker", []byte(`{}`), map[string]string{"connection": "keep-alive"}},
		{"control-session-read", http.MethodGet, "https://api.anthropic.com/v1/sessions/session_transport_probe", nil, map[string]string{"connection": "close"}},
		{"control-session-write", http.MethodPatch, "https://api.anthropic.com/v1/sessions/session_transport_probe", []byte(`{}`), map[string]string{"connection": "close"}},
	}
	for _, endpoint := range endpoints {
		endpoint := endpoint
		t.Run("endpoint-role-"+endpoint.role, func(t *testing.T) {
			client, errClient := registry.EndpointClient(context.Background(), cfg, auth, bundle, endpoint.role)
			if errClient != nil {
				t.Fatal(errClient)
			}
			profile, errProfile := bundle.TransportForEndpointRole(endpoint.role)
			if errProfile != nil {
				t.Fatal(errProfile)
			}
			probeRoundTrip(t, client, profile, endpoint.method, endpoint.url, endpoint.body, endpoint.headers)
		})
	}

	t.Run("renderer-concurrent-streams", func(t *testing.T) {
		client, errClient := registry.EndpointClient(context.Background(), cfg, auth, bundle, bundle.Telemetry.EndpointRole)
		if errClient != nil {
			t.Fatal(errClient)
		}
		profile, errProfile := bundle.TransportForEndpointRole(bundle.Telemetry.EndpointRole)
		if errProfile != nil {
			t.Fatal(errProfile)
		}
		var wait sync.WaitGroup
		errors := make(chan error, 2)
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				errors <- probeRoundTripError(client, profile, http.MethodPost, bundle.Telemetry.Endpoint, []byte(`{"events":[]}`), nil)
			}()
		}
		wait.Wait()
		close(errors)
		for errProbe := range errors {
			if errProbe != nil {
				t.Fatal(errProbe)
			}
		}
	})
}

func probeRoundTrip(t *testing.T, client *http.Client, profile claudeprofile.TransportProfile, method, rawURL string, body []byte, overrides map[string]string) {
	t.Helper()
	if errProbe := probeRoundTripError(client, profile, method, rawURL, body, overrides); errProbe != nil {
		t.Fatal(errProbe)
	}
}

func probeRoundTripError(client *http.Client, profile claudeprofile.TransportProfile, method, rawURL string, body []byte, overrides map[string]string) error {
	request, errRequest := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if errRequest != nil {
		return errRequest
	}
	optional := make(map[string]struct{}, len(profile.OptionalHeaders))
	for _, rawName := range profile.OptionalHeaders {
		optional[strings.ToLower(strings.TrimSpace(rawName))] = struct{}{}
	}
	for _, rawName := range profile.HeaderOrder {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if name == "" || strings.HasPrefix(name, ":") || name == "host" || name == "content-length" {
			continue
		}
		if value, ok := overrides[name]; ok {
			request.Header.Set(rawName, value)
			continue
		}
		if _, ok := optional[name]; ok {
			continue
		}
		request.Header.Set(rawName, claudeDesktopTransportProbeHeader(name))
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return errDo
	}
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		if errClose := response.Body.Close(); errClose != nil {
			return errClose
		}
	}
	return nil
}

func claudeDesktopTransportProbeHeader(name string) string {
	switch name {
	case "accept":
		return "*/*"
	case "accept-encoding":
		return "gzip, deflate, br, zstd"
	case "accept-language":
		return "en-US"
	case "authorization":
		return "Bearer transport-probe"
	case "baggage":
		return "sentry-environment=production,sentry-release=Claude%401.40609.0,sentry-public_key=transport-probe,sentry-trace_id=00000000000000000000000000000000,sentry-org_id=1158394"
	case "connection":
		return "keep-alive"
	case "content-encoding":
		return "gzip"
	case "content-type":
		return "application/json"
	case "dd-api-key":
		return "transport-probe"
	case "origin":
		return "https://claude.ai"
	case "priority":
		return "u=4, i"
	case "referer":
		return "https://claude.ai/"
	case "sec-ch-ua":
		return `"Not/A)Brand";v="99", "Chromium";v="148"`
	case "sec-ch-ua-mobile":
		return "?0"
	case "sec-ch-ua-platform":
		return `"Windows"`
	case "sec-fetch-dest":
		return "empty"
	case "sec-fetch-mode":
		return "no-cors"
	case "sec-fetch-site":
		return "none"
	case "sec-fetch-storage-access":
		return "active"
	case "sentry-trace":
		return "00000000000000000000000000000000-0000000000000000"
	case "user-agent":
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.40609.0 Chrome/148.0.7778.280 Electron/42.10.0 Safari/537.36 MSIX"
	case "x-claude-code-session-id", "x-client-request-id":
		return "00000000-0000-4000-8000-000000000000"
	case "x-service-name":
		return "claude_desktop"
	default:
		return "transport-probe"
	}
}
