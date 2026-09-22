package helps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestClaudeDesktopClientHelloSpecMatchesCapturedStructure(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	profile, errProfile := bundle.TransportForRole(claudeprofile.RoleMain)
	if errProfile != nil {
		t.Fatalf("resolve transport profile: %v", errProfile)
	}
	spec, errSpec := claudeDesktopClientHelloSpec(profile, "api.anthropic.com")
	if errSpec != nil {
		t.Fatalf("build ClientHello spec: %v", errSpec)
	}
	if len(spec.CipherSuites) != len(profile.CipherSuites) {
		t.Fatalf("cipher suite count = %d, want %d", len(spec.CipherSuites), len(profile.CipherSuites))
	}
	if len(spec.Extensions) != len(profile.ExtensionOrder) {
		t.Fatalf("extension count = %d, want %d", len(spec.Extensions), len(profile.ExtensionOrder))
	}
	if spec.TLSVersMin != 771 || spec.TLSVersMax != 772 {
		t.Fatalf("TLS versions = %d..%d, want 771..772", spec.TLSVersMin, spec.TLSVersMax)
	}
	clientSide, serverSide := net.Pipe()
	defer func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	}()
	connection := tls.UClient(clientSide, &tls.Config{ServerName: "api.anthropic.com", OmitEmptyPsk: true}, tls.HelloCustom)
	if errPreset := connection.ApplyPreset(spec); errPreset != nil {
		t.Fatalf("apply ClientHello spec: %v", errPreset)
	}
	if errBuild := connection.BuildHandshakeState(); errBuild != nil {
		t.Fatalf("build ClientHello: %v", errBuild)
	}
	parsed, errParse := parseClaudeDesktopClientHello(connection.HandshakeState.Hello.Raw)
	if errParse != nil {
		t.Fatalf("parse ClientHello: %v", errParse)
	}
	if !slices.Equal(parsed.cipherSuites, profile.CipherSuites) {
		t.Fatalf("wire cipher suites = %v, want %v", parsed.cipherSuites, profile.CipherSuites)
	}
	if !slices.Equal(parsed.extensions, profile.ExtensionOrder) {
		t.Fatalf("wire extensions = %v, want %v", parsed.extensions, profile.ExtensionOrder)
	}
	if !slices.Equal(parsed.supportedGroups, profile.SupportedGroups) {
		t.Fatalf("wire supported groups = %v, want %v", parsed.supportedGroups, profile.SupportedGroups)
	}
	if !slices.Equal(parsed.pointFormats, profile.ECPointFormats) {
		t.Fatalf("wire point formats = %v, want %v", parsed.pointFormats, profile.ECPointFormats)
	}
	if got := parsed.ja3Hash(); got != profile.JA3Hash {
		t.Fatalf("wire JA3 = %s, want %s", got, profile.JA3Hash)
	}
}

