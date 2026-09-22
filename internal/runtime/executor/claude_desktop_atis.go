package executor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

const (
	claudeDesktopATISEndpointRole = "atis-bootstrap"
	claudeDesktopATISStateVersion = 2
	claudeDesktopATISMaxBodyBytes = 1 << 20
	claudeDesktopATISTimeout      = 10 * time.Second
)

type claudeDesktopATISHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type claudeDesktopATISDoerFactory func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error)

type claudeDesktopATISManager struct {
	statePath    string
	codeVersion  string
	client       claudeDesktopATISClient
	profileID    string
	doerFactory  claudeDesktopATISDoerFactory
	now          func() time.Time
	requestGroup singleflight.Group

	mu                         sync.Mutex
	loaded                     bool
	identityHash               string
	assignments                map[string]claudeDesktopATISAssignment
	sessions                   map[string]claudeDesktopATISSessionBinding
	legacyFeatures             map[string]json.RawMessage
	loadErr                    error
	persistErr                 error
	featureBindMu              sync.Mutex
	featureHosts               *claudefeatures.Registry
	desktopRecords             *claudesessions.Registry
	featureStartMu             sync.Mutex
	featureStarted             map[*claudefeatures.Host]bool
	startFeatureHost           func(*claudefeatures.Host) error
	featureSink                func(*cliproxyauth.Auth, claudefeatures.Exposure) (bool, error)
	featureStateObserver       func(*cliproxyauth.Auth, string, bool) error
	featureHostObserverFactory func(*cliproxyauth.Auth, string) (func(bool) error, func(), error)
	featureHealthMu            sync.Mutex
	featureHealth              map[*claudefeatures.Host]map[string]claudeDesktopFeatureHealth
	featureHealthClosed        bool
}

type claudeDesktopFeatureHealth struct {
	observe func(bool) error
	retire  func()
}

type claudeDesktopQueryContextKey struct{}

var errClaudeDesktopQueryOwnerUnavailable = errors.New("Claude Desktop request query owner is no longer available")

// This handle is private execution ownership, not a caller header or a durable
// session identifier. Every use checks the runtime and account binding again.
type claudeDesktopQueryContext struct {
	manager                                         *claudeDesktopATISManager
	host                                            *claudefeatures.Host
	accountID, profileID, egress, identity, session string
	unavailable                                     error
	lifetime                                        context.Context
	executionScope                                  string
	desktopSessionID                                string
	remoteInput                                     bool
	agentID                                         string
}

type claudeDesktopATISAssignment struct {
	ExperimentKey string `json:"experiment_key,omitempty"`
	ATIS          string `json:"atis,omitempty"`
	FetchedAt     string `json:"fetched_at"`
}

// claudeDesktopATISClient carries only the non-secret first-party Code
// identity that the bootstrap endpoint observes. It is request-scoped so a
// current Desktop request never has to inherit the stale identity of an older
// emulation profile.
type claudeDesktopATISClient struct {
	CodeVersion    string
	ClientPlatform string
	ClientVersion  string
}

func (m *claudeDesktopATISManager) defaultClient() claudeDesktopATISClient {
	if m == nil {
		return claudeDesktopATISClient{}
	}
	return m.client.normalized(m.codeVersion)
}

func (c claudeDesktopATISClient) normalized(fallbackCodeVersion string) claudeDesktopATISClient {
	c.CodeVersion = strings.TrimSpace(c.CodeVersion)
	if c.CodeVersion == "" {
		c.CodeVersion = strings.TrimSpace(fallbackCodeVersion)
	}
	c.ClientPlatform = strings.TrimSpace(c.ClientPlatform)
	c.ClientVersion = strings.TrimSpace(c.ClientVersion)
	return c
}

func claudeDesktopATISClientFromHeaders(headers http.Header, fallbackCodeVersion string) claudeDesktopATISClient {
	client := claudeDesktopATISClient{
		ClientPlatform: claudeDesktopHeaderValue(headers, "anthropic-client-platform"),
		ClientVersion:  claudeDesktopHeaderValue(headers, "anthropic-client-version"),
	}
	client.CodeVersion = claudeDesktopCodeVersionFromUserAgent(claudeDesktopHeaderValue(headers, "user-agent"))
	return client.normalized(fallbackCodeVersion)
}

func claudeDesktopCodeVersionFromUserAgent(userAgent string) string {
	userAgent = strings.ToLower(strings.TrimSpace(userAgent))
	for _, prefix := range []string{"claude-code/", "claude-cli/"} {
		marker := strings.Index(userAgent, prefix)
		if marker < 0 {
			continue
		}
		var version string
		for _, character := range userAgent[marker+len(prefix):] {
			if (character < '0' || character > '9') && character != '.' {
				break
			}
			version += string(character)
		}
		if version != "" {
			return version
		}
	}
	return ""
}

// claudeDesktopATISClientFromProfile selects the program-owned identity of the
// native bootstrap sender. Caller request headers are deliberately not an
// input: the capture profile is the sole authority for this auxiliary request.
func claudeDesktopATISClientFromProfile(bundle *claudeprofile.Bundle) claudeDesktopATISClient {
	if bundle == nil {
		return claudeDesktopATISClient{}
	}
	client := claudeDesktopATISClient{CodeVersion: bundle.CodeVersion}
	for _, endpoint := range bundle.Startup.Endpoints {
		if endpoint.EndpointRole != "startup-bootstrap" {
			continue
		}
		profile, ok := bundle.Startup.HeaderProfiles[endpoint.HeaderProfile]
		if !ok {
			break
		}
		for _, header := range profile.Headers {
			switch {
			case strings.EqualFold(header.Name, "User-Agent"):
				if version := claudeDesktopCodeVersionFromUserAgent(header.Value); version != "" {
					client.CodeVersion = version
				}
			case strings.EqualFold(header.Name, "anthropic-client-platform"):
				client.ClientPlatform = header.Value
			case strings.EqualFold(header.Name, "anthropic-client-version"):
				client.ClientVersion = header.Value
			}
		}
		break
	}
	return client.normalized(bundle.CodeVersion)
}

