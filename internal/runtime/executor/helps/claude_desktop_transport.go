package helps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"golang.org/x/net/proxy"
)

const (
	claudeDesktopMaxIdleConnections = 32
	claudeDesktopHTTP1IdleTimeout   = 90 * time.Second
)

// ClaudeDesktopTransportRegistry owns account-isolated connection pools and
// TLS session caches. A credential can reuse its own Desktop connections, but
// no mutable transport state is shared with another Auth.ID.
type ClaudeDesktopTransportRegistry struct {
	mu         sync.Mutex
	transports map[string]claudeDesktopTransport
	bindings   map[string]string
}

type claudeDesktopTransport interface {
	roundTripWithProfile(*http.Request, claudeprofile.TransportProfile) (*http.Response, error)
	CloseIdleConnections()
}

type claudeDesktopBoundRoundTripper struct {
	policy  claudeDesktopRequestPolicy
	profile claudeprofile.TransportProfile
	next    claudeDesktopTransport
}

func (t *claudeDesktopBoundRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.next == nil {
		return nil, fmt.Errorf("claude desktop transport binding is incomplete")
	}
	if errPolicy := t.policy.validateRequest(request); errPolicy != nil {
		return nil, errPolicy
	}
	return t.next.roundTripWithProfile(request, t.profile)
}

type claudeDesktopPolicyRoundTripper struct {
	policy claudeDesktopRequestPolicy
	next   http.RoundTripper
}

func (t claudeDesktopPolicyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if errPolicy := t.policy.validateRequest(request); errPolicy != nil {
		return nil, errPolicy
	}
	return t.next.RoundTrip(request)
}

type claudeDesktopRequestTarget struct {
	method            string
	scheme            string
	hostname          string
	port              string
	path              string
	allowAnyQuery     bool
	requiredQueryKeys []string
	optionalQueryKeys []string
	orderedQueryKeys  []string
}

type claudeDesktopRequestPolicy struct {
	role    string
	targets []claudeDesktopRequestTarget
}

func claudeDesktopRoleRequestPolicy(role claudeprofile.RequestRole) (claudeDesktopRequestPolicy, error) {
	path := "/v1/messages"
	if role == claudeprofile.RoleCountTokens {
		path = "/v1/messages/count_tokens"
	}
	switch role {
	case claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleLightHelper,
		claudeprofile.RoleWebSearchHelper, claudeprofile.RoleCompaction, claudeprofile.RoleSubagent,
		claudeprofile.RoleSecurityMonitor, claudeprofile.RoleCountTokens:
		return claudeDesktopRequestPolicy{
			role: string(role),
			targets: []claudeDesktopRequestTarget{{
				method: http.MethodPost, scheme: "https", hostname: "api.anthropic.com", path: path, allowAnyQuery: true,
			}},
		}, nil
	default:
		return claudeDesktopRequestPolicy{}, fmt.Errorf("claude desktop request role %q has no captured transport policy", role)
	}
}