func TestClaudeDesktopRendererClientHelloMatchesCapturedChromium148Structure(t *testing.T) {
	spec, errSpec := claudeDesktopRendererClientHelloSpec("claude.ai")
	if errSpec != nil {
		t.Fatal(errSpec)
	}
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	connection := tls.UClient(clientSide, &tls.Config{ServerName: "claude.ai", OmitEmptyPsk: true}, tls.HelloCustom)
	if errPreset := connection.ApplyPreset(spec); errPreset != nil {
		t.Fatal(errPreset)
	}
	if errBuild := connection.BuildHandshakeState(); errBuild != nil {
		t.Fatal(errBuild)
	}
	parsed, errParse := parseClaudeDesktopClientHello(connection.HandshakeState.Hello.Raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	normalizedCiphers := normalizeClaudeDesktopGREASE16(parsed.cipherSuites)
	wantCiphers := []uint16{
		tls.GREASE_PLACEHOLDER, 4865, 4866, 4867, 49195, 49199, 49196, 49200,
		52393, 52392, 49171, 49172, 156, 157, 47, 53,
	}
	if !slices.Equal(normalizedCiphers, wantCiphers) {
		t.Fatalf("renderer cipher suites = %v, want %v", normalizedCiphers, wantCiphers)
	}
	normalizedExtensions := normalizeClaudeDesktopGREASE16(parsed.extensions)
	slices.Sort(normalizedExtensions)
	wantExtensions := []uint16{
		0, 5, 10, 11, 13, 16, 18, 23, 27, 35, 43, 45, 51,
		tls.GREASE_PLACEHOLDER, tls.GREASE_PLACEHOLDER, 17613, 65037, 65281,
	}
	slices.Sort(wantExtensions)
	if !slices.Equal(normalizedExtensions, wantExtensions) {
		t.Fatalf("renderer extension set = %v, want %v", normalizedExtensions, wantExtensions)
	}
	normalizedGroups := normalizeClaudeDesktopGREASE16(parsed.supportedGroups)
	wantGroups := []uint16{tls.GREASE_PLACEHOLDER, 4588, 29, 23, 24}
	if !slices.Equal(normalizedGroups, wantGroups) {
		t.Fatalf("renderer supported groups = %v, want %v", normalizedGroups, wantGroups)
	}
}

func normalizeClaudeDesktopGREASE16(values []uint16) []uint16 {
	normalized := slices.Clone(values)
	for index, value := range normalized {
		if value>>8 == value&0xff && value&0x0f0f == 0x0a0a {
			normalized[index] = tls.GREASE_PLACEHOLDER
		}
	}
	return normalized
}

type claudeDesktopParsedClientHello struct {
	legacyVersion   uint16
	cipherSuites    []uint16
	extensions      []uint16
	supportedGroups []uint16
	pointFormats    []uint8
}

func parseClaudeDesktopClientHello(raw []byte) (claudeDesktopParsedClientHello, error) {
	if len(raw) < 4+2+32+1 || raw[0] != 1 {
		return claudeDesktopParsedClientHello{}, fmt.Errorf("invalid ClientHello handshake")
	}
	declaredLength := int(raw[1])<<16 | int(raw[2])<<8 | int(raw[3])
	if declaredLength != len(raw)-4 {
		return claudeDesktopParsedClientHello{}, fmt.Errorf("ClientHello length = %d, want %d", declaredLength, len(raw)-4)
	}
	parsed := claudeDesktopParsedClientHello{legacyVersion: binary.BigEndian.Uint16(raw[4:6])}
	position := 4 + 2 + 32
	readVector := func(lengthBytes int) ([]byte, error) {
		if position+lengthBytes > len(raw) {
			return nil, fmt.Errorf("truncated vector length at byte %d", position)
		}
		length := 0
		for index := 0; index < lengthBytes; index++ {
			length = length<<8 | int(raw[position+index])
		}
		position += lengthBytes
		if position+length > len(raw) {
			return nil, fmt.Errorf("truncated vector body at byte %d", position)
		}
		value := raw[position : position+length]
		position += length
		return value, nil
	}
	if _, errSession := readVector(1); errSession != nil {
		return claudeDesktopParsedClientHello{}, errSession
	}
	ciphers, errCiphers := readVector(2)
	if errCiphers != nil || len(ciphers)%2 != 0 {
		return claudeDesktopParsedClientHello{}, fmt.Errorf("invalid cipher suite vector")
	}
	for index := 0; index < len(ciphers); index += 2 {
		parsed.cipherSuites = append(parsed.cipherSuites, binary.BigEndian.Uint16(ciphers[index:index+2]))
	}
	if _, errCompression := readVector(1); errCompression != nil {
		return claudeDesktopParsedClientHello{}, errCompression
	}
	extensions, errExtensions := readVector(2)
	if errExtensions != nil {
		return claudeDesktopParsedClientHello{}, errExtensions
	}
	for offset := 0; offset < len(extensions); {
		if offset+4 > len(extensions) {
			return claudeDesktopParsedClientHello{}, fmt.Errorf("truncated extension header")
		}
		extensionID := binary.BigEndian.Uint16(extensions[offset : offset+2])
		extensionLength := int(binary.BigEndian.Uint16(extensions[offset+2 : offset+4]))
		offset += 4
		if offset+extensionLength > len(extensions) {
			return claudeDesktopParsedClientHello{}, fmt.Errorf("truncated extension %d", extensionID)
		}
		value := extensions[offset : offset+extensionLength]
		offset += extensionLength
		parsed.extensions = append(parsed.extensions, extensionID)
		switch extensionID {
		case 10:
			if len(value) < 2 || int(binary.BigEndian.Uint16(value[:2])) != len(value)-2 || (len(value)-2)%2 != 0 {
				return claudeDesktopParsedClientHello{}, fmt.Errorf("invalid supported_groups extension")
			}
			for groupOffset := 2; groupOffset < len(value); groupOffset += 2 {
				parsed.supportedGroups = append(parsed.supportedGroups, binary.BigEndian.Uint16(value[groupOffset:groupOffset+2]))
			}
		case 11:
			if len(value) < 1 || int(value[0]) != len(value)-1 {
				return claudeDesktopParsedClientHello{}, fmt.Errorf("invalid ec_point_formats extension")
			}
			parsed.pointFormats = append(parsed.pointFormats, value[1:]...)
		}
	}
	if position != len(raw) {
		return claudeDesktopParsedClientHello{}, fmt.Errorf("unexpected trailing ClientHello bytes")
	}
	return parsed, nil
}

func (hello claudeDesktopParsedClientHello) ja3Hash() string {
	joinUint16 := func(values []uint16) string {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			if value&0x0f0f == 0x0a0a && byte(value) == byte(value>>8) {
				continue
			}
			parts = append(parts, strconv.Itoa(int(value)))
		}
		return strings.Join(parts, "-")
	}
	pointParts := make([]string, 0, len(hello.pointFormats))
	for _, value := range hello.pointFormats {
		pointParts = append(pointParts, strconv.Itoa(int(value)))
	}
	ja3 := strings.Join([]string{
		strconv.Itoa(int(hello.legacyVersion)),
		joinUint16(hello.cipherSuites),
		joinUint16(hello.extensions),
		joinUint16(hello.supportedGroups),
		strings.Join(pointParts, "-"),
	}, ",")
	digest := md5.Sum([]byte(ja3))
	return hex.EncodeToString(digest[:])
}