type claudeDesktopATISSessionBinding struct {
	// Model follows the owned main configuration, independently of the pin
	// fixed at initialization. Auxiliary wire models never replace it.
	Model     string `json:"model"`
	ATIS      string `json:"atis"`
	UpdatedAt string `json:"updated_at"`
}

type claudeDesktopATISState struct {
	Version      int                                        `json:"version"`
	IdentityHash string                                     `json:"identity_hash"`
	Assignments  map[string]claudeDesktopATISAssignment     `json:"assignments"`
	Sessions     map[string]claudeDesktopATISSessionBinding `json:"sessions"`
	Features     map[string]json.RawMessage                 `json:"features,omitempty"`
}

type claudeDesktopATISProtectedState struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext string `json:"ciphertext"`
}

type claudeDesktopATISBootstrapResponse struct {
	ClientData struct {
		ExperimentKey json.RawMessage `json:"experimentKey"`
		ATIS          json.RawMessage `json:"atis"`
	} `json:"client_data"`
	OAuthAccount struct {
		AccountUUID      string `json:"account_uuid"`
		OrganizationUUID string `json:"organization_uuid"`
	} `json:"oauth_account"`
}

func newClaudeDesktopATISManager(statePath, codeVersion string, doerFactory claudeDesktopATISDoerFactory, stores ...claudefeatures.Store) *claudeDesktopATISManager {
	var store claudefeatures.Store
	var aliases claudefeatures.Store
	var records claudefeatures.Store
	if strings.TrimSpace(statePath) != "" {
		store = helps.NewClaudeDesktopFeatureStore(statePath, "")
		aliases = helps.NewClaudeDesktopSessionAliasStore(statePath, "")
		records = helps.NewClaudeDesktopSessionRecordStore(statePath, "")
	}
	if len(stores) > 0 {
		store = stores[0]
	}
	if len(stores) > 1 {
		aliases = stores[1]
	}
	if len(stores) > 2 {
		records = stores[2]
	}
	return &claudeDesktopATISManager{
		statePath:      strings.TrimSpace(statePath),
		codeVersion:    strings.TrimSpace(codeVersion),
		client:         claudeDesktopATISClient{CodeVersion: strings.TrimSpace(codeVersion)},
		doerFactory:    doerFactory,
		now:            time.Now,
		assignments:    make(map[string]claudeDesktopATISAssignment),
		sessions:       make(map[string]claudeDesktopATISSessionBinding),
		featureHosts:   claudefeatures.NewRegistry(store, aliases),
		desktopRecords: claudesessions.NewRegistry(records),
		featureStarted: make(map[*claudefeatures.Host]bool),
	}
}

// resolveClaudeDesktopSession runs before lineage, prompt/history binding and
// body rendering. Internal helpers already carry a scoped native identity and
// must never pass it through the downstream alias map again.
func (e *ClaudeExecutor) resolveClaudeDesktopSession(ctx context.Context, auth *cliproxyauth.Auth, caller string, role claudeprofile.RequestRole) string {
	session, _, _ := e.resolveClaudeDesktopSessionOwner(ctx, auth, caller, role)
	return session
}

func (e *ClaudeExecutor) bindClaudeDesktopQueryContext(ctx context.Context, auth *cliproxyauth.Auth, caller string, role claudeprofile.RequestRole, metadata ...map[string]any) (context.Context, string) {
	if e.desktopOnly {
		bound, err := e.desktopExecutionSessions.Bind(ctx, metadata...)
		ctx = bound
		if err != nil {
			// A retired/foreign execution cannot claim a query or revive the
			// prewarm host. Keep cancellation even for a detached late helper.
			lifetime, cancel := context.WithCancel(context.Background())
			cancel()
			owner := claudeDesktopQueryContext{manager: e.desktopATIS, unavailable: err, lifetime: lifetime}
			return context.WithValue(ctx, claudeDesktopQueryContextKey{}, owner), caller
		}
	}
	session, host, ownerErr := e.resolveClaudeDesktopSessionOwner(ctx, auth, caller, role)
	if !e.desktopOnly || e.desktopProfile == nil {
		return ctx, session
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owner := claudeDesktopQueryContext{manager: e.desktopATIS, unavailable: ownerErr}
	if host != nil && auth != nil {
		if enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata); err == nil {
			owner = claudeDesktopQueryContext{manager: e.desktopATIS, host: host, accountID: auth.ID,
				profileID: e.desktopProfile.ProfileID, egress: auth.ProxyURL,
				identity: claudeDesktopATISIdentityHash(enrollment), session: session, lifetime: host.Context(),
				executionScope:   e.desktopExecutionSessions.Scope(ctx, ""),
				desktopSessionID: e.desktopATIS.desktopRecords.ByHost(host).ID}
			inherited, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
			owner.remoteInput = inherited.remoteInput && inherited.manager == owner.manager && inherited.host == host
			if owner.remoteInput {
				owner.agentID = inherited.agentID
			}
			if host.Context().Err() != nil {
				owner.host, owner.unavailable = nil, errClaudeDesktopQueryOwnerUnavailable
			}
		}
	}
	if owner.host == nil && errors.Is(ownerErr, errClaudeDesktopQueryOwnerUnavailable) && auth != nil {
		inherited, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata); err == nil &&
			inherited.manager == e.desktopATIS && inherited.accountID == auth.ID && inherited.profileID == e.desktopProfile.ProfileID &&
			inherited.egress == auth.ProxyURL && inherited.identity == claudeDesktopATISIdentityHash(enrollment) && inherited.session == session && inherited.lifetime != nil && inherited.executionScope == e.desktopExecutionSessions.Scope(ctx, "") {
			// A detached late helper retains cancellation, never feature access,
			// to its retired query. Ambiguity and foreign bindings have no such
			// authority and continue through the existing bootstrap-only path.
			owner = inherited
			owner.host, owner.unavailable = nil, ownerErr
		}
	}
	// An unresolved request shadows any inherited foreign/stale handle. It
	// must not select that handle later merely because a session label matches.
	return context.WithValue(ctx, claudeDesktopQueryContextKey{}, owner), session
}