func claudeDesktopEndpointRequestPolicy(bundle *claudeprofile.Bundle, endpointRole string) (claudeDesktopRequestPolicy, error) {
	endpointRole = strings.TrimSpace(endpointRole)
	if bundle == nil || endpointRole == "" {
		return claudeDesktopRequestPolicy{}, fmt.Errorf("claude desktop endpoint transport policy is incomplete")
	}
	policy := claudeDesktopRequestPolicy{role: endpointRole}
	addEndpoint := func(method, rawURL string, requiredQueryKeys, optionalQueryKeys []string) error {
		target, errTarget := claudeDesktopRequestTargetForURL(method, rawURL, requiredQueryKeys, optionalQueryKeys)
		if errTarget != nil {
			return errTarget
		}
		policy.targets = append(policy.targets, target)
		return nil
	}
	switch endpointRole {
	case bundle.Telemetry.EndpointRole:
		if errAdd := addEndpoint(http.MethodPost, bundle.Telemetry.Endpoint, nil, nil); errAdd != nil {
			return claudeDesktopRequestPolicy{}, errAdd
		}
	case bundle.SDKTelemetry.EndpointRole:
		if errAdd := addEndpoint(http.MethodPost, bundle.SDKTelemetry.Endpoint, nil, nil); errAdd != nil {
			return claudeDesktopRequestPolicy{}, errAdd
		}
	default:
		matched := false
		for _, auxiliary := range bundle.AuxiliaryTelemetry.All() {
			if auxiliary.EndpointRole != endpointRole {
				continue
			}
			var requiredQueryKeys, optionalQueryKeys []string
			switch auxiliary.BodyFormat {
			case "datadog-browser-logs-json":
				requiredQueryKeys = []string{"dd-api-key", "dd-evp-origin", "dd-evp-origin-version", "dd-request-id", "ddsource"}
			case "datadog-rum-ndjson":
				requiredQueryKeys = []string{"_dd.api", "batch_time", "dd-api-key", "dd-evp-origin", "dd-evp-origin-version", "dd-request-id", "ddsource"}
				optionalQueryKeys = []string{"dd-evp-encoding"}
			case "sentry-envelope":
				requiredQueryKeys = []string{"sentry_client", "sentry_key", "sentry_version"}
			}
			if errAdd := addEndpoint(http.MethodPost, auxiliary.Endpoint, requiredQueryKeys, optionalQueryKeys); errAdd != nil {
				return claudeDesktopRequestPolicy{}, errAdd
			}
			matched = true
			break
		}
		if !matched && endpointRole == "atis-bootstrap" {
			if errAdd := addEndpoint(http.MethodGet, "https://api.anthropic.com/api/claude_cli/bootstrap", []string{"entrypoint"}, []string{"model"}); errAdd != nil {
				return claudeDesktopRequestPolicy{}, errAdd
			}
			matched = true
		}
		if !matched {
			for _, endpoint := range bundle.Startup.Endpoints {
				if endpoint.EndpointRole != endpointRole {
					continue
				}
				target, errTarget := claudeDesktopRequestTargetForURL(endpoint.Method, endpoint.Endpoint, nil, nil)
				if errTarget != nil {
					return claudeDesktopRequestPolicy{}, errTarget
				}
				for _, query := range endpoint.Query {
					target.orderedQueryKeys = append(target.orderedQueryKeys, query.Name)
				}
				policy.targets = append(policy.targets, target)
				matched = true
				break
			}
		}
		if !matched {
			baseURL, errBase := url.Parse(bundle.ControlPlane.BaseURL)
			if errBase != nil || !strings.EqualFold(baseURL.Scheme, "https") || baseURL.Hostname() == "" {
				return claudeDesktopRequestPolicy{}, fmt.Errorf("claude desktop control-plane base URL is invalid")
			}
			for _, endpoint := range bundle.ControlPlane.Endpoints {
				if endpoint.EndpointRole != endpointRole {
					continue
				}
				targetURL := *baseURL
				targetURL.Path = endpoint.Path
				targetURL.RawPath = ""
				targetURL.RawQuery = ""
				if errAdd := addEndpoint(endpoint.Method, targetURL.String(), nil, nil); errAdd != nil {
					return claudeDesktopRequestPolicy{}, errAdd
				}
				matched = true
			}
		}
		if !matched {
			return claudeDesktopRequestPolicy{}, fmt.Errorf("claude desktop endpoint role %q has no captured request target", endpointRole)
		}
	}
	policy.canonicalize()
	if errPolicy := policy.validateDefinition(); errPolicy != nil {
		return claudeDesktopRequestPolicy{}, errPolicy
	}
	return policy, nil
}

func claudeDesktopRequestTargetForURL(method, rawURL string, requiredQueryKeys, optionalQueryKeys []string) (claudeDesktopRequestTarget, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.Path == "" || parsed.User != nil || parsed.Fragment != "" {
		return claudeDesktopRequestTarget{}, fmt.Errorf("claude desktop transport target is invalid")
	}
	return claudeDesktopRequestTarget{
		method: strings.ToUpper(strings.TrimSpace(method)), scheme: "https", hostname: strings.ToLower(parsed.Hostname()),
		port: parsed.Port(), path: parsed.Path,
		requiredQueryKeys: append([]string(nil), requiredQueryKeys...),
		optionalQueryKeys: append([]string(nil), optionalQueryKeys...),
	}, nil
}

func (p *claudeDesktopRequestPolicy) canonicalize() {
	if p == nil {
		return
	}
	for index := range p.targets {
		sort.Strings(p.targets[index].requiredQueryKeys)
		sort.Strings(p.targets[index].optionalQueryKeys)
	}
	sort.Slice(p.targets, func(left, right int) bool {
		return p.targets[left].cacheKey() < p.targets[right].cacheKey()
	})
}