func TestWriteClaudeDesktopRequestUsesCapturedHeaderOrder(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	profile, errProfile := bundle.TransportForRole(claudeprofile.RoleMain)
	if errProfile != nil {
		t.Fatalf("resolve transport profile: %v", errProfile)
	}
	req, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader("{}"))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	for _, pair := range [][2]string{
		{"Accept", "application/json"},
		{"Authorization", "Bearer test"},
		{"Content-Type", "application/json"},
		{"User-Agent", "desktop"},
		{"X-Claude-Code-Session-Id", "session"},
		{"X-Stainless-Arch", "x64"},
		{"X-Stainless-Lang", "js"},
		{"X-Stainless-OS", "Windows"},
		{"X-Stainless-Package-Version", "0.112.1"},
		{"X-Stainless-Retry-Count", "0"},
		{"X-Stainless-Runtime", "node"},
		{"X-Stainless-Runtime-Version", "v26.3.0"},
		{"X-Stainless-Timeout", "900"},
		{"anthropic-beta", "beta"},
		{"anthropic-client-platform", "desktop_app"},
		{"anthropic-client-version", "2.2553.1"},
		{"anthropic-dangerous-direct-browser-access", "true"},
		{"anthropic-version", "2023-06-01"},
		{"x-app", "cli"},
		{"x-cc-atis", "0123456789abcdef"},
		{"x-claude-code-request-class", "main"},
		{"x-client-request-id", "request"},
		{"Connection", "keep-alive"},
		{"Accept-Encoding", "gzip, deflate, br, zstd"},
	} {
		req.Header[pair[0]] = []string{pair[1]}
	}
	var raw bytes.Buffer
	writer := bufio.NewWriter(&raw)
	if errWrite := writeClaudeDesktopRequest(writer, req, []byte("{}"), profile); errWrite != nil {
		t.Fatalf("write request: %v", errWrite)
	}
	lines := strings.Split(strings.SplitN(raw.String(), "\r\n\r\n", 2)[0], "\r\n")
	if len(lines) != len(profile.HeaderOrder)+1 {
		t.Fatalf("line count = %d, want %d\n%s", len(lines), len(profile.HeaderOrder)+1, raw.String())
	}
	for index, header := range profile.HeaderOrder {
		if !strings.HasPrefix(lines[index+1], header+": ") {
			t.Fatalf("line %d = %q, want header %q", index+1, lines[index+1], header)
		}
	}
}

func TestWriteClaudeDesktopCountTokensOmitsTimeoutInPlace(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	profile, errProfile := bundle.TransportForRole(claudeprofile.RoleCountTokens)
	if errProfile != nil {
		t.Fatalf("resolve transport profile: %v", errProfile)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens?beta=true", strings.NewReader("{}"))
	for _, name := range profile.HeaderOrder {
		if name == "X-Stainless-Timeout" || name == "Host" || name == "Content-Length" {
			continue
		}
		req.Header[name] = []string{"value"}
	}
	var raw bytes.Buffer
	if errWrite := writeClaudeDesktopRequest(bufio.NewWriter(&raw), req, []byte("{}"), profile); errWrite != nil {
		t.Fatalf("write request: %v", errWrite)
	}
	if strings.Contains(raw.String(), "X-Stainless-Timeout:") {
		t.Fatal("count_tokens request emitted a timeout header")
	}
	if !strings.Contains(raw.String(), "x-client-request-id: value\r\nConnection: value\r\nHost: api.anthropic.com\r\nAccept-Encoding: value\r\nContent-Length: 2\r\n") {
		t.Fatalf("tail header order differs:\n%s", raw.String())
	}
}

func TestWriteClaudeDesktopMessagesOmitsOptionalATISInPlace(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	profile, errProfile := bundle.TransportForRole(claudeprofile.RoleMain)
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader("{}"))
	for _, name := range profile.HeaderOrder {
		if name == "x-cc-atis" || name == "Host" || name == "Content-Length" {
			continue
		}
		request.Header[name] = []string{"value"}
	}
	var raw bytes.Buffer
	if errWrite := writeClaudeDesktopRequest(bufio.NewWriter(&raw), request, []byte("{}"), profile); errWrite != nil {
		t.Fatal(errWrite)
	}
	if strings.Contains(strings.ToLower(raw.String()), "x-cc-atis:") {
		t.Fatalf("optional ATIS header was emitted without an assignment:\n%s", raw.String())
	}
	if !strings.Contains(raw.String(), "x-app: value\r\nx-claude-code-request-class: value\r\nx-client-request-id: value\r\n") {
		t.Fatalf("optional ATIS omission changed captured header adjacency:\n%s", raw.String())
	}
}