func (e *ClaudeExecutor) beginClaudeDesktopQueryLifetime(ctx context.Context) (context.Context, func()) {
	releaseExecution := func() {}
	if e.desktopOnly {
		ctx, releaseExecution = helps.BindClaudeDesktopQueryLifetime(ctx, e.desktopExecutionSessions.Lifetime(ctx))
	}
	if ctx != nil && e.desktopOnly {
		owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if owner.manager == e.desktopATIS && owner.lifetime != nil {
			bound, releaseQuery := helps.BindClaudeDesktopQueryLifetime(ctx, owner.lifetime)
			return bound, func() { releaseQuery(); releaseExecution() }
		}
	}
	return ctx, releaseExecution
}

// Only an owned main input can claim or drop its provisional query. Helpers
// and token counting neither adopt input nor retire the main query on failure.
func (e *ClaudeExecutor) beginClaudeDesktopInput(ctx context.Context, role claudeprofile.RequestRole) (*claudefeatures.InputLease, error) {
	if !e.desktopOnly || ctx == nil || role != claudeprofile.RoleMain {
		return nil, nil
	}
	owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	if owner.manager == nil || owner.manager != e.desktopATIS || owner.host == nil {
		return nil, nil
	}
	return owner.host.BeginInput(ctx, func() { owner.manager.featureHosts.Retire(owner.host) })
}

func (m *claudeDesktopATISManager) queryHostFromContext(ctx context.Context, auth *cliproxyauth.Auth, identity, session string) *claudefeatures.Host {
	if ctx == nil || m == nil || auth == nil {
		return nil
	}
	owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	if owner.unavailable != nil || owner.manager != m || owner.host == nil || owner.accountID != auth.ID || owner.egress != auth.ProxyURL || owner.identity != identity || owner.session != session || (m.profileID != "" && owner.profileID != m.profileID) {
		return nil
	}
	if owner.host.Context().Err() != nil || owner.host.SessionID() != session {
		return nil
	}
	return owner.host
}

func (e *ClaudeExecutor) resolveClaudeDesktopSessionOwner(ctx context.Context, auth *cliproxyauth.Auth, caller string, role claudeprofile.RequestRole) (string, *claudefeatures.Host, error) {
	if !e.desktopOnly || e.desktopProfile == nil {
		return caller, nil, nil
	}
	m := e.desktopATIS
	if native := helps.ClaudeDesktopSessionUUID(ctx, auth, e.desktopProfile.ProfileID, ""); native != "" {
		if m != nil && auth != nil {
			if enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata); err == nil {
				owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
				if owner.executionScope != e.desktopExecutionSessions.Scope(ctx, "") {
					return native, nil, errClaudeDesktopQueryOwnerUnavailable
				}
				if host := m.queryHostFromContext(ctx, auth, claudeDesktopATISIdentityHash(enrollment), native); host != nil {
					return native, host, nil
				}
				if owner.host != nil || owner.unavailable != nil {
					// A held query cannot silently migrate to a different live
					// query (or process) that happens to share its transcript.
					return native, nil, errClaudeDesktopQueryOwnerUnavailable
				}
				host, err := m.featureHosts.FindSession(native)
				return native, host, err
			}
		}
		return native, nil, nil
	}
	if m == nil || auth == nil || caller == "" {
		return caller, nil, nil
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return caller, nil, nil
	}
	identity := claudeDesktopATISIdentityHash(enrollment)
	loadErr := m.ensureLoaded(identity)
	scopeBytes, _ := json.Marshal([]string{"desktop-sdk-session-alias-v1", auth.ID, e.desktopProfile.ProfileID, auth.ProxyURL, identity, caller})
	digest := sha256.Sum256(scopeBytes)
	scope := hex.EncodeToString(digest[:])
	queryScope := e.desktopExecutionSessions.Scope(ctx, scope)
	var native string
	var host *claudefeatures.Host
	if role == claudeprofile.RoleMain {
		// An existing durable latch proves a pre-alias conversation. Preserve
		// its old native UUID so an upgrade does not strand its transcript.
		legacy := ""
		if loadErr == nil && m.Latch(caller) != nil {
			legacy = caller
		}
		host, _, err = m.desktopRecords.Resolve(ctx, e.claudeDesktopRecordOwner(auth), queryScope, func(resume *string) (*claudefeatures.Host, error) {
			if resume == nil {
				return m.featureHosts.ResolveQuery(queryScope, scope, legacy)
			}
			return m.featureHosts.Main(queryScope, *resume)
		})
		if ctx.Err() != nil {
			return caller, nil, ctx.Err()
		}
		if host != nil {
			native = host.SessionID()
			if !e.desktopExecutionSessions.Own(ctx, host.ID(), host.Context(), func() { m.featureHosts.Retire(host) }) {
				err = errors.Join(err, errClaudeDesktopQueryOwnerUnavailable)
			} else if host != m.featureHosts.Warm() {
				err = errors.Join(err, m.startSDKFeatureHost(host))
			}
		}
	} else {
		native, err = m.featureHosts.QuerySession(queryScope, scope)
		host = m.featureHosts.Lookup(queryScope)
		if queryScope != scope && host == nil {
			// The alias may belong to another connection. Knowing its durable
			// transcript does not authorize this helper to use that query.
			if native == "" {
				native = caller
			}
			return native, nil, errClaudeDesktopQueryOwnerUnavailable
		}
	}
	if m.featureStateObserver != nil {
		// Alias persistence and a feature evaluation are independent facts;
		// an evaluation success must not clear a broken identity checkpoint.
		_ = m.featureStateObserver(auth, "session-alias:"+scope, errors.Join(loadErr, err) == nil)
		if role == claudeprofile.RoleMain {
			_ = m.featureStateObserver(auth, "desktop-session-record:"+queryScope, err == nil)
		}
	}
	if native == "" {
		return caller, nil, nil
	}
	if host != nil && host.Context().Err() != nil {
		// Retain the known cancellation source when retirement races binding.
		// The caller may use its lifetime, but must not publish it as live.
		return native, host, errClaudeDesktopQueryOwnerUnavailable
	}
	return native, host, nil
}