func (p claudeDesktopRequestPolicy) validateDefinition() error {
	if strings.TrimSpace(p.role) == "" || len(p.targets) == 0 {
		return fmt.Errorf("claude desktop transport request policy is empty")
	}
	for _, target := range p.targets {
		if target.method == "" || target.scheme != "https" || target.hostname == "" || target.path == "" {
			return fmt.Errorf("claude desktop transport request policy %q is invalid", p.role)
		}
		seenQueryKeys := make(map[string]struct{}, len(target.requiredQueryKeys)+len(target.optionalQueryKeys))
		queryKeys := append(append([]string(nil), target.requiredQueryKeys...), target.optionalQueryKeys...)
		for _, key := range queryKeys {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("claude desktop transport request policy %q has an empty query key", p.role)
			}
			if _, duplicate := seenQueryKeys[key]; duplicate {
				return fmt.Errorf("claude desktop transport request policy %q repeats query key %q", p.role, key)
			}
			seenQueryKeys[key] = struct{}{}
		}
		for _, key := range target.orderedQueryKeys {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("claude desktop transport request policy %q has an empty ordered query key", p.role)
			}
		}
	}
	return nil
}

func (p claudeDesktopRequestPolicy) validateRequest(request *http.Request) error {
	if request == nil || request.URL == nil {
		return fmt.Errorf("claude desktop transport received a nil request")
	}
	for _, target := range p.targets {
		if request.Method != target.method || !strings.EqualFold(request.URL.Scheme, target.scheme) ||
			!strings.EqualFold(request.URL.Hostname(), target.hostname) || request.URL.Port() != target.port ||
			!claudeDesktopPathMatches(target.path, request.URL.Path) || !target.allowsQuery(request.URL.RawQuery) {
			continue
		}
		return nil
	}
	return fmt.Errorf("claude desktop transport policy %q refuses %s %s://%s%s", p.role, request.Method, request.URL.Scheme, request.URL.Host, request.URL.EscapedPath())
}

func (t claudeDesktopRequestTarget) allowsQuery(rawQuery string) bool {
	if t.allowAnyQuery {
		return true
	}
	if len(t.orderedQueryKeys) > 0 {
		parts := strings.Split(rawQuery, "&")
		if len(parts) != len(t.orderedQueryKeys) {
			return false
		}
		for index, part := range parts {
			name, value, found := strings.Cut(part, "=")
			decodedName, errName := url.QueryUnescape(name)
			decodedValue, errValue := url.QueryUnescape(value)
			if !found || errName != nil || errValue != nil || decodedName != t.orderedQueryKeys[index] || strings.TrimSpace(decodedValue) == "" {
				return false
			}
		}
		return true
	}
	values, errParse := url.ParseQuery(rawQuery)
	if errParse != nil {
		return false
	}
	allowed := make(map[string]struct{}, len(t.requiredQueryKeys)+len(t.optionalQueryKeys))
	for _, key := range t.requiredQueryKeys {
		allowed[key] = struct{}{}
		items, exists := values[key]
		if !exists || len(items) != 1 || strings.TrimSpace(items[0]) == "" {
			return false
		}
	}
	for _, key := range t.optionalQueryKeys {
		allowed[key] = struct{}{}
	}
	for key, items := range values {
		if _, ok := allowed[key]; !ok || len(items) != 1 || strings.TrimSpace(items[0]) == "" {
			return false
		}
	}
	return true
}

func (t claudeDesktopRequestTarget) cacheKey() string {
	return strings.Join([]string{
		t.method, t.scheme, t.hostname, t.port, t.path, strconv.FormatBool(t.allowAnyQuery),
		strings.Join(t.requiredQueryKeys, ","), strings.Join(t.optionalQueryKeys, ","), strings.Join(t.orderedQueryKeys, ","),
	}, "\x1f")
}

func (p claudeDesktopRequestPolicy) cacheKey() string {
	parts := make([]string, 0, len(p.targets)+1)
	parts = append(parts, p.role)
	for _, target := range p.targets {
		parts = append(parts, target.cacheKey())
	}
	return strings.Join(parts, "\x1e")
}

func (p claudeDesktopRequestPolicy) originCacheKey() string {
	origins := make([]string, 0, len(p.targets))
	seen := make(map[string]struct{}, len(p.targets))
	for _, target := range p.targets {
		port := target.port
		if port == "" {
			port = "443"
		}
		origin := strings.Join([]string{target.scheme, target.hostname, port}, "\x1f")
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	return strings.Join(origins, "\x1e")
}

func claudeDesktopPathMatches(template, path string) bool {
	if !strings.Contains(template, "{") {
		return path == template
	}
	templateParts := strings.Split(template, "/")
	pathParts := strings.Split(path, "/")
	if len(templateParts) != len(pathParts) {
		return false
	}
	for index, part := range templateParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && len(part) > 2 {
			if pathParts[index] == "" || strings.Contains(pathParts[index], "/") {
				return false
			}
			continue
		}
		if pathParts[index] != part {
			return false
		}
	}
	return true
}

func NewClaudeDesktopTransportRegistry() *ClaudeDesktopTransportRegistry {
	return &ClaudeDesktopTransportRegistry{
		transports: make(map[string]claudeDesktopTransport),
		bindings:   make(map[string]string),
	}
}