func TestClaudeDesktopTransportAllowsCapturedControlPlaneClasses(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/v1/code/sessions", true},
		{http.MethodPost, "/v1/code/sessions/cse_abc/bridge", true},
		{http.MethodGet, "/v1/code/sessions/cse_abc/worker/events/stream", true},
		{http.MethodGet, "/v1/code/sessions/cse_abc/worker", true},
		{http.MethodPut, "/v1/code/sessions/cse_abc/worker", true},
		{http.MethodPost, "/v1/code/sessions/cse_abc/worker/events", true},
		{http.MethodPost, "/v1/code/sessions/cse_abc/worker/heartbeat", true},
		{http.MethodGet, "/v1/sessions/session_abc", true},
		{http.MethodPatch, "/v1/sessions/session_abc", true},
		{http.MethodPost, "/v1/sessions/session_abc/archive", true},
		{http.MethodDelete, "/v1/code/sessions/cse_abc/worker", false},
		{http.MethodPost, "/v1/code/sessions/not-cse/bridge", false},
		{http.MethodPost, "/v1/sessions/session_abc/delete", false},
	}
	for _, test := range tests {
		if got := isClaudeDesktopControlPlaneRequest(test.method, test.path); got != test.want {
			t.Fatalf("isClaudeDesktopControlPlaneRequest(%s, %s) = %v, want %v", test.method, test.path, got, test.want)
		}
	}
}

func TestClaudeDesktopEndpointPoliciesCoverEveryCapturedDeliveryTarget(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	tests := []struct {
		role      string
		method    string
		endpoint  string
		query     string
		wantHTTP2 bool
	}{
		{bundle.Telemetry.EndpointRole, http.MethodPost, bundle.Telemetry.Endpoint, "", true},
		{bundle.SDKTelemetry.EndpointRole, http.MethodPost, bundle.SDKTelemetry.Endpoint, "", false},
	}
	for _, auxiliary := range bundle.AuxiliaryTelemetry.All() {
		query := ""
		switch auxiliary.BodyFormat {
		case "datadog-browser-logs-json":
			query = "dd-api-key=test&dd-evp-origin=browser&dd-evp-origin-version=7.6.0&dd-request-id=test&ddsource=browser"
		case "datadog-rum-ndjson":
			query = "_dd.api=fetch&batch_time=1&dd-api-key=test&dd-evp-origin=browser&dd-evp-origin-version=7.6.0&dd-request-id=test&ddsource=browser"
		case "sentry-envelope":
			query = "sentry_client=sentry.javascript.electron%2F7.12.0&sentry_key=test&sentry_version=7"
		}
		tests = append(tests, struct {
			role      string
			method    string
			endpoint  string
			query     string
			wantHTTP2 bool
		}{auxiliary.EndpointRole, http.MethodPost, auxiliary.Endpoint, query, auxiliary.Protocol == "http/2"})
	}
	registry := NewClaudeDesktopTransportRegistry()
	t.Cleanup(registry.CloseAll)
	auth := &cliproxyauth.Auth{ID: "desktop-endpoint-policy"}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			client, errClient := registry.EndpointClient(t.Context(), nil, auth, bundle, test.role)
			if errClient != nil {
				t.Fatal(errClient)
			}
			endpoint := test.endpoint
			if test.query != "" {
				endpoint += "?" + test.query
			}
			request, errRequest := http.NewRequest(test.method, endpoint, nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			bound, ok := client.Transport.(*claudeDesktopBoundRoundTripper)
			if !ok {
				t.Fatalf("transport = %T, want bound Desktop transport", client.Transport)
			}
			if test.wantHTTP2 {
				if _, ok := bound.next.(*claudeDesktopHTTP2Transport); !ok {
					t.Fatalf("underlying transport = %T, want HTTP/2", bound.next)
				}
			} else if _, ok := bound.next.(*claudeDesktopHTTP1Transport); !ok {
				t.Fatalf("underlying transport = %T, want HTTP/1.1", bound.next)
			}
			policy := bound.policy
			if errPolicy := policy.validateRequest(request); errPolicy != nil {
				t.Fatalf("captured target was rejected: %v", errPolicy)
			}
			wrongHost := request.Clone(t.Context())
			wrongURL := *request.URL
			wrongURL.Host = "example.com"
			wrongHost.URL = &wrongURL
			if errPolicy := policy.validateRequest(wrongHost); errPolicy == nil {
				t.Fatal("endpoint policy accepted an unrelated host")
			}
			if test.query != "" {
				unexpectedQuery := request.Clone(t.Context())
				unexpectedURL := *request.URL
				unexpectedURL.RawQuery += "&unexpected=value"
				unexpectedQuery.URL = &unexpectedURL
				if errPolicy := policy.validateRequest(unexpectedQuery); errPolicy == nil {
					t.Fatal("endpoint policy accepted an uncaptured query key")
				}
				missingRequired := request.Clone(t.Context())
				missingURL := *request.URL
				missingURL.RawQuery = ""
				missingRequired.URL = &missingURL
				if errPolicy := policy.validateRequest(missingRequired); errPolicy == nil {
					t.Fatal("endpoint policy accepted a request without required query material")
				}
			}
		})
	}
}