// Assignment reads bootstrap data separately from the conversation latch. Only
// the owned main query initializes a latch, including an explicit negative.
// An internal fork key has its own latch and never implicitly inherits main.
func (m *claudeDesktopATISManager) Assignment(ctx context.Context, auth *cliproxyauth.Auth, sessionID, logicalModel, wireModel string, role claudeprofile.RequestRole, forkID ...string) (string, error) {
	return m.assignment(ctx, auth, sessionID, logicalModel, wireModel, role, m.defaultClient(), forkID...)
}

func (m *claudeDesktopATISManager) AssignmentForClient(ctx context.Context, auth *cliproxyauth.Auth, sessionID, logicalModel, wireModel string, role claudeprofile.RequestRole, client claudeDesktopATISClient, forkID ...string) (string, error) {
	return m.assignment(ctx, auth, sessionID, logicalModel, wireModel, role, client.normalized(m.codeVersion), forkID...)
}

func (m *claudeDesktopATISManager) assignment(ctx context.Context, auth *cliproxyauth.Auth, sessionID, logicalModel, wireModel string, role claudeprofile.RequestRole, client claudeDesktopATISClient, forkID ...string) (string, error) {
	if m == nil || auth == nil {
		return "", nil
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return "", fmt.Errorf("validate Claude Desktop ATIS account binding: %w", errEnrollment)
	}
	identityHash := claudeDesktopATISIdentityHash(enrollment)
	loadErr := m.ensureLoaded(identityHash)

	sessionKey := claudeDesktopATISScopeKey(sessionID, forkID...)
	model := normalizeClaudeDesktopBootstrapModel(logicalModel)
	if model == "" {
		model = normalizeClaudeDesktopBootstrapModel(wireModel)
	}
	authoritative := role == claudeprofile.RoleMain
	var hostErr error
	host := m.queryHostFromContext(ctx, auth, identityHash, sessionID)
	if ctx != nil && host == nil {
		owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if owner.unavailable != nil || owner.host != nil {
			hostErr = owner.unavailable
			if hostErr == nil {
				hostErr = errClaudeDesktopQueryOwnerUnavailable
			}
		}
	}
	if len(forkID) > 0 && forkID[0] != "" {
		host = m.featureHosts.Lookup(sessionKey)
		hostErr = nil
		if host != nil && host.Context().Err() != nil {
			host = nil
		}
	} else if host == nil && hostErr == nil {
		host, hostErr = m.featureHosts.FindSession(sessionID)
	}
	if authoritative && sessionKey != "" {
		if host == nil && hostErr == nil {
			host, hostErr = m.featureHosts.Main(sessionKey, sessionID)
		}
		if hostErr == nil && host != nil && host != m.featureHosts.Warm() {
			hostErr = m.startSDKFeatureHost(host)
		}
	}
	canOwnLatch := authoritative && host != nil && host.Context().Err() == nil
	readGate := func() (bool, error) {
		if host != nil {
			value, err := m.featureValueOnHost(auth, host, "tengu_kestrel_moor", json.RawMessage(`true`))
			return claudefeatures.Truthy(value.Raw), err
		}
		if hostErr != nil {
			var errHealth error
			if m.featureStateObserver != nil {
				errHealth = m.featureStateObserver(auth, "query-owner:"+sessionKey, false)
			}
			// Keep inference usable with bootstrap data, but neither consume
			// another query's gate nor invent a third query for its exposure.
			return false, errors.Join(hostErr, errHealth)
		}
		return m.featureEnabled(auth, sessionID, sessionKey)
	}

	m.mu.Lock()
	if m.identityHash != identityHash {
		m.mu.Unlock()
		return "", fmt.Errorf("Claude Desktop ATIS account binding changed during resolution")
	}
	binding, hasBinding := m.sessions[sessionKey]
	if hasBinding && canOwnLatch && model != "" && binding.Model != model {
		binding.Model, binding.UpdatedAt = model, m.now().UTC().Format(time.RFC3339Nano)
		m.sessions[sessionKey] = binding
		_ = m.persistLocked()
	}
	if hasBinding && !authoritative {
		model = binding.Model
	}
	stateErr := errors.Join(loadErr, m.persistErr, hostErr)
	m.mu.Unlock()
	// An initialized latch needs one effective-header feature read. A new main
	// first establishes its latch and only then reads the gate, matching native
	// initialization rather than retrying the exposure twice on one request.
	gateRead, latchEnabled := false, false
	if hasBinding {
		var errGate error
		latchEnabled, errGate = readGate()
		gateRead = true
		stateErr = errors.Join(stateErr, errGate)
		if latchEnabled {
			return binding.ATIS, stateErr
		}
	}
	if model == "" {
		return "", stateErr
	}

	assignment, errAssignment := m.assignmentForModel(ctx, auth, enrollment, model, client)
	m.mu.Lock()
	if m.identityHash != identityHash {
		m.mu.Unlock()
		return "", fmt.Errorf("Claude Desktop ATIS account binding changed during resolution")
	}
	// The check and initialization are atomic: a slower bootstrap for another
	// model cannot overwrite either a positive or a negative conversation pin.
	binding, hasBinding = m.sessions[sessionKey]
	var persistErr error
	if sessionKey != "" && !hasBinding && canOwnLatch {
		pin := assignment.ATIS
		if !claudeDesktopATISPrintable(pin) {
			pin = ""
		}
		binding = claudeDesktopATISSessionBinding{
			Model:     model,
			ATIS:      pin,
			UpdatedAt: m.now().UTC().Format(time.RFC3339Nano),
		}
		m.sessions[sessionKey], hasBinding = binding, true
		persistErr = m.persistLocked()
	}
	m.mu.Unlock()
	if !gateRead {
		var errGate error
		latchEnabled, errGate = readGate()
		stateErr = errors.Join(stateErr, errGate)
	}
	if latchEnabled && hasBinding {
		return binding.ATIS, errors.Join(stateErr, errAssignment, persistErr)
	}
	return assignment.ATIS, errors.Join(stateErr, errAssignment, persistErr)
}