// Client returns the captured Desktop transport for one request role.
func (r *ClaudeDesktopTransportRegistry) Client(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	bundle *claudeprofile.Bundle,
	role claudeprofile.RequestRole,
) (*http.Client, error) {
	if r == nil {
		return nil, fmt.Errorf("claude desktop transport registry is nil")
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, fmt.Errorf("claude desktop transport requires an Auth.ID")
	}
	if bundle == nil {
		return nil, fmt.Errorf("claude desktop transport requires a profile bundle")
	}
	transportProfile, errProfile := bundle.TransportForRole(role)
	if errProfile != nil {
		return nil, errProfile
	}
	policy, errPolicy := claudeDesktopRoleRequestPolicy(role)
	if errPolicy != nil {
		return nil, errPolicy
	}
	return r.clientForProfile(ctx, cfg, auth, bundle, transportProfile, policy)
}

// EndpointClient returns an account-isolated Desktop transport for a background
// endpoint. Compatible roles on the same origin share the physical pool, as
// the official Desktop does, while request policy and header order stay bound
// to the individual client.
func (r *ClaudeDesktopTransportRegistry) EndpointClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	bundle *claudeprofile.Bundle,
	endpointRole string,
) (*http.Client, error) {
	if r == nil {
		return nil, fmt.Errorf("claude desktop transport registry is nil")
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, fmt.Errorf("claude desktop transport requires an Auth.ID")
	}
	if bundle == nil {
		return nil, fmt.Errorf("claude desktop transport requires a profile bundle")
	}
	transportProfile, errProfile := bundle.TransportForEndpointRole(endpointRole)
	if errProfile != nil {
		return nil, errProfile
	}
	policy, errPolicy := claudeDesktopEndpointRequestPolicy(bundle, endpointRole)
	if errPolicy != nil {
		return nil, errPolicy
	}
	return r.clientForProfile(ctx, cfg, auth, bundle, transportProfile, policy)
}

func (r *ClaudeDesktopTransportRegistry) clientForProfile(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	bundle *claudeprofile.Bundle,
	transportProfile claudeprofile.TransportProfile,
	policy claudeDesktopRequestPolicy,
) (*http.Client, error) {

	proxyURL := strings.TrimSpace(auth.ProxyURL)
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	// Executor tests and embedders may provide an in-process RoundTripper. An
	// explicit proxy still uses the Desktop dialer so its upstream TLS remains
	// profile-owned.
	if proxyURL == "" && ctx != nil {
		if roundTripper, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && roundTripper != nil {
			return &http.Client{Transport: claudeDesktopPolicyRoundTripper{policy: policy, next: roundTripper}}, nil
		}
	}

	binding := bundle.ProfileID + "\x00" + proxyURL
	transportKey := auth.ID + "\x00" + binding + "\x00" + policy.originCacheKey() + "\x00" + transportProfile.Protocol + "\x00" + transportProfile.ClientHelloPreset + "\x00" + transportProfile.JA3Hash + "\x00" + fmt.Sprint(transportProfile.CipherSuites) + "\x00" + fmt.Sprint(transportProfile.ExtensionOrder) + "\x00" + fmt.Sprint(transportProfile.SupportedGroups) + "\x00" + fmt.Sprint(transportProfile.SignatureAlgorithms) + "\x00" + fmt.Sprint(transportProfile.SupportedVersions) + "\x00" + fmt.Sprint(transportProfile.KeyShareGroups) + "\x00" + fmt.Sprint(transportProfile.ECPointFormats) + "\x00" + strings.Join(transportProfile.ALPN, "\x00") + "\x00" + fmt.Sprint(transportProfile.HTTP2Settings) + "\x00" + strconv.FormatUint(uint64(transportProfile.ConnectionWindow), 10)

	r.mu.Lock()
	if r.transports == nil {
		r.transports = make(map[string]claudeDesktopTransport)
	}
	if r.bindings == nil {
		r.bindings = make(map[string]string)
	}
	var stale []claudeDesktopTransport
	if previous := r.bindings[auth.ID]; previous != "" && previous != binding {
		prefix := auth.ID + "\x00"
		for key, transport := range r.transports {
			if strings.HasPrefix(key, prefix) {
				stale = append(stale, transport)
				delete(r.transports, key)
			}
		}
	}
	r.bindings[auth.ID] = binding
	transport := r.transports[transportKey]
	if transport == nil {
		var errNew error
		switch transportProfile.Protocol {
		case "http/1.1":
			transport, errNew = newClaudeDesktopHTTP1Transport(transportProfile, policy, proxyURL)
		case "http/2":
			transport, errNew = newClaudeDesktopHTTP2Transport(transportProfile, policy, proxyURL)
		default:
			errNew = fmt.Errorf("claude desktop transport protocol %q is not supported", transportProfile.Protocol)
		}
		if errNew != nil {
			r.mu.Unlock()
			for _, old := range stale {
				old.CloseIdleConnections()
			}
			return nil, errNew
		}
		r.transports[transportKey] = transport
	}
	r.mu.Unlock()
	for _, old := range stale {
		old.CloseIdleConnections()
	}
	return &http.Client{Transport: &claudeDesktopBoundRoundTripper{policy: policy, profile: transportProfile, next: transport}}, nil
}