func TestClaudeDesktopEndpointPoliciesRejectCrossRoleTargets(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	type roleTarget struct {
		role     string
		method   string
		endpoint string
		query    string
	}
	targets := []roleTarget{
		{bundle.Telemetry.EndpointRole, http.MethodPost, bundle.Telemetry.Endpoint, ""},
		{bundle.SDKTelemetry.EndpointRole, http.MethodPost, bundle.SDKTelemetry.Endpoint, ""},
	}
	for _, auxiliary := range bundle.AuxiliaryTelemetry.All() {
		query := ""
		switch auxiliary.BodyFormat {
		case "datadog-rum-ndjson":
			query = "_dd.api=fetch&batch_time=1&dd-api-key=test&dd-evp-origin=browser&dd-evp-origin-version=7.6.0&dd-request-id=test&ddsource=browser"
		case "sentry-envelope":
			query = "sentry_client=sentry.javascript.electron%2F7.12.0&sentry_key=test&sentry_version=7"
		}
		targets = append(targets, roleTarget{auxiliary.EndpointRole, http.MethodPost, auxiliary.Endpoint, query})
	}
	for index, own := range targets {
		policy, errPolicy := claudeDesktopEndpointRequestPolicy(bundle, own.role)
		if errPolicy != nil {
			t.Fatal(errPolicy)
		}
		other := targets[(index+1)%len(targets)]
		endpoint := other.endpoint
		if other.query != "" {
			endpoint += "?" + other.query
		}
		request, errRequest := http.NewRequest(other.method, endpoint, nil)
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		if errValidate := policy.validateRequest(request); errValidate == nil {
			t.Fatalf("endpoint role %q accepted captured target owned by %q", own.role, other.role)
		}
	}
}

func TestClaudeDesktopControlPlanePolicyCacheKeyIsDeterministic(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	var want string
	for iteration := 0; iteration < 100; iteration++ {
		policy, errPolicy := claudeDesktopEndpointRequestPolicy(bundle, "control-account-json")
		if errPolicy != nil {
			t.Fatal(errPolicy)
		}
		if got := policy.cacheKey(); want == "" {
			want = got
		} else if got != want {
			t.Fatalf("control-plane policy cache key changed across map iteration: %q != %q", got, want)
		}
	}
}

func TestClaudeDesktopInjectedRoundTripperStillEnforcesPolicy(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	called := false
	delegate := roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, fmt.Errorf("delegate should not be reached")
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(delegate))
	client, errClient := NewClaudeDesktopTransportRegistry().EndpointClient(ctx, nil, &cliproxyauth.Auth{ID: "policy-injection"}, bundle, bundle.SDKTelemetry.EndpointRole)
	if errClient != nil {
		t.Fatal(errClient)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.com/api/event_logging/batch", nil)
	if _, errDo := client.Do(request); errDo == nil {
		t.Fatal("injected RoundTripper bypassed the Desktop request policy")
	}
	if called {
		t.Fatal("policy-rejected request reached the injected RoundTripper")
	}
}

func TestClaudeDesktopControlPlanePoliciesStayBoundToTheirEndpointRole(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	for _, endpoint := range bundle.ControlPlane.Endpoints {
		policy, errPolicy := claudeDesktopEndpointRequestPolicy(bundle, endpoint.EndpointRole)
		if errPolicy != nil {
			t.Fatal(errPolicy)
		}
		path := strings.ReplaceAll(endpoint.Path, "{session_id}", "cse_example")
		request, errRequest := http.NewRequest(endpoint.Method, strings.TrimRight(bundle.ControlPlane.BaseURL, "/")+path, nil)
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		if errValidate := policy.validateRequest(request); errValidate != nil {
			t.Fatalf("role %q rejected %s %s: %v", endpoint.EndpointRole, endpoint.Method, path, errValidate)
		}
	}
	accountPolicy, errPolicy := claudeDesktopEndpointRequestPolicy(bundle, "control-account-json")
	if errPolicy != nil {
		t.Fatal(errPolicy)
	}
	workerRequest, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/code/sessions/cse_example/worker/events", nil)
	if errValidate := accountPolicy.validateRequest(workerRequest); errValidate == nil {
		t.Fatal("account control policy accepted a worker-event request")
	}
}