func (m *claudeDesktopATISManager) assignmentForModel(ctx context.Context, auth *cliproxyauth.Auth, enrollment claudedesktop.Enrollment, model string, client claudeDesktopATISClient) (claudeDesktopATISAssignment, error) {
	identityHash := claudeDesktopATISIdentityHash(enrollment)
	m.mu.Lock()
	if m.identityHash != identityHash {
		m.mu.Unlock()
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap account binding changed")
	}
	if assignment, ok := m.assignments[model]; ok {
		err := m.persistErr
		m.mu.Unlock()
		return assignment, err
	}
	m.mu.Unlock()

	value, errFetch, _ := m.requestGroup.Do(identityHash+"/"+model, func() (any, error) {
		m.mu.Lock()
		if m.identityHash != identityHash {
			m.mu.Unlock()
			return nil, fmt.Errorf("Claude Desktop ATIS bootstrap account binding changed")
		}
		if assignment, ok := m.assignments[model]; ok {
			err := m.persistErr
			m.mu.Unlock()
			return assignment, err
		}
		m.mu.Unlock()

		assignment, errBootstrap := m.fetch(ctx, auth, enrollment, model, client)
		if errBootstrap != nil {
			return claudeDesktopATISAssignment{}, errBootstrap
		}
		m.mu.Lock()
		if m.identityHash != identityHash {
			m.mu.Unlock()
			return nil, fmt.Errorf("Claude Desktop ATIS bootstrap account binding changed")
		}
		m.assignments[model] = assignment
		errPersist := m.persistLocked()
		m.mu.Unlock()
		return assignment, errPersist
	})
	assignment, ok := value.(claudeDesktopATISAssignment)
	if !ok {
		if errFetch != nil {
			return claudeDesktopATISAssignment{}, errFetch
		}
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap returned an invalid internal result")
	}
	return assignment, errFetch
}

func (m *claudeDesktopATISManager) fetch(ctx context.Context, auth *cliproxyauth.Auth, enrollment claudedesktop.Enrollment, model string, client claudeDesktopATISClient) (claudeDesktopATISAssignment, error) {
	if m.doerFactory == nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap transport is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, claudeDesktopATISTimeout)
	defer cancel()

	endpoint := &url.URL{
		Scheme: "https",
		Host:   "api.anthropic.com",
		Path:   "/api/claude_cli/bootstrap",
	}
	query := endpoint.Query()
	query.Set("entrypoint", "claude-desktop")
	query.Set("model", model)
	endpoint.RawQuery = query.Encode()
	request, errRequest := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if errRequest != nil {
		return claudeDesktopATISAssignment{}, errRequest
	}
	accessToken, _ := claudeCreds(auth)
	if accessToken = strings.TrimSpace(accessToken); accessToken == "" {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap has no OAuth access token")
	}
	client = client.normalized(m.codeVersion)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "claude-code/"+client.CodeVersion)
	if client.ClientPlatform != "" {
		request.Header.Set("anthropic-client-platform", client.ClientPlatform)
	}
	if client.ClientVersion != "" {
		request.Header.Set("anthropic-client-version", client.ClientVersion)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	request.Header.Set("Connection", "close")
	request.Close = true

	doer, errDoer := m.doerFactory(requestContext, auth)
	if errDoer != nil {
		return claudeDesktopATISAssignment{}, errDoer
	}
	response, errDo := doer.Do(request)
	if errDo != nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap request: %w", errDo)
	}
	if response == nil || response.Body == nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap returned no response body")
	}
	decodedBody, errDecode := decodeResponseBody(response.Body, claudeResponseContentEncoding(response.Header))
	if errDecode != nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("decode Claude Desktop ATIS bootstrap response: %w", errDecode)
	}
	defer decodedBody.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap returned HTTP %d", response.StatusCode)
	}
	payload, errRead := io.ReadAll(io.LimitReader(decodedBody, claudeDesktopATISMaxBodyBytes+1))
	if errRead != nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("read Claude Desktop ATIS bootstrap response: %w", errRead)
	}
	if len(payload) > claudeDesktopATISMaxBodyBytes {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap response exceeds size limit")
	}
	var bootstrap claudeDesktopATISBootstrapResponse
	if errJSON := json.Unmarshal(payload, &bootstrap); errJSON != nil {
		return claudeDesktopATISAssignment{}, fmt.Errorf("parse Claude Desktop ATIS bootstrap response: %w", errJSON)
	}
	if !strings.EqualFold(strings.TrimSpace(bootstrap.OAuthAccount.AccountUUID), enrollment.AccountUUID) ||
		!strings.EqualFold(strings.TrimSpace(bootstrap.OAuthAccount.OrganizationUUID), enrollment.OrganizationUUID) {
		return claudeDesktopATISAssignment{}, fmt.Errorf("Claude Desktop ATIS bootstrap account binding does not match the enrolled credential")
	}
	var experimentKey, atis string
	_ = json.Unmarshal(bootstrap.ClientData.ExperimentKey, &experimentKey)
	_ = json.Unmarshal(bootstrap.ClientData.ATIS, &atis)
	return claudeDesktopATISAssignment{
		ExperimentKey: experimentKey,
		ATIS:          atis,
		FetchedAt:     m.now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func (m *claudeDesktopATISManager) ensureLoaded(identityHash string) (loadErr error) {
	m.mu.Lock()
	defer func() {
		if loadErr != nil {
			m.loadErr = loadErr
		}
		m.mu.Unlock()
	}()
	if m.loaded {
		if m.identityHash != identityHash {
			m.identityHash = identityHash
			m.assignments = make(map[string]claudeDesktopATISAssignment)
			m.sessions = make(map[string]claudeDesktopATISSessionBinding)
			m.legacyFeatures = nil
			m.loadErr, m.persistErr = nil, nil
			return m.persistLocked()
		}
		return m.loadErr
	}
	m.loaded = true
	m.identityHash = identityHash
	path := m.filePath()
	if path == "" {
		return nil
	}
	encoded, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read Claude Desktop ATIS state: %w", errRead)
	}
	var protected claudeDesktopATISProtectedState
	if errJSON := json.Unmarshal(encoded, &protected); errJSON != nil || (protected.Version != 1 && protected.Version != claudeDesktopATISStateVersion) {
		return fmt.Errorf("parse Claude Desktop ATIS state envelope")
	}
	ciphertext, errBase64 := base64.StdEncoding.DecodeString(protected.Ciphertext)
	if errBase64 != nil {
		return fmt.Errorf("decode Claude Desktop ATIS state: %w", errBase64)
	}
	plaintext, errUnprotect := claudedesktop.UnprotectRuntimePayload(path, protected.Protector, ciphertext)
	if errUnprotect != nil {
		return fmt.Errorf("unprotect Claude Desktop ATIS state: %w", errUnprotect)
	}
	var state claudeDesktopATISState
	if errJSON := json.Unmarshal(plaintext, &state); errJSON != nil || state.Version != protected.Version {
		return fmt.Errorf("parse Claude Desktop ATIS state")
	}
	if state.IdentityHash != identityHash {
		return m.persistLocked()
	}
	for model, assignment := range state.Assignments {
		model = normalizeClaudeDesktopBootstrapModel(model)
		if model == "" {
			continue
		}
		m.assignments[model] = assignment
	}
	if state.Version == 1 {
		// Legacy model bindings did not prove native latch initialization. Keep
		// their bootstrap cache, but never silently turn them into native facts.
		return fmt.Errorf("legacy Claude Desktop ATIS conversation state is unverified")
	}
	for sessionKey, binding := range state.Sessions {
		key, errKey := hex.DecodeString(sessionKey)
		if errKey != nil || len(key) != sha256.Size || normalizeClaudeDesktopBootstrapModel(binding.Model) == "" || (binding.ATIS != "" && !claudeDesktopATISPrintable(binding.ATIS)) {
			continue
		}
		m.sessions[sessionKey] = binding
	}
	m.legacyFeatures = state.Features
	return nil
}