func (r *ClaudeDesktopTransportRegistry) CloseAuth(authID string) {
	if r == nil || strings.TrimSpace(authID) == "" {
		return
	}
	r.mu.Lock()
	prefix := authID + "\x00"
	var transports []claudeDesktopTransport
	for key, transport := range r.transports {
		if strings.HasPrefix(key, prefix) {
			transports = append(transports, transport)
			delete(r.transports, key)
		}
	}
	delete(r.bindings, authID)
	r.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

// CloseAll releases every connection and TLS session cache owned by the registry.
func (r *ClaudeDesktopTransportRegistry) CloseAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	transports := make([]claudeDesktopTransport, 0, len(r.transports))
	for key, transport := range r.transports {
		transports = append(transports, transport)
		delete(r.transports, key)
	}
	clear(r.bindings)
	r.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

type claudeDesktopHTTP1Transport struct {
	profile      claudeprofile.TransportProfile
	policy       claudeDesktopRequestPolicy
	dialer       proxy.ContextDialer
	sessionCache tls.ClientSessionCache

	mu     sync.Mutex
	idle   []*claudeDesktopHTTP1Connection
	all    map[*claudeDesktopHTTP1Connection]struct{}
	closed bool
}

type claudeDesktopHTTP1Connection struct {
	transport *claudeDesktopHTTP1Transport
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	idleAt    time.Time
	closeOnce sync.Once
}

func newClaudeDesktopHTTP1Transport(profile claudeprofile.TransportProfile, policy claudeDesktopRequestPolicy, proxyURL string) (*claudeDesktopHTTP1Transport, error) {
	if profile.Protocol != "http/1.1" {
		return nil, fmt.Errorf("claude desktop transport protocol %q is not supported", profile.Protocol)
	}
	if errPolicy := policy.validateDefinition(); errPolicy != nil {
		return nil, errPolicy
	}
	dialer, errDialer := claudeDesktopDialer(proxyURL)
	if errDialer != nil {
		return nil, errDialer
	}
	return &claudeDesktopHTTP1Transport{
		profile:      profile,
		policy:       policy,
		dialer:       dialer,
		sessionCache: tls.NewLRUClientSessionCache(64),
		all:          make(map[*claudeDesktopHTTP1Connection]struct{}),
	}, nil
}

func claudeDesktopDialer(proxyURL string) (proxy.ContextDialer, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return &net.Dialer{}, nil
	}
	dialer, _, errBuild := proxyutil.BuildDialer(proxyURL)
	if errBuild != nil {
		return nil, fmt.Errorf("build Claude Desktop proxy dialer: %w", errBuild)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok || contextDialer == nil {
		return nil, fmt.Errorf("Claude Desktop proxy dialer does not support context cancellation")
	}
	return contextDialer, nil
}

func (t *claudeDesktopHTTP1Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("claude desktop transport received a nil request")
	}
	if errPolicy := t.policy.validateRequest(req); errPolicy != nil {
		return nil, errPolicy
	}
	return t.roundTripWithProfile(req, t.profile)
}

func (t *claudeDesktopHTTP1Transport) roundTripWithProfile(req *http.Request, profile claudeprofile.TransportProfile) (*http.Response, error) {
	body, errBody := readClaudeDesktopRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}

	connection, errConnection := t.connection(req.Context(), req.URL.Hostname(), claudeDesktopAddress(req.URL.Hostname(), req.URL.Port()))
	if errConnection != nil {
		return nil, errConnection
	}
	stopCancel := context.AfterFunc(req.Context(), func() {
		_ = connection.close()
	})

	if errWrite := writeClaudeDesktopRequest(connection.writer, req, body, profile); errWrite != nil {
		stopCancel()
		_ = connection.close()
		return nil, errWrite
	}
	response, errResponse := http.ReadResponse(connection.reader, req)
	if errResponse != nil {
		stopCancel()
		_ = connection.close()
		return nil, fmt.Errorf("read Claude Desktop upstream response: %w", errResponse)
	}
	response.Request = req
	reusable := !response.Close && !headerContainsToken(response.Header, "Connection", "close")
	if response.Body == nil || response.Body == http.NoBody || response.ContentLength == 0 {
		stopCancel()
		if reusable {
			t.release(connection)
		} else {
			_ = connection.close()
		}
		response.Body = http.NoBody
		return response, nil
	}
	response.Body = &claudeDesktopResponseBody{
		body:       response.Body,
		transport:  t,
		connection: connection,
		reusable:   reusable,
		stopCancel: stopCancel,
	}
	return response, nil
}