func TestWriteClaudeDesktopATISBootstrapUsesCapturedHeaderOrder(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	profile, errProfile := bundle.TransportForEndpointRole("atis-bootstrap")
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/claude_cli/bootstrap?entrypoint=claude-desktop&model=claude-sonnet-5", nil)
	for _, name := range profile.HeaderOrder {
		if name == "Host" {
			continue
		}
		request.Header[name] = []string{"value"}
	}
	var raw bytes.Buffer
	if errWrite := writeClaudeDesktopRequest(bufio.NewWriter(&raw), request, nil, profile); errWrite != nil {
		t.Fatal(errWrite)
	}
	lines := strings.Split(strings.SplitN(raw.String(), "\r\n\r\n", 2)[0], "\r\n")
	if len(lines) != len(profile.HeaderOrder)+1 {
		t.Fatalf("bootstrap header lines = %d, want %d\n%s", len(lines), len(profile.HeaderOrder)+1, raw.String())
	}
	for index, name := range profile.HeaderOrder {
		if !strings.HasPrefix(lines[index+1], name+": ") {
			t.Fatalf("bootstrap header[%d] = %q, want %q", index, lines[index+1], name)
		}
	}
	if strings.Contains(strings.ToLower(raw.String()), "content-length:") {
		t.Fatalf("bootstrap unexpectedly emitted Content-Length:\n%s", raw.String())
	}
}

func TestWriteClaudeDesktopSDKTelemetryUsesCapturedHeaderOrder(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	profile, errProfile := bundle.TransportForEndpointRole(bundle.SDKTelemetry.EndpointRole)
	if errProfile != nil {
		t.Fatalf("resolve SDK telemetry transport profile: %v", errProfile)
	}
	req, errRequest := http.NewRequest(http.MethodPost, bundle.SDKTelemetry.Endpoint, strings.NewReader("{}"))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	for _, header := range bundle.SDKTelemetry.Headers {
		req.Header.Set(header.Name, header.Value)
	}
	req.Header.Set("Authorization", "Bearer test")

	var raw bytes.Buffer
	if errWrite := writeClaudeDesktopRequest(bufio.NewWriter(&raw), req, []byte("{}"), profile); errWrite != nil {
		t.Fatalf("write SDK telemetry request: %v", errWrite)
	}
	lines := strings.Split(strings.SplitN(raw.String(), "\r\n\r\n", 2)[0], "\r\n")
	if len(lines) != len(bundle.SDKTelemetry.HeaderOrder)+1 {
		t.Fatalf("line count = %d, want %d\n%s", len(lines), len(bundle.SDKTelemetry.HeaderOrder)+1, raw.String())
	}
	for index, header := range bundle.SDKTelemetry.HeaderOrder {
		if !strings.HasPrefix(lines[index+1], header+": ") {
			t.Fatalf("line %d = %q, want header %q", index+1, lines[index+1], header)
		}
	}
	for _, forbidden := range []string{"x-app:", "X-Stainless-", "X-Claude-Code-Session-Id:"} {
		if strings.Contains(raw.String(), forbidden) {
			t.Fatalf("SDK telemetry inherited Messages header %q:\n%s", forbidden, raw.String())
		}
	}
}

func TestClaudeDesktopRendererHTTP2PrefaceMatchesCapturedFrames(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	profile, errProfile := bundle.TransportForEndpointRole(bundle.Telemetry.EndpointRole)
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	var wire bytes.Buffer
	writer := http2.NewFramer(&wire, nil)
	if errWrite := writeClaudeDesktopHTTP2ConnectionPreface(&wire, writer, profile); errWrite != nil {
		t.Fatal(errWrite)
	}
	if !bytes.HasPrefix(wire.Bytes(), []byte(http2.ClientPreface)) {
		t.Fatal("renderer transport omitted the HTTP/2 client connection preface")
	}
	reader := http2.NewFramer(nil, bytes.NewReader(wire.Bytes()[len(http2.ClientPreface):]))
	frame, errFrame := reader.ReadFrame()
	if errFrame != nil {
		t.Fatal(errFrame)
	}
	settings, ok := frame.(*http2.SettingsFrame)
	if !ok || settings.IsAck() || settings.NumSettings() != len(profile.HTTP2Settings) {
		t.Fatalf("first renderer frame = %#v", frame)
	}
	for index, want := range profile.HTTP2Settings {
		got := settings.Setting(index)
		if uint16(got.ID) != want.ID || got.Val != want.Value {
			t.Fatalf("SETTINGS[%d] = id:%d value:%d, want id:%d value:%d", index, got.ID, got.Val, want.ID, want.Value)
		}
	}
	frame, errFrame = reader.ReadFrame()
	if errFrame != nil {
		t.Fatal(errFrame)
	}
	window, ok := frame.(*http2.WindowUpdateFrame)
	if !ok || window.StreamID != 0 || window.Increment != profile.ConnectionWindow {
		t.Fatalf("second renderer frame = %#v", frame)
	}
}