func (m *claudeDesktopATISManager) persistLocked() (persistErr error) {
	defer func() { m.persistErr = persistErr }()
	path := m.filePath()
	if path == "" || m.identityHash == "" {
		return nil
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop ATIS state directory: %w", errMkdir)
	}
	state := claudeDesktopATISState{
		Version:      claudeDesktopATISStateVersion,
		IdentityHash: m.identityHash,
		Assignments:  m.assignments,
		Sessions:     m.sessions,
		Features:     m.legacyFeatures,
	}
	plaintext, errJSON := json.Marshal(state)
	if errJSON != nil {
		return errJSON
	}
	protector, ciphertext, errProtect := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if errProtect != nil {
		return fmt.Errorf("protect Claude Desktop ATIS state: %w", errProtect)
	}
	encoded, errEnvelope := json.Marshal(claudeDesktopATISProtectedState{
		Version:    claudeDesktopATISStateVersion,
		Protector:  protector,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if errEnvelope != nil {
		return errEnvelope
	}
	if errWrite := helps.AtomicWriteFile(path, encoded, 0o600); errWrite != nil {
		return fmt.Errorf("write Claude Desktop ATIS state: %w", errWrite)
	}
	return nil
}

func (m *claudeDesktopATISManager) filePath() string {
	if m == nil || strings.TrimSpace(m.statePath) == "" {
		return ""
	}
	return filepath.Join(m.statePath, "atis", "bootstrap-state.json")
}

func normalizeClaudeDesktopBootstrapModel(model string) string {
	// Request planning already resolves the supported logical model. Native
	// bootstrap uses that configured model, not a second three-model allowlist.
	return strings.ToLower(strings.TrimSpace(model))
}

func claudeDesktopATISIdentityHash(enrollment claudedesktop.Enrollment) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"claude-desktop-atis-account-v1",
		strings.ToLower(strings.TrimSpace(enrollment.AccountUUID)),
		strings.ToLower(strings.TrimSpace(enrollment.OrganizationUUID)),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func claudeDesktopATISSessionKey(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("claude-desktop-atis-session-v1\x00" + sessionID))
	return hex.EncodeToString(digest[:])
}

func claudeDesktopATISScopeKey(sessionID string, forkID ...string) string {
	if len(forkID) == 0 || forkID[0] == "" {
		return claudeDesktopATISSessionKey(sessionID)
	}
	if sessionID == "" {
		return ""
	}
	encoded, _ := json.Marshal([]string{"claude-desktop-atis-fork-v1", sessionID, forkID[0]})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func claudeDesktopATISPrintable(pin string) bool {
	if pin == "" {
		return false
	}
	for i := range pin {
		if pin[i] < 0x21 || pin[i] > 0x7e {
			return false
		}
	}
	return true
}

// Latch returns an owned snapshot, preserving undefined versus explicit empty.
// It does not initialize, consult the current model, or inspect wire headers.
func (m *claudeDesktopATISManager) Latch(sessionID string, forkID ...string) *string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding, ok := m.sessions[claudeDesktopATISScopeKey(sessionID, forkID...)]; ok {
		pin := binding.ATIS
		return &pin
	}
	return nil
}

func claudeDesktopATISFeatureEnabled(features map[string]json.RawMessage) bool {
	raw := features["tengu_kestrel_moor"]
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" || !json.Valid(raw) {
		return true
	}
	return claudefeatures.Truthy(raw)
}