func isClaudeDesktopControlPlaneRequest(method, path string) bool {
	if method == http.MethodPost && path == "/v1/code/sessions" {
		return true
	}
	if strings.HasPrefix(path, "/v1/code/sessions/cse_") {
		suffix := strings.TrimPrefix(path, "/v1/code/sessions/")
		sessionID, operation, okOperation := strings.Cut(suffix, "/")
		if !okOperation || len(sessionID) <= len("cse_") {
			return false
		}
		switch operation {
		case "bridge":
			return method == http.MethodPost
		case "worker":
			return method == http.MethodGet || method == http.MethodPut
		case "worker/events":
			return method == http.MethodPost
		case "worker/heartbeat":
			return method == http.MethodPost
		case "worker/events/stream":
			return method == http.MethodGet
		default:
			return false
		}
	}
	if strings.HasPrefix(path, "/v1/sessions/session_") {
		suffix := strings.TrimPrefix(path, "/v1/sessions/")
		sessionID, operation, hasOperation := strings.Cut(suffix, "/")
		if len(sessionID) <= len("session_") {
			return false
		}
		if !hasOperation {
			return method == http.MethodGet || method == http.MethodPatch
		}
		return operation == "archive" && method == http.MethodPost
	}
	return false
}

func readClaudeDesktopRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, errRead := io.ReadAll(req.Body)
	errClose := req.Body.Close()
	if errRead != nil {
		return nil, fmt.Errorf("read Claude Desktop request body: %w", errRead)
	}
	if errClose != nil {
		return nil, fmt.Errorf("close Claude Desktop request body: %w", errClose)
	}
	return body, nil
}

func claudeDesktopAddress(hostname, port string) string {
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(hostname, port)
}

func (t *claudeDesktopHTTP1Transport) connection(ctx context.Context, hostname, address string) (*claudeDesktopHTTP1Connection, error) {
	if connection, closed := t.checkoutIdleConnection(time.Now()); connection != nil || closed {
		if closed {
			return nil, fmt.Errorf("claude desktop transport is closed")
		}
		return connection, nil
	}

	rawConnection, errDial := t.dialer.DialContext(ctx, "tcp", address)
	if errDial != nil {
		return nil, fmt.Errorf("dial Claude Desktop upstream: %w", errDial)
	}
	tlsConfig := &tls.Config{
		ServerName:                         hostname,
		ClientSessionCache:                 t.sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
	tlsConnection := tls.UClient(rawConnection, tlsConfig, tls.HelloCustom)
	spec, errSpec := claudeDesktopClientHelloSpec(t.profile, hostname)
	if errSpec != nil {
		_ = rawConnection.Close()
		return nil, errSpec
	}
	if errPreset := tlsConnection.ApplyPreset(spec); errPreset != nil {
		_ = rawConnection.Close()
		return nil, fmt.Errorf("apply Claude Desktop ClientHello profile: %w", errPreset)
	}
	if errHandshake := tlsConnection.HandshakeContext(ctx); errHandshake != nil {
		_ = rawConnection.Close()
		return nil, fmt.Errorf("Claude Desktop TLS handshake: %w", errHandshake)
	}
	if negotiated := tlsConnection.ConnectionState().NegotiatedProtocol; negotiated != "" && negotiated != "http/1.1" {
		_ = tlsConnection.Close()
		return nil, fmt.Errorf("Claude Desktop transport negotiated unexpected ALPN %q", negotiated)
	}

	connection := &claudeDesktopHTTP1Connection{
		transport: t,
		conn:      tlsConnection,
		reader:    bufio.NewReader(tlsConnection),
		writer:    bufio.NewWriter(tlsConnection),
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = connection.close()
		return nil, fmt.Errorf("claude desktop transport is closed")
	}
	t.all[connection] = struct{}{}
	t.mu.Unlock()
	return connection, nil
}

func (t *claudeDesktopHTTP1Transport) checkoutIdleConnection(now time.Time) (*claudeDesktopHTTP1Connection, bool) {
	if t == nil {
		return nil, true
	}
	var expired []*claudeDesktopHTTP1Connection
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, true
	}
	for len(t.idle) > 0 {
		last := len(t.idle) - 1
		connection := t.idle[last]
		t.idle = t.idle[:last]
		if connection == nil {
			continue
		}
		if connection.idleAt.IsZero() || now.Sub(connection.idleAt) < claudeDesktopHTTP1IdleTimeout {
			connection.idleAt = time.Time{}
			t.mu.Unlock()
			for _, stale := range expired {
				_ = stale.close()
			}
			return connection, false
		}
		expired = append(expired, connection)
	}
	t.mu.Unlock()
	for _, stale := range expired {
		_ = stale.close()
	}
	return nil, false
}