func TestClaudeDesktopRendererHTTP2HeadersMatchCapturedOrder(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	profile, errProfile := bundle.TransportForEndpointRole(bundle.Telemetry.EndpointRole)
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	request, errRequest := http.NewRequest(http.MethodPost, bundle.Telemetry.Endpoint, strings.NewReader("{}"))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	for _, name := range profile.HeaderOrder {
		if strings.HasPrefix(name, ":") || name == "content-length" {
			continue
		}
		request.Header.Set(name, "value")
	}
	fields, errFields := claudeDesktopHTTP2HeaderFields(request, []byte("{}"), profile.HeaderOrder)
	if errFields != nil {
		t.Fatal(errFields)
	}
	if len(fields) != len(profile.HeaderOrder) {
		t.Fatalf("header fields = %d, want %d", len(fields), len(profile.HeaderOrder))
	}
	for index, want := range profile.HeaderOrder {
		if fields[index].Name != want {
			t.Fatalf("header[%d] = %q, want %q", index, fields[index].Name, want)
		}
	}
}

func TestClaudeDesktopRendererHTTP2ConnectionMultiplexesStreams(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	connection := &claudeDesktopHTTP2Connection{
		conn:                 clientSide,
		framer:               http2.NewFramer(clientSide, clientSide),
		streams:              make(map[uint32]*claudeDesktopHTTP2ResponseState),
		nextStreamID:         1,
		peerInitialWindow:    claudeDesktopHTTP2DefaultWindow,
		connectionSendWindow: claudeDesktopHTTP2DefaultWindow,
		maxFrameSize:         claudeDesktopHTTP2DefaultFrame,
		windowChanged:        make(chan struct{}),
	}
	connection.encoder = hpack.NewEncoder(&connection.encoderBuffer)
	connection.decoder = hpack.NewDecoder(65536, nil)
	connection.decoder.SetAllowedMaxDynamicTableSize(65536)
	go connection.readLoop()

	profile := claudeprofile.TransportProfile{HeaderOrder: []string{":method", ":authority", ":scheme", ":path", "content-length"}}
	type result struct {
		response *http.Response
		err      error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://claude.ai/api/event_logging/v2/batch", strings.NewReader("{}"))
			if errRequest != nil {
				results <- result{err: errRequest}
				return
			}
			response, errRoundTrip := connection.roundTrip(request, []byte("{}"), profile)
			results <- result{response: response, err: errRoundTrip}
		}()
	}
	close(start)

	serverFramer := http2.NewFramer(serverSide, serverSide)
	streamIDs := make(map[uint32]struct{}, 2)
	completedBodies := make(map[uint32]struct{}, 2)
	deadline := time.After(2 * time.Second)
	for len(completedBodies) < 2 {
		read := make(chan http2.Frame, 1)
		errs := make(chan error, 1)
		go func() {
			frame, errRead := serverFramer.ReadFrame()
			if errRead != nil {
				errs <- errRead
				return
			}
			read <- frame
		}()
		select {
		case <-deadline:
			t.Fatal("second HTTP/2 stream was blocked behind the first response")
		case errRead := <-errs:
			t.Fatal(errRead)
		case frame := <-read:
			switch typed := frame.(type) {
			case *http2.HeadersFrame:
				streamIDs[typed.StreamID] = struct{}{}
			case *http2.DataFrame:
				streamIDs[typed.StreamID] = struct{}{}
				if typed.StreamEnded() {
					completedBodies[typed.StreamID] = struct{}{}
				}
			}
		}
	}
	if _, ok := streamIDs[1]; !ok {
		t.Fatalf("stream IDs = %v, missing 1", streamIDs)
	}
	if _, ok := streamIDs[3]; !ok {
		t.Fatalf("stream IDs = %v, missing 3", streamIDs)
	}

	var headerBlock bytes.Buffer
	responseEncoder := hpack.NewEncoder(&headerBlock)
	for _, streamID := range []uint32{3, 1} {
		headerBlock.Reset()
		if errEncode := responseEncoder.WriteField(hpack.HeaderField{Name: ":status", Value: "204"}); errEncode != nil {
			t.Fatal(errEncode)
		}
		if errWrite := serverFramer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: append([]byte(nil), headerBlock.Bytes()...),
			EndHeaders:    true,
			EndStream:     true,
		}); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	for index := 0; index < 2; index++ {
		select {
		case <-time.After(2 * time.Second):
			t.Fatal("multiplexed response did not complete")
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.response == nil || got.response.StatusCode != http.StatusNoContent {
				t.Fatalf("response = %#v", got.response)
			}
		}
	}
}