func (m *claudeDesktopATISManager) bindFeatures(auth *cliproxyauth.Auth) (claudefeatures.Ticket, claudefeatures.Sink, error) {
	if m == nil {
		return claudefeatures.Ticket{}, nil, fmt.Errorf("Claude Desktop feature host is unavailable")
	}
	return m.bindFeatureHost(auth, m.featureHosts.Warm())
}

func (m *claudeDesktopATISManager) bindFeatureHost(auth *cliproxyauth.Auth, host *claudefeatures.Host) (claudefeatures.Ticket, claudefeatures.Sink, error) {
	if m == nil || auth == nil || host == nil {
		return claudefeatures.Ticket{}, nil, fmt.Errorf("Claude Desktop feature host is unavailable")
	}
	m.featureBindMu.Lock()
	defer m.featureBindMu.Unlock()
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return claudefeatures.Ticket{}, nil, fmt.Errorf("validate Claude Desktop feature account binding")
	}
	identity := claudeDesktopATISIdentityHash(enrollment)
	loadErr := m.ensureLoaded(identity)
	token, _ := claudeCreds(auth)
	revision := sha256.Sum256([]byte(token))
	ticket, errBind := host.Service().Bind(identity, hex.EncodeToString(revision[:]))
	m.mu.Lock()
	host.Service().ImportLegacy(ticket, m.legacyFeatures)
	m.mu.Unlock()
	ownedAuth := auth.Clone()
	var sink claudefeatures.Sink
	if m.featureSink != nil {
		sink = func(exposure claudefeatures.Exposure) (bool, error) { return m.featureSink(ownedAuth, exposure) }
	}
	var errHealth error
	if !errors.Is(errBind, claudefeatures.ErrStale) {
		errHealth = m.observeFeaturePersistence(ownedAuth, host, ticket, false)
	}
	return ticket, sink, errors.Join(loadErr, errBind, errHealth)
}

func (m *claudeDesktopATISManager) featureEnabled(auth *cliproxyauth.Auth, session string, scope ...string) (bool, error) {
	key := claudeDesktopATISScopeKey(session)
	if len(scope) > 0 {
		key = scope[0]
	}
	host := m.featureHosts.Lookup(key)
	if host == nil && key == claudeDesktopATISScopeKey(session) {
		host = m.featureHosts.LookupSession(session)
	}
	if host == nil {
		warm := m.featureHosts.Warm()
		ticket, _, err := m.bindFeatureHost(auth, warm)
		value, errRead := warm.Service().Peek(ticket, "tengu_kestrel_moor", json.RawMessage(`true`))
		if m.featureStateObserver != nil {
			err = errors.Join(err, m.featureStateObserver(auth, session, false))
		}
		return claudefeatures.Truthy(value.Raw), errors.Join(err, errRead)
	}
	value, err := m.featureValueOnHost(auth, host, "tengu_kestrel_moor", json.RawMessage(`true`))
	return claudefeatures.Truthy(value.Raw), err
}

func (m *claudeDesktopATISManager) featureValueOnHost(auth *cliproxyauth.Auth, host *claudefeatures.Host, name string, fallback json.RawMessage) (claudefeatures.Value, error) {
	ticket, sink, errBind := m.bindFeatureHost(auth, host)
	m.featureBindMu.Lock()
	defer m.featureBindMu.Unlock()
	value, errRead := host.Service().Lookup(ticket, host.SessionID(), name, fallback, sink)
	err := errors.Join(errBind, errRead)
	if err != nil && !errors.Is(errRead, claudefeatures.ErrStale) {
		// A successful cached read is not recovery of a failed refresh. Only an
		// actual healthy observation may clear the durable feature-state issue.
		err = errors.Join(err, m.observeFeatureHostHealth(auth, host, false))
	}
	return value, err
}

func (m *claudeDesktopATISManager) FeatureRefreshCadence(auth *cliproxyauth.Auth) (time.Duration, bool) {
	if m == nil {
		return 6 * time.Hour, false
	}
	return m.featureRefreshCadenceForHost(auth, m.featureHosts.Warm())
}

func (m *claudeDesktopATISManager) featureRefreshCadenceForHost(auth *cliproxyauth.Auth, host *claudefeatures.Host) (time.Duration, bool) {
	value, _ := m.featureValueOnHost(auth, host, "tengu_gb_refresh_interval_minutes", json.RawMessage(`null`))
	return claudefeatures.RefreshCadence(value.Raw, mathrand.Float64())
}

func (m *claudeDesktopATISManager) FeatureAuthedEvaluation(auth *cliproxyauth.Auth) bool {
	if m == nil {
		return false
	}
	return m.featureAuthedEvaluationForHost(auth, m.featureHosts.Warm())
}

func (m *claudeDesktopATISManager) featureAuthedEvaluationForHost(auth *cliproxyauth.Auth, host *claudefeatures.Host) bool {
	value, _ := m.featureValueOnHost(auth, host, "tengu_gb_eval_authed_enable", json.RawMessage(`false`))
	return claudefeatures.Truthy(value.Raw)
}

// PrepareSDKFeatures captures the dispatch's auth generation before transport.
// Its completion cannot relabel an old response with a rotated credential.
func (m *claudeDesktopATISManager) PrepareSDKFeatures(auth *cliproxyauth.Auth) (string, func([]byte) error, error) {
	if m == nil {
		return "", nil, nil
	}
	return m.prepareSDKFeatureHost(auth, m.featureHosts.Warm())
}