func claudeDesktopClientHelloSpec(profile claudeprofile.TransportProfile, hostname string) (*tls.ClientHelloSpec, error) {
	versions := append([]uint16(nil), profile.SupportedVersions...)
	if len(versions) == 0 {
		return nil, fmt.Errorf("Claude Desktop TLS profile has no supported versions")
	}
	minimum, maximum := versions[0], versions[0]
	for _, version := range versions[1:] {
		if version < minimum {
			minimum = version
		}
		if version > maximum {
			maximum = version
		}
	}
	extensions := make([]tls.TLSExtension, 0, len(profile.ExtensionOrder))
	for _, extensionID := range profile.ExtensionOrder {
		extension, errExtension := claudeDesktopTLSExtension(profile, hostname, extensionID)
		if errExtension != nil {
			return nil, errExtension
		}
		extensions = append(extensions, extension)
	}
	return &tls.ClientHelloSpec{
		CipherSuites:       append([]uint16(nil), profile.CipherSuites...),
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
		TLSVersMin:         minimum,
		TLSVersMax:         maximum,
	}, nil
}

func claudeDesktopTLSExtension(profile claudeprofile.TransportProfile, hostname string, extensionID uint16) (tls.TLSExtension, error) {
	switch extensionID {
	case 0:
		return &tls.SNIExtension{ServerName: hostname}, nil
	case 5:
		return &tls.StatusRequestExtension{}, nil
	case 10:
		curves := make([]tls.CurveID, 0, len(profile.SupportedGroups))
		for _, group := range profile.SupportedGroups {
			curves = append(curves, tls.CurveID(group))
		}
		return &tls.SupportedCurvesExtension{Curves: curves}, nil
	case 11:
		return &tls.SupportedPointsExtension{SupportedPoints: append([]uint8(nil), profile.ECPointFormats...)}, nil
	case 13:
		schemes := make([]tls.SignatureScheme, 0, len(profile.SignatureAlgorithms))
		for _, algorithm := range profile.SignatureAlgorithms {
			schemes = append(schemes, tls.SignatureScheme(algorithm))
		}
		return &tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: schemes}, nil
	case 16:
		return &tls.ALPNExtension{AlpnProtocols: append([]string(nil), profile.ALPN...)}, nil
	case 18:
		return &tls.SCTExtension{}, nil
	case 21:
		return &tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle}, nil
	case 23:
		return &tls.ExtendedMasterSecretExtension{}, nil
	case 35:
		return &tls.SessionTicketExtension{}, nil
	case 43:
		return &tls.SupportedVersionsExtension{Versions: append([]uint16(nil), profile.SupportedVersions...)}, nil
	case 45:
		return &tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}}, nil
	case 51:
		shares := make([]tls.KeyShare, 0, len(profile.KeyShareGroups))
		for _, group := range profile.KeyShareGroups {
			shares = append(shares, tls.KeyShare{Group: tls.CurveID(group)})
		}
		return &tls.KeyShareExtension{KeyShares: shares}, nil
	case 65281:
		return &tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient}, nil
	default:
		return nil, fmt.Errorf("Claude Desktop TLS profile contains unsupported extension %d", extensionID)
	}
}