func TestClaudeDesktopTransportRegistrySharesCompatibleRolesAndIsolatesAccounts(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	registry := NewClaudeDesktopTransportRegistry()
	authA := &cliproxyauth.Auth{ID: "desktop-auth-a"}
	authB := &cliproxyauth.Auth{ID: "desktop-auth-b"}
	mainA, errMainA := registry.Client(t.Context(), nil, authA, bundle, claudeprofile.RoleMain)
	if errMainA != nil {
		t.Fatal(errMainA)
	}
	mainARepeated, errRepeated := registry.Client(t.Context(), nil, authA, bundle, claudeprofile.RoleMain)
	if errRepeated != nil {
		t.Fatal(errRepeated)
	}
	countA, errCountA := registry.Client(t.Context(), nil, authA, bundle, claudeprofile.RoleCountTokens)
	if errCountA != nil {
		t.Fatal(errCountA)
	}
	mainB, errMainB := registry.Client(t.Context(), nil, authB, bundle, claudeprofile.RoleMain)
	if errMainB != nil {
		t.Fatal(errMainB)
	}
	sdkA, errSDKA := registry.EndpointClient(t.Context(), nil, authA, bundle, bundle.SDKTelemetry.EndpointRole)
	if errSDKA != nil {
		t.Fatal(errSDKA)
	}
	logsA, errLogsA := registry.EndpointClient(t.Context(), nil, authA, bundle, bundle.AuxiliaryTelemetry.DatadogLogs.EndpointRole)
	if errLogsA != nil {
		t.Fatal(errLogsA)
	}
	bound := func(client *http.Client) *claudeDesktopBoundRoundTripper {
		t.Helper()
		transport, ok := client.Transport.(*claudeDesktopBoundRoundTripper)
		if !ok {
			t.Fatalf("transport = %T, want *claudeDesktopBoundRoundTripper", client.Transport)
		}
		return transport
	}
	mainABound := bound(mainA)
	if mainABound.next != bound(mainARepeated).next {
		t.Fatal("same account and role did not reuse its Desktop connection pool")
	}
	if mainABound.next != bound(countA).next || mainABound.next != bound(sdkA).next {
		t.Fatal("same-account api.anthropic.com Node roles did not share the official-style connection pool")
	}
	if mainABound.next == bound(mainB).next {
		t.Fatal("two accounts shared mutable Desktop transport state")
	}
	if mainABound.next == bound(logsA).next {
		t.Fatal("different origins shared a Desktop connection pool")
	}
	if got := len(registry.transports); got != 3 {
		t.Fatalf("transport count = %d, want 3", got)
	}
}

func TestClaudeDesktopHTTP1PoolDropsExpiredIdleConnections(t *testing.T) {
	transport := &claudeDesktopHTTP1Transport{
		all: make(map[*claudeDesktopHTTP1Connection]struct{}),
	}
	expiredClient, expiredServer := net.Pipe()
	freshClient, freshServer := net.Pipe()
	t.Cleanup(func() {
		_ = expiredServer.Close()
		_ = freshServer.Close()
	})
	now := time.Now()
	expired := &claudeDesktopHTTP1Connection{
		transport: transport,
		conn:      expiredClient,
		reader:    bufio.NewReader(expiredClient),
		writer:    bufio.NewWriter(expiredClient),
		idleAt:    now.Add(-claudeDesktopHTTP1IdleTimeout - time.Second),
	}
	fresh := &claudeDesktopHTTP1Connection{
		transport: transport,
		conn:      freshClient,
		reader:    bufio.NewReader(freshClient),
		writer:    bufio.NewWriter(freshClient),
		idleAt:    now.Add(-time.Second),
	}
	transport.all[expired] = struct{}{}
	transport.all[fresh] = struct{}{}
	transport.idle = []*claudeDesktopHTTP1Connection{fresh, expired}

	got, closed := transport.checkoutIdleConnection(now)
	if closed {
		t.Fatal("open transport was reported closed")
	}
	if got != fresh {
		t.Fatalf("checkout returned %p, want fresh connection %p", got, fresh)
	}
	if !fresh.idleAt.IsZero() {
		t.Fatalf("checked-out connection retained idle timestamp %s", fresh.idleAt)
	}
	if _, exists := transport.all[expired]; exists {
		t.Fatal("expired idle connection remained registered")
	}
	if _, exists := transport.all[fresh]; !exists {
		t.Fatal("fresh idle connection was removed")
	}
	if len(transport.idle) != 0 {
		t.Fatalf("idle pool length = %d, want 0 after checkout", len(transport.idle))
	}
	_ = fresh.close()
}

func TestClaudeDesktopSentryContentEncodingIsOptional(t *testing.T) {
	bundle, errLoad := claudeprofile.BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("load Desktop profile: %v", errLoad)
	}
	profile, errProfile := bundle.TransportForEndpointRole(bundle.AuxiliaryTelemetry.Sentry.EndpointRole)
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	request, errRequest := http.NewRequest(http.MethodPost, bundle.AuxiliaryTelemetry.Sentry.Endpoint+"?sentry_client=test&sentry_key=test&sentry_version=7", strings.NewReader("{}"))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	for _, header := range bundle.AuxiliaryTelemetry.Sentry.Headers {
		request.Header.Set(header.Name, header.Value)
	}
	fields, errFields := claudeDesktopHTTP2HeaderFieldsForProfile(request, []byte("{}"), profile)
	if errFields != nil {
		t.Fatal(errFields)
	}
	for _, field := range fields {
		if field.Name == "content-encoding" {
			t.Fatal("uncompressed Sentry session unexpectedly emitted content-encoding")
		}
	}
}