func (m *claudeDesktopATISManager) prepareSDKFeatureHost(auth *cliproxyauth.Auth, host *claudefeatures.Host) (string, func([]byte) error, error) {
	if auth == nil {
		return "", nil, fmt.Errorf("Claude Desktop feature auth is unavailable")
	}
	ticket, sink, errBind := m.bindFeatureHost(auth, host)
	if errors.Is(errBind, claudefeatures.ErrStale) {
		return "", nil, errBind
	}
	ownedAuth := auth.Clone()
	if errBind != nil {
		_ = m.observeFeatureHostHealth(ownedAuth, host, false)
	}
	return host.SessionID(), func(payload []byte) error {
		// Publication and its diagnostic outcome share the auth-binding order.
		// A later token generation must not be cleared by an older completion.
		m.featureBindMu.Lock()
		defer m.featureBindMu.Unlock()
		accepted, err := host.Service().Observe(ticket, payload, host.SessionID(), sink)
		if errors.Is(err, claudefeatures.ErrStale) {
			return err
		}
		err = errors.Join(err, m.observeFeaturePersistence(ownedAuth, host, ticket, accepted))
		err = errors.Join(err, m.observeFeatureHostHealth(ownedAuth, host, err == nil))
		return err
	}, nil
}

// Shared cache health has a durable identity; query health does not. Closing
// a query must not erase a corrupt cache or an unpersisted experiment exposure.
func (m *claudeDesktopATISManager) observeFeaturePersistence(auth *cliproxyauth.Auth, host *claudefeatures.Host, ticket claudefeatures.Ticket, accepted bool) error {
	if m.featureStateObserver == nil {
		return nil
	}
	errState := host.Service().PersistenceError(ticket)
	if errors.Is(errState, claudefeatures.ErrStale) {
		return nil
	}
	m.mu.Lock()
	errState = errors.Join(errState, m.loadErr)
	m.mu.Unlock()
	if errState == nil && !accepted {
		return nil
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return err
	}
	return m.featureStateObserver(auth, "feature-cache:"+claudeDesktopATISIdentityHash(enrollment), errState == nil)
}

// One serialized observer/retirement pair belongs to a live host and account
// binding. Token rotation and session reidentity do not allocate another pair.
func (m *claudeDesktopATISManager) observeFeatureHostHealth(auth *cliproxyauth.Auth, host *claudefeatures.Host, healthy bool) error {
	m.featureHealthMu.Lock()
	defer m.featureHealthMu.Unlock()
	if m.featureHealthClosed || host.Context().Err() != nil {
		return nil
	}
	owner := "sdk-query:" + host.ID()
	if m.featureHostObserverFactory == nil {
		if m.featureStateObserver != nil {
			return m.featureStateObserver(auth, owner, healthy)
		}
		return nil
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal([]string{auth.ID, auth.ProxyURL, claudeDesktopATISIdentityHash(enrollment)})
	key := string(encoded)
	if m.featureHealth == nil {
		m.featureHealth = make(map[*claudefeatures.Host]map[string]claudeDesktopFeatureHealth)
	}
	bindings := m.featureHealth[host]
	if bindings == nil {
		bindings = make(map[string]claudeDesktopFeatureHealth)
		m.featureHealth[host] = bindings
		context.AfterFunc(host.Context(), func() { m.retireFeatureHostHealth(host) })
	}
	binding, exists := bindings[key]
	if !exists {
		observe, retire, err := m.featureHostObserverFactory(auth, owner)
		if err != nil {
			return err
		}
		if observe == nil || retire == nil {
			return fmt.Errorf("Claude Desktop SDK health callbacks are unavailable")
		}
		binding = claudeDesktopFeatureHealth{observe: observe, retire: retire}
		bindings[key] = binding
	}
	return binding.observe(healthy)
}

func (m *claudeDesktopATISManager) retireFeatureHostHealth(host *claudefeatures.Host) {
	m.featureHealthMu.Lock()
	defer m.featureHealthMu.Unlock()
	for _, binding := range m.featureHealth[host] {
		binding.retire()
	}
	delete(m.featureHealth, host)
}

// Immediate observation is kept for local producers/tests; asynchronous HTTP
// evaluation must use PrepareSDKFeatures at dispatch time instead.
func (m *claudeDesktopATISManager) ObserveSDKFeatures(auth *cliproxyauth.Auth, payload []byte) error {
	_, observe, err := m.PrepareSDKFeatures(auth)
	if err != nil || observe == nil {
		return err
	}
	return observe(payload)
}

func (m *claudeDesktopATISManager) Close() {
	if m != nil {
		m.featureHealthMu.Lock()
		m.featureHealthClosed = true
		m.featureHealthMu.Unlock()
		m.featureHosts.Close()
		// Close drains health retirement before the telemetry manager stops.
		m.featureHealthMu.Lock()
		defer m.featureHealthMu.Unlock()
		for host, bindings := range m.featureHealth {
			for _, binding := range bindings {
				binding.retire()
			}
			delete(m.featureHealth, host)
		}
	}
}

func (m *claudeDesktopATISManager) FeatureContext() context.Context {
	if m == nil {
		return nil
	}
	return m.featureHosts.Warm().Context()
}

func (m *claudeDesktopATISManager) startSDKFeatureHost(host *claudefeatures.Host) error {
	m.featureStartMu.Lock()
	defer m.featureStartMu.Unlock()
	if m.featureStarted[host] || m.startFeatureHost == nil {
		return nil
	}
	if err := m.startFeatureHost(host); err != nil {
		return err
	}
	m.featureStarted[host] = true
	context.AfterFunc(host.Context(), func() {
		m.featureStartMu.Lock()
		delete(m.featureStarted, host)
		m.featureStartMu.Unlock()
	})
	return nil
}