func writeClaudeDesktopRequest(writer *bufio.Writer, req *http.Request, body []byte, profile claudeprofile.TransportProfile) error {
	if writer == nil {
		return fmt.Errorf("Claude Desktop request writer is nil")
	}
	requestURI := req.URL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	if _, errWrite := fmt.Fprintf(writer, "%s %s HTTP/1.1\r\n", req.Method, requestURI); errWrite != nil {
		return fmt.Errorf("write Claude Desktop request line: %w", errWrite)
	}

	known := make(map[string]struct{}, len(profile.HeaderOrder))
	for _, wireName := range profile.HeaderOrder {
		lowerName := strings.ToLower(wireName)
		known[lowerName] = struct{}{}
		var values []string
		switch lowerName {
		case "host":
			host := strings.TrimSpace(req.Host)
			if host == "" {
				host = req.URL.Host
			}
			values = []string{host}
		case "content-length":
			values = []string{strconv.Itoa(len(body))}
		default:
			values = claudeDesktopHeaderValues(req.Header, wireName)
		}
		if len(values) == 0 {
			// The Node sender uses one stable relative order with role-specific
			// omissions (for example count_tokens omits timeout, and ATIS is
			// conditional on bootstrap assignment). Missing values keep their
			// profiled slot empty without allowing any unprofiled header.
			continue
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("Claude Desktop header %q contains a line break", wireName)
			}
			if _, errWrite := fmt.Fprintf(writer, "%s: %s\r\n", wireName, value); errWrite != nil {
				return fmt.Errorf("write Claude Desktop header %q: %w", wireName, errWrite)
			}
		}
	}
	for name := range req.Header {
		if _, ok := known[strings.ToLower(name)]; !ok {
			return fmt.Errorf("Claude Desktop request contains unprofiled header %q", name)
		}
	}
	if _, errWrite := writer.WriteString("\r\n"); errWrite != nil {
		return fmt.Errorf("finish Claude Desktop headers: %w", errWrite)
	}
	if len(body) > 0 {
		if _, errWrite := writer.Write(body); errWrite != nil {
			return fmt.Errorf("write Claude Desktop request body: %w", errWrite)
		}
	}
	if errFlush := writer.Flush(); errFlush != nil {
		return fmt.Errorf("flush Claude Desktop request: %w", errFlush)
	}
	return nil
}

func claudeDesktopOptionalHeaders(headers []string) map[string]struct{} {
	optional := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		if header = strings.ToLower(strings.TrimSpace(header)); header != "" {
			optional[header] = struct{}{}
		}
	}
	return optional
}

func claudeDesktopHeaderValues(headers http.Header, name string) []string {
	for candidate, values := range headers {
		if strings.EqualFold(candidate, name) {
			return values
		}
	}
	return nil
}

func headerContainsToken(headers http.Header, name, token string) bool {
	for _, value := range headers.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

func (t *claudeDesktopHTTP1Transport) release(connection *claudeDesktopHTTP1Connection) {
	if t == nil || connection == nil {
		return
	}
	t.mu.Lock()
	if t.closed || len(t.idle) >= claudeDesktopMaxIdleConnections {
		t.mu.Unlock()
		_ = connection.close()
		return
	}
	connection.idleAt = time.Now()
	t.idle = append(t.idle, connection)
	t.mu.Unlock()
}

func (t *claudeDesktopHTTP1Transport) forget(connection *claudeDesktopHTTP1Connection) {
	if t == nil || connection == nil {
		return
	}
	t.mu.Lock()
	delete(t.all, connection)
	for index := len(t.idle) - 1; index >= 0; index-- {
		if t.idle[index] == connection {
			t.idle = append(t.idle[:index], t.idle[index+1:]...)
		}
	}
	t.mu.Unlock()
}

func (t *claudeDesktopHTTP1Transport) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.closed = true
	connections := make([]*claudeDesktopHTTP1Connection, 0, len(t.all))
	for connection := range t.all {
		connections = append(connections, connection)
	}
	t.idle = nil
	t.mu.Unlock()
	for _, connection := range connections {
		_ = connection.close()
	}
}

func (c *claudeDesktopHTTP1Connection) close() error {
	if c == nil {
		return nil
	}
	var errClose error
	c.closeOnce.Do(func() {
		if c.conn != nil {
			errClose = c.conn.Close()
		}
		if c.transport != nil {
			c.transport.forget(c)
		}
	})
	return errClose
}

type claudeDesktopResponseBody struct {
	body       io.ReadCloser
	transport  *claudeDesktopHTTP1Transport
	connection *claudeDesktopHTTP1Connection
	reusable   bool
	stopCancel func() bool

	mu       sync.Mutex
	eof      bool
	finished bool
}

func (b *claudeDesktopResponseBody) Read(p []byte) (int, error) {
	if b == nil || b.body == nil {
		return 0, io.EOF
	}
	n, errRead := b.body.Read(p)
	if errors.Is(errRead, io.EOF) {
		b.mu.Lock()
		b.eof = true
		b.mu.Unlock()
	}
	return n, errRead
}

func (b *claudeDesktopResponseBody) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.finished {
		b.mu.Unlock()
		return nil
	}
	b.finished = true
	eof := b.eof
	b.mu.Unlock()

	errBody := b.body.Close()
	contextActive := true
	if b.stopCancel != nil {
		contextActive = b.stopCancel()
	}
	if eof && b.reusable && contextActive && errBody == nil {
		b.transport.release(b.connection)
		return nil
	}
	errConnection := b.connection.close()
	return errors.Join(errBody, errConnection)
}
