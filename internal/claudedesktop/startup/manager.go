package startup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	rendererGiB                  = uint64(1 << 30)
	startupCodeSessionsWatchRole = "startup-code-sessions-watch"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type DoerFactory func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error)

// SDKFeaturesObserverFactory binds response ownership before network dispatch.
// The returned session belongs to the SDK host, not the Renderer application.
type SDKFeaturesObserverFactory func(*cliproxyauth.Auth) (string, func([]byte) error, error)

type sdkFeatureObservationKey struct{}

type sdkFeatureObservation struct {
	apply   func([]byte) error
	failure func() error
	authed  bool
}

type Options struct {
	StatePath                  string
	Bundle                     *claudeprofile.Bundle
	ApplicationSessionID       string
	DoerFactory                DoerFactory
	HostSnapshotProvider       claudetelemetry.HostSnapshotProvider
	UpdateCheckObserver        func(*cliproxyauth.Auth, string) error
	SessionsWatchRetryObserver func(*cliproxyauth.Auth, int) error
	SessionsWatchRetryDelay    func(int) time.Duration
	SDKFeaturesObserver        func(*cliproxyauth.Auth, []byte) error
	SDKFeaturesObserverFactory SDKFeaturesObserverFactory
	SDKFeatureRefreshCadence   func(*cliproxyauth.Auth) (time.Duration, bool)
	SDKFeatureAuthedEvaluation func(*cliproxyauth.Auth) bool
	SDKFeatureContext          context.Context
	SDKFeatureFailureObserver  func(*cliproxyauth.Auth) error
	RandomFloat                func() float64
	Now                        func() time.Time
}

// Status is deliberately secret-free. It reports activation coverage without
// exposing endpoint URLs, query values, cookies, tokens, or account identity.
type Status struct {
	Enabled                 bool   `json:"enabled"`
	State                   string `json:"state"`
	EndpointCount           int    `json:"endpoint_count"`
	Attempted               int    `json:"attempted"`
	Completed               int    `json:"completed"`
	Failed                  int    `json:"failed"`
	Skipped                 int    `json:"skipped"`
	LastErrorCategory       string `json:"last_error_category,omitempty"`
	TelemetryErrorCategory  string `json:"telemetry_error_category,omitempty"`
	StartedAt               string `json:"started_at,omitempty"`
	CompletedAt             string `json:"completed_at,omitempty"`
	FeatureRefreshAttempted int    `json:"feature_refresh_attempted"`
	FeatureRefreshCompleted int    `json:"feature_refresh_completed"`
	FeatureRefreshFailed    int    `json:"feature_refresh_failed"`
	FeatureRefreshError     string `json:"feature_refresh_error,omitempty"`
}

type Manager struct {
	root                       string
	bundle                     *claudeprofile.Bundle
	profile                    claudeprofile.StartupProfile
	appSessionID               string
	doerFactory                DoerFactory
	hostSnapshot               claudetelemetry.HostSnapshotProvider
	updateCheckObserver        func(*cliproxyauth.Auth, string) error
	sessionsWatchRetryObserver func(*cliproxyauth.Auth, int) error
	sessionsWatchRetryDelay    func(int) time.Duration
	sdkFeaturesObserver        func(*cliproxyauth.Auth, []byte) error
	sdkFeaturesObserverFactory SDKFeaturesObserverFactory
	sdkFeatureRefreshCadence   func(*cliproxyauth.Auth) (time.Duration, bool)
	sdkFeatureAuthedEvaluation func(*cliproxyauth.Auth) bool
	sdkFeatureContext          context.Context
	sdkFeatureFailureObserver  func(*cliproxyauth.Auth) error
	stopSDKFeatureContext      func() bool
	randomFloat                func() float64
	activationMu               sync.Mutex
	featureLoopOnce            sync.Once
	featureRefreshWake         chan struct{}
	featureHosts               map[*SDKFeatureHost]*sdkFeatureRunner
	featureHostErrors          map[*sdkFeatureRunner]string
	featureDefaultError        string
	authGeneration             uint64
	sdkFetchOK                 uint64
	now                        func() time.Time
	ctx                        context.Context
	cancel                     context.CancelFunc
	activateOnce               sync.Once
	closeOnce                  sync.Once
	wg                         sync.WaitGroup
	persistMu                  sync.Mutex

	mu                sync.Mutex
	status            Status
	auth              *cliproxyauth.Auth
	enrollment        claudedesktop.Enrollment
	accessToken       string
	sessionKey        string
	identity          persistentIdentity
	missingSessionKey bool
}

func NewManager(options Options) *Manager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		root:                       strings.TrimSpace(options.StatePath),
		bundle:                     options.Bundle,
		appSessionID:               strings.TrimSpace(options.ApplicationSessionID),
		doerFactory:                options.DoerFactory,
		hostSnapshot:               options.HostSnapshotProvider,
		updateCheckObserver:        options.UpdateCheckObserver,
		sessionsWatchRetryObserver: options.SessionsWatchRetryObserver,
		sessionsWatchRetryDelay:    options.SessionsWatchRetryDelay,
		sdkFeaturesObserver:        options.SDKFeaturesObserver,
		sdkFeaturesObserverFactory: options.SDKFeaturesObserverFactory,
		sdkFeatureRefreshCadence:   options.SDKFeatureRefreshCadence,
		sdkFeatureAuthedEvaluation: options.SDKFeatureAuthedEvaluation,
		randomFloat:                options.RandomFloat,
		featureRefreshWake:         make(chan struct{}, 1),
		sdkFeatureFailureObserver:  options.SDKFeatureFailureObserver,
		now:                        now,
		ctx:                        ctx,
		cancel:                     cancel,
		status:                     Status{State: "stopped"},
	}
	if manager.randomFloat == nil {
		manager.randomFloat = mathrand.Float64
	}
	if manager.sessionsWatchRetryDelay == nil {
		manager.sessionsWatchRetryDelay = defaultSessionsWatchRetryDelay
	}
	manager.sdkFeatureContext = ctx
	var cancelSDK context.CancelFunc
	if options.SDKFeatureContext != nil {
		manager.sdkFeatureContext, cancelSDK = context.WithCancel(ctx)
	}
	if manager.bundle != nil {
		manager.profile = manager.bundle.Startup
		manager.status.EndpointCount = len(manager.profile.Endpoints)
	}
	manager.status.Enabled = manager.Enabled()
	if cancelSDK != nil {
		manager.stopSDKFeatureContext = context.AfterFunc(options.SDKFeatureContext, func() {
			cancelSDK()
			manager.retireDefaultFeatureHost()
		})
	}
	return manager
}

func (m *Manager) Enabled() bool {
	return m != nil && m.root != "" && m.bundle != nil && m.profile.SchemaVersion == 1 && m.doerFactory != nil
}

// Activate starts the captured application startup set asynchronously. Its
// failures never reject or quarantine inference requests.
func (m *Manager) Activate(auth *cliproxyauth.Auth) {
	if !m.Enabled() {
		return
	}
	m.activationMu.Lock()
	defer m.activationMu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	first := false
	m.activateOnce.Do(func() { first = true; m.start(auth) })
	if first || auth == nil {
		return
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return
	}
	accessToken := oauthAccessToken(auth)
	sessionKey, errSession := claudedesktop.SessionKeyFromMetadata(auth.Metadata)
	m.mu.Lock()
	changed := accessToken != m.accessToken || enrollment.AccountUUID != m.enrollment.AccountUUID || enrollment.OrganizationUUID != m.enrollment.OrganizationUUID
	m.auth, m.enrollment, m.accessToken = auth.Clone(), enrollment, accessToken
	if errSession == nil {
		m.sessionKey, m.missingSessionKey = sessionKey, false
	}
	if changed {
		m.authGeneration++
		for _, host := range m.featureHosts {
			select {
			case host.wake <- struct{}{}:
			default:
			}
		}
	}
	m.mu.Unlock()
	if changed {
		select {
		case m.featureRefreshWake <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) start(auth *cliproxyauth.Auth) {
	m.mu.Lock()
	m.auth = auth.Clone()
	m.authGeneration++
	m.status.State = "starting"
	m.status.StartedAt = m.now().UTC().Format(time.RFC3339Nano)
	m.mu.Unlock()

	if auth == nil {
		m.failAll("invalid-enrollment")
		return
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		m.failAll("invalid-enrollment")
		return
	}
	identity, errIdentity := loadOrCreateIdentity(m.identityPath(), m.now())
	if errIdentity != nil {
		m.failAll("identity-state")
		return
	}
	accessToken := oauthAccessToken(auth)
	sessionKey, errSessionKey := claudedesktop.SessionKeyFromMetadata(auth.Metadata)
	m.mu.Lock()
	m.enrollment = enrollment
	m.identity = identity
	m.accessToken = accessToken
	if errSessionKey == nil {
		m.sessionKey = sessionKey
	} else {
		m.missingSessionKey = true
	}
	m.mu.Unlock()

	var updateHead, updateGet *claudeprofile.StartupEndpointProfile
	for _, endpoint := range m.profile.Endpoints {
		missing := m.missingFacts(endpoint)
		if len(missing) > 0 {
			category := "missing-runtime-fact"
			if containsString(missing, "session_key") {
				category = "missing-session-key"
			}
			m.recordSkipped(category)
			continue
		}
		switch endpoint.EndpointRole {
		case "startup-update-head":
			updateHead = &endpoint
			continue
		case "startup-update-get":
			updateGet = &endpoint
			continue
		}
		m.mu.Lock()
		m.status.Attempted++
		m.mu.Unlock()
		m.wg.Add(1)
		go func(profile claudeprofile.StartupEndpointProfile) {
			defer m.wg.Done()
			m.execute(profile)
			if profile.EndpointRole == "startup-sdk-eval" {
				m.runFeatureRefreshLoop(profile)
			}
		}(endpoint)
	}
	if updateHead != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.recordAttempt()
			if !m.execute(*updateHead) {
				if updateGet != nil {
					m.recordSkipped("update-head-failed")
				}
				return
			}
			if updateGet != nil && m.ctx.Err() == nil {
				m.recordAttempt()
				m.execute(*updateGet)
			}
		}()
	} else if updateGet != nil {
		m.recordSkipped("missing-update-head")
	}
	m.refreshState()
}

func (m *Manager) recordAttempt() {
	m.mu.Lock()
	m.status.Attempted++
	m.mu.Unlock()
}

func (m *Manager) failAll(category string) {
	m.mu.Lock()
	m.status.Failed = len(m.profile.Endpoints)
	m.status.State = "degraded"
	m.status.LastErrorCategory = category
	m.status.CompletedAt = m.now().UTC().Format(time.RFC3339Nano)
	m.mu.Unlock()
}

func (m *Manager) missingFacts(endpoint claudeprofile.StartupEndpointProfile) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	missing := make([]string, 0, len(endpoint.RequiredFacts))
	for _, fact := range endpoint.RequiredFacts {
		// The capture contained this attribute, but the native evaluation factory
		// conditionally omits it. It is not a prerequisite for feature fetching.
		if endpoint.EndpointRole == "startup-sdk-eval" && fact == "subscription_created_at" {
			continue
		}
		available := false
		switch fact {
		case "oauth_access_token":
			available = m.accessToken != ""
		case "session_key":
			available = m.sessionKey != ""
		case "account_uuid":
			available = m.enrollment.AccountUUID != ""
		case "organization_uuid":
			available = m.enrollment.OrganizationUUID != ""
		case "installation_id":
			available = m.identity.InstallationID != ""
		case "subscription_created_at":
			available = authMetadataInt64(m.auth, "subscription_created_at", "subscriptionCreatedAt") > 0
		}
		if !available {
			missing = append(missing, fact)
		}
	}
	return missing
}

func (m *Manager) execute(endpoint claudeprofile.StartupEndpointProfile, refresh ...bool) bool {
	return m.executeForSDKHost(endpoint, len(refresh) > 0 && refresh[0], nil)
}

func (m *Manager) executeForSDKHost(endpoint claudeprofile.StartupEndpointProfile, refresh bool, host *sdkFeatureRunner) bool {
	recordFailure, recordSuccess := m.recordFailure, m.recordSuccess
	if refresh {
		recordFailure = func(category string) { m.recordHostFeatureRefresh(host, false, category) }
		recordSuccess = func() { m.recordHostFeatureRefresh(host, true, "") }
	}
	if !refresh && host == nil && m.supportsSessionsWatchRetry(endpoint) {
		return m.executeSessionsWatch(endpoint, recordFailure, recordSuccess)
	}
	request, doer, cancel, errBuild := m.buildRequestForSDKHost(endpoint, host)
	if errBuild != nil {
		recordFailure(classifyError(errBuild))
		return false
	}
	defer cancel()
	if request.Context().Err() != nil {
		return false
	}
	if endpoint.EndpointRole == "startup-sdk-eval" {
		observation, _ := request.Context().Value(sdkFeatureObservationKey{}).(sdkFeatureObservation)
		if observation.failure != nil {
			record := recordFailure
			recordFailure = func(category string) {
				_ = observation.failure()
				record(category)
			}
		}
	}
	if endpoint.EndpointRole == "startup-update-head" {
		m.observeUpdateCheck(claudetelemetry.FactUpdateCheckStarted)
	}
	response, errDo := m.doStartupRequest(endpoint, request, doer)
	if errDo != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if errors.Is(errDo, context.Canceled) && request.Context().Err() != nil {
			return false
		}
		recordFailure(classifyError(errDo))
		return false
	}
	if response == nil || response.Body == nil {
		recordFailure("empty-response")
		return false
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, m.profile.MaxResponseBytes))
		_ = response.Body.Close()
		recordFailure(httpStatusCategory(response.StatusCode))
		return false
	}
	if endpoint.EndpointRole == "startup-sdk-eval" {
		// Native fetchRemoteEval increments on Response.ok, before downstream
		// JSON processing or persistence. Those failures must not trigger the
		// cadence's extra network fetch as though transport had failed.
		m.mu.Lock()
		if host == nil {
			m.sdkFetchOK++
		} else {
			host.fetchOK++
		}
		m.mu.Unlock()
	}
	if endpoint.EndpointRole == "startup-update-get" {
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			recordFailure("unmodeled-update-result")
			return false
		}
		noUpdate, errResult := readUpdateResult(response, m.profile.MaxResponseBytes, strings.TrimSuffix(m.bundle.DesktopVersion, ".0"))
		if errResult != nil {
			recordFailure("invalid-update-response")
			return false
		}
		if !noUpdate {
			recordFailure("unmodeled-update-result")
			return false
		}
		if etag := strings.TrimSpace(response.Header.Get("ETag")); etag != "" {
			m.storeETag(endpoint.EndpointRole, etag)
		}
		m.observeUpdateCheck(claudetelemetry.FactUpdateNotAvailable)
		recordSuccess()
		return true
	}
	if etag := strings.TrimSpace(response.Header.Get("ETag")); etag != "" {
		m.storeETag(endpoint.EndpointRole, etag)
	}
	observation, _ := request.Context().Value(sdkFeatureObservationKey{}).(sdkFeatureObservation)
	if endpoint.EndpointRole == "startup-sdk-eval" && observation.apply != nil {
		payload, errRead := readStartupJSONBody(response, m.profile.MaxResponseBytes)
		if errRead != nil || observation.apply(payload) != nil {
			if request.Context().Err() != nil {
				return false
			}
			recordFailure("invalid-sdk-feature-response")
			return false
		}
		recordSuccess()
		return true
	}
	if endpoint.ResponseMode == "stream" {
		recordSuccess()
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return true
	}
	n, errRead := io.Copy(io.Discard, io.LimitReader(response.Body, m.profile.MaxResponseBytes+1))
	errClose := response.Body.Close()
	if endpoint.EndpointRole == "startup-update-head" && (errRead != nil || errClose != nil || n > m.profile.MaxResponseBytes) {
		recordFailure("invalid-update-response")
		return false
	}
	recordSuccess()
	if errRead != nil || errClose != nil {
		log.WithError(errors.Join(errRead, errClose)).WithField("endpoint_role", endpoint.EndpointRole).Debug("claude desktop startup response ended after successful headers")
	}
	return true
}

// The retry loop is a renderer contract measured only for Desktop 1.40609.0.0.
// Other versions keep the ordinary one-shot startup behavior until captured.
func (m *Manager) supportsSessionsWatchRetry(endpoint claudeprofile.StartupEndpointProfile) bool {
	return m != nil && m.bundle != nil && m.bundle.DesktopVersion == "1.40609.0.0" &&
		endpoint.EndpointRole == startupCodeSessionsWatchRole && endpoint.ResponseMode == "stream"
}

func defaultSessionsWatchRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Second
	for attempt > 0 && delay < 30*time.Second {
		delay *= 2
		attempt--
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

// executeSessionsWatch keeps the captured long-lived stream alive across an
// upstream 502. Retry attempts are one startup endpoint, so the status counters
// receive exactly one terminal success or failure instead of one failure per
// reconnect.
func (m *Manager) executeSessionsWatch(endpoint claudeprofile.StartupEndpointProfile, recordFailure func(string), recordSuccess func()) bool {
	for retry := 0; ; retry++ {
		request, doer, cancel, errBuild := m.buildRequest(endpoint)
		if errBuild != nil {
			recordFailure(classifyError(errBuild))
			return false
		}
		if request.Context().Err() != nil {
			cancel()
			return false
		}
		response, errDo := m.doStartupRequest(endpoint, request, doer)
		if errDo != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			cancel()
			if errors.Is(errDo, context.Canceled) && request.Context().Err() != nil {
				return false
			}
			recordFailure(classifyError(errDo))
			return false
		}
		if response == nil || response.Body == nil {
			cancel()
			recordFailure("empty-response")
			return false
		}
		if response.StatusCode == http.StatusBadGateway {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, m.profile.MaxResponseBytes))
			_ = response.Body.Close()
			cancel()
			m.observeSessionsWatchRetry(response.StatusCode)
			if !m.waitForSessionsWatchRetry(m.sessionsWatchRetryDelay(retry)) {
				return false
			}
			continue
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, m.profile.MaxResponseBytes))
			_ = response.Body.Close()
			cancel()
			recordFailure(httpStatusCategory(response.StatusCode))
			return false
		}
		if etag := strings.TrimSpace(response.Header.Get("ETag")); etag != "" {
			m.storeETag(endpoint.EndpointRole, etag)
		}
		recordSuccess()
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		cancel()
		return true
	}
}

func (m *Manager) waitForSessionsWatchRetry(delay time.Duration) bool {
	if delay <= 0 {
		return m.ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) observeSessionsWatchRetry(status int) {
	if m.sessionsWatchRetryObserver == nil {
		return
	}
	m.mu.Lock()
	auth := m.auth.Clone()
	m.mu.Unlock()
	if errObserve := m.sessionsWatchRetryObserver(auth, status); errObserve != nil {
		m.mu.Lock()
		m.status.TelemetryErrorCategory = "watch-retry-event-projection-or-queue"
		m.mu.Unlock()
	}
}

func (m *Manager) observeUpdateCheck(fact string) {
	if m.updateCheckObserver == nil {
		return
	}
	m.mu.Lock()
	auth := m.auth.Clone()
	m.mu.Unlock()
	if errObserve := m.updateCheckObserver(auth, fact); errObserve != nil {
		m.mu.Lock()
		m.status.TelemetryErrorCategory = "update-event-projection-or-queue"
		m.mu.Unlock()
	}
}

func (m *Manager) buildRequest(endpoint claudeprofile.StartupEndpointProfile) (*http.Request, HTTPDoer, context.CancelFunc, error) {
	return m.buildRequestForSDKHost(endpoint, nil)
}

func (m *Manager) buildRequestForSDKHost(endpoint claudeprofile.StartupEndpointProfile, host *sdkFeatureRunner) (*http.Request, HTTPDoer, context.CancelFunc, error) {
	m.mu.Lock()
	auth := m.auth.Clone()
	enrollment := m.enrollment
	accessToken := m.accessToken
	sessionKey := m.sessionKey
	identity := m.identity
	identity.ETags = make(map[string]string, len(m.identity.ETags))
	for role, value := range m.identity.ETags {
		identity.ETags[role] = value
	}
	sdkSessionID := m.appSessionID
	var observer func([]byte) error
	var failureObserver func() error
	var errObserver error
	authedEvaluation := false
	if endpoint.EndpointRole == "startup-sdk-eval" {
		prepare, authed := m.sdkFeaturesObserverFactory, m.sdkFeatureAuthedEvaluation
		failure := m.sdkFeatureFailureObserver
		if host != nil {
			prepare, authed = host.binding.Prepare, host.binding.Authed
			failure = host.binding.Failure
		}
		if prepare != nil {
			var session string
			session, observer, errObserver = prepare(auth)
			if session != "" {
				sdkSessionID = session
			}
		} else if host == nil && m.sdkFeaturesObserver != nil {
			observer = func(payload []byte) error { return m.sdkFeaturesObserver(auth, payload) }
		}
		if authed != nil {
			authedEvaluation = authed(auth)
		}
		if observer != nil {
			apply, generation := observer, m.authGeneration
			observer = func(payload []byte) error {
				m.mu.Lock()
				defer m.mu.Unlock()
				if generation != m.authGeneration || m.ctx.Err() != nil || (host == nil && m.sdkFeatureContext.Err() != nil) || (host != nil && host.ctx.Err() != nil) {
					return fmt.Errorf("stale SDK feature observation")
				}
				return apply(payload)
			}
		}
		if failure != nil {
			generation := m.authGeneration
			failureObserver = func() error {
				m.mu.Lock()
				defer m.mu.Unlock()
				if generation != m.authGeneration || m.ctx.Err() != nil || (host == nil && m.sdkFeatureContext.Err() != nil) || (host != nil && host.ctx.Err() != nil) {
					return nil
				}
				return failure(auth)
			}
		}
	}
	m.mu.Unlock()
	if errObserver != nil {
		return nil, nil, func() {}, errObserver
	}
	rawURL := strings.ReplaceAll(endpoint.Endpoint, "{organization_uuid}", url.PathEscape(enrollment.OrganizationUUID))
	parsed, errURL := url.Parse(rawURL)
	if errURL != nil {
		return nil, nil, func() {}, fmt.Errorf("startup-url: %w", errURL)
	}
	queryParts := make([]string, 0, len(endpoint.Query))
	for _, parameter := range endpoint.Query {
		value := m.resolveTemplate(parameter.Value, identity)
		if value == "" {
			return nil, nil, func() {}, fmt.Errorf("startup-query: unresolved template")
		}
		queryParts = append(queryParts, url.QueryEscape(parameter.Name)+"="+url.QueryEscape(value))
	}
	parsed.RawQuery = strings.Join(queryParts, "&")
	var body []byte
	if endpoint.EndpointRole == "startup-sdk-eval" {
		var errBody error
		body, errBody = m.evalBodyFor(identity, auth, enrollment, sdkSessionID)
		if errBody != nil {
			return nil, nil, func() {}, errBody
		}
	}
	requestContext := m.ctx
	if endpoint.EndpointRole == "startup-sdk-eval" {
		requestContext = m.sdkFeatureContext
	}
	if host != nil {
		requestContext = host.ctx
	}
	cancel := func() {}
	if endpoint.ResponseMode != "stream" && endpoint.EndpointRole != "startup-sdk-eval" {
		requestContext, cancel = context.WithTimeout(m.ctx, time.Duration(m.profile.RequestTimeoutMS)*time.Millisecond)
	}
	if observer != nil || failureObserver != nil || authedEvaluation {
		requestContext = context.WithValue(requestContext, sdkFeatureObservationKey{}, sdkFeatureObservation{apply: observer, failure: failureObserver, authed: authedEvaluation})
	}
	request, errRequest := http.NewRequestWithContext(requestContext, endpoint.Method, parsed.String(), bytes.NewReader(body))
	if errRequest != nil {
		cancel()
		return nil, nil, func() {}, fmt.Errorf("startup-request: %w", errRequest)
	}
	headerProfile, okProfile := m.profile.HeaderProfiles[endpoint.HeaderProfile]
	if !okProfile {
		cancel()
		return nil, nil, func() {}, fmt.Errorf("startup-header-profile: unavailable")
	}
	for _, header := range headerProfile.Headers {
		request.Header.Add(header.Name, header.Value)
	}
	m.applyDynamicHeaders(request, headerProfile, endpoint, enrollment, identity, accessToken, sessionKey)
	doer, errDoer := m.doerFactory(m.ctx, endpoint.EndpointRole, auth)
	if errDoer != nil || doer == nil {
		cancel()
		if errDoer == nil {
			errDoer = fmt.Errorf("doer is nil")
		}
		return nil, nil, func() {}, fmt.Errorf("startup-transport: %w", errDoer)
	}
	return request, doer, cancel, nil
}

func (m *Manager) applyDynamicHeaders(request *http.Request, profile claudeprofile.StartupHeaderProfile, endpoint claudeprofile.StartupEndpointProfile, enrollment claudedesktop.Enrollment, identity persistentIdentity, accessToken, sessionKey string) {
	runtimeProfile := m.bundle.Telemetry.Runtime
	sentry := m.bundle.Telemetry.Sentry
	host := claudetelemetry.HostSnapshot{}
	if m.hostSnapshot != nil {
		host = m.hostSnapshot()
	}
	totalBytes := host.TotalMemoryBytes
	if totalBytes == 0 {
		totalBytes = uint64(runtimeProfile.FallbackTotalMemoryGB) * rendererGiB
	}
	totalMemoryGB := (totalBytes + rendererGiB/2) / rendererGiB
	if totalMemoryGB == 0 {
		totalMemoryGB = 1
	}
	osVersion := strings.TrimSpace(host.OSVersion)
	if osVersion == "" {
		osVersion = runtimeProfile.OSVersion
	}
	traceID := randomHex(16)
	spanID := randomHex(8)
	traceDecimal := randomUint64String()
	parentDecimal := randomUint64String()
	for _, wireName := range profile.HeaderOrder {
		if request.Header.Get(wireName) != "" {
			continue
		}
		switch strings.ToLower(wireName) {
		case "authorization":
			if endpoint.AuthPolicy == "oauth-bearer" {
				request.Header.Set(wireName, "Bearer "+accessToken)
			}
		case "cookie":
			if endpoint.AuthPolicy == "session-cookie" {
				request.Header.Add(wireName, (&http.Cookie{Name: "sessionKey", Value: sessionKey}).String())
			}
		case "x-organization-uuid":
			request.Header.Set(wireName, enrollment.OrganizationUUID)
		case "accept-encoding":
			request.Header.Set(wireName, runtimeProfile.AcceptEncoding)
		case "accept-language":
			request.Header.Set(wireName, runtimeProfile.AcceptLanguage)
		case "anthropic-client-app":
			request.Header.Set(wireName, runtimeProfile.ClientApp)
		case "anthropic-client-device-class":
			request.Header.Set(wireName, rendererDeviceClass(totalBytes))
		case "anthropic-client-os-platform":
			request.Header.Set(wireName, runtimeProfile.OSPlatform)
		case "anthropic-client-os-version":
			request.Header.Set(wireName, osVersion)
		case "anthropic-client-platform":
			request.Header.Set(wireName, runtimeProfile.Platform)
		case "anthropic-client-total-memory-gb":
			request.Header.Set(wireName, strconv.FormatUint(totalMemoryGB, 10))
		case "anthropic-client-version":
			request.Header.Set(wireName, runtimeProfile.ClientVersion)
		case "anthropic-desktop-topbar":
			request.Header.Set(wireName, runtimeProfile.DesktopTopbar)
		case "anthropic-client-build":
			request.Header.Set(wireName, m.profile.WebClientBuild)
		case "anthropic-client-sha":
			request.Header.Set(wireName, m.profile.WebClientSHA)
		case "anthropic-anonymous-id":
			request.Header.Set(wireName, "claudeai.v1."+identity.InstallationID)
		case "anthropic-device-id":
			request.Header.Set(wireName, identity.InstallationID)
		case "user-agent":
			request.Header.Set(wireName, runtimeProfile.UserAgent)
		case "sentry-trace":
			request.Header.Set(wireName, traceID+"-"+spanID)
		case "baggage":
			request.Header.Set(wireName, strings.Join([]string{"sentry-environment=" + sentry.Environment, "sentry-release=" + sentry.Release, "sentry-public_key=" + sentry.PublicKey, "sentry-trace_id=" + traceID, "sentry-org_id=" + sentry.OrgID}, ","))
		case "traceparent":
			request.Header.Set(wireName, "00-"+traceID+"-"+spanID+"-01")
		case "x-datadog-trace-id":
			request.Header.Set(wireName, traceDecimal)
		case "x-datadog-parent-id":
			request.Header.Set(wireName, parentDecimal)
		case "x-activity-session-id":
			request.Header.Set(wireName, m.appSessionID)
		case "if-none-match":
			if etag := identity.ETags[endpoint.EndpointRole]; etag != "" {
				request.Header.Set(wireName, etag)
			}
		}
	}
}

func (m *Manager) evalBody(identity persistentIdentity) ([]byte, error) {
	m.mu.Lock()
	auth := m.auth
	enrollment := m.enrollment
	m.mu.Unlock()
	return m.evalBodyFor(identity, auth, enrollment, m.appSessionID)
}

func (m *Manager) evalBodyFor(identity persistentIdentity, auth *cliproxyauth.Auth, enrollment claudedesktop.Enrollment, sessionID string) ([]byte, error) {
	attributes := map[string]any{
		"id": claudedesktop.RequestDeviceID(enrollment.DeviceID), "sessionId": sessionID, "deviceID": claudedesktop.RequestDeviceID(enrollment.DeviceID),
		"platform": "win32", "organizationUUID": enrollment.OrganizationUUID, "accountUUID": enrollment.AccountUUID,
		"userType": "external", "appVersion": m.bundle.CodeVersion, "entrypoint": "claude-desktop",
	}
	for output, keys := range map[string][]string{
		"subscriptionType": {"subscription_type", "subscriptionType", "billing_type"},
		"rateLimitTier":    {"rate_limit_tier", "rateLimitTier"},
		"organizationRole": {"organization_role", "organizationRole"},
		"email":            {"email"},
	} {
		for _, key := range keys {
			if value := authMetadataStringDefault(auth, key, ""); value != "" {
				attributes[output] = value
				break
			}
		}
	}
	if createdAt := sdkSubscriptionCreatedAt(auth); createdAt > 0 {
		attributes["subscriptionCreatedAt"] = createdAt
	}
	payload := map[string]any{
		"attributes":     attributes,
		"forcedFeatures": []string{}, "url": "",
	}
	return json.Marshal(payload)
}

func sdkSubscriptionCreatedAt(auth *cliproxyauth.Auth) int64 {
	for _, key := range []string{"subscription_created_at", "subscriptionCreatedAt"} {
		value := authMetadataStringDefault(auth, key, "")
		if value != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UnixMilli()
			}
			if parsed, err := time.Parse(time.RFC3339Nano, value+"Z"); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	// Numeric metadata is an explicit normalized millisecond fact. Do not guess
	// a missing unit or substitute the application/credential creation time.
	return authMetadataInt64(auth, "subscription_created_at", "subscriptionCreatedAt")
}

func (m *Manager) resolveTemplate(value string, identity persistentIdentity) string {
	host := claudetelemetry.HostSnapshot{}
	if m.hostSnapshot != nil {
		host = m.hostSnapshot()
	}
	osVersion := strings.TrimSpace(host.OSVersion)
	if osVersion == "" {
		osVersion = m.bundle.Telemetry.Runtime.OSVersion
	}
	replacer := strings.NewReplacer(
		"{{installation_id}}", identity.InstallationID,
		"{{desktop_version_short}}", strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		"{{os_version}}", osVersion,
	)
	return strings.TrimSpace(replacer.Replace(value))
}

func (m *Manager) storeETag(role, etag string) {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	if m.identity.ETags == nil {
		m.identity.ETags = make(map[string]string)
	}
	m.identity.ETags[role] = etag
	identity := persistentIdentity{
		Version:        m.identity.Version,
		InstallationID: m.identity.InstallationID,
		CreatedAt:      m.identity.CreatedAt,
		ETags:          make(map[string]string, len(m.identity.ETags)),
	}
	for endpointRole, cachedETag := range m.identity.ETags {
		identity.ETags[endpointRole] = cachedETag
	}
	m.mu.Unlock()
	if errPersist := saveIdentity(m.identityPath(), identity); errPersist != nil {
		log.WithError(errPersist).Debug("claude desktop startup ETag cache was not persisted")
	}
}

func (m *Manager) recordSkipped(category string) {
	m.mu.Lock()
	m.status.Skipped++
	m.status.LastErrorCategory = category
	m.mu.Unlock()
	m.refreshState()
}

func (m *Manager) recordSuccess() {
	m.mu.Lock()
	m.status.Completed++
	m.mu.Unlock()
	m.refreshState()
}

func (m *Manager) recordFailure(category string) {
	m.mu.Lock()
	m.status.Failed++
	m.status.LastErrorCategory = category
	m.mu.Unlock()
	m.refreshState()
}

func (m *Manager) refreshState() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == "stopped" {
		return
	}
	if m.status.Completed+m.status.Failed < m.status.Attempted || m.status.Attempted+m.status.Skipped < m.status.EndpointCount {
		if m.missingSessionKey {
			m.status.State = "missing-session-key"
		} else {
			m.status.State = "starting"
		}
		return
	}
	m.status.CompletedAt = m.now().UTC().Format(time.RFC3339Nano)
	switch {
	case m.missingSessionKey:
		m.status.State = "missing-session-key"
	case m.status.Failed > 0 || m.status.Skipped > 0 || m.status.TelemetryErrorCategory != "" || m.status.FeatureRefreshError != "":
		m.status.State = "degraded"
	default:
		m.status.State = "ready"
	}
}

func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: "stopped"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.activationMu.Lock()
		m.cancel()
		if m.stopSDKFeatureContext != nil {
			m.stopSDKFeatureContext()
		}
		m.activationMu.Unlock()
		m.wg.Wait()
		m.retireDefaultFeatureHost()
		m.mu.Lock()
		m.status.State = "stopped"
		m.status.CompletedAt = m.now().UTC().Format(time.RFC3339Nano)
		m.mu.Unlock()
	})
}

func (m *Manager) identityPath() string {
	return strings.TrimRight(m.root, "\\/") + "/startup/identity.json"
}

func oauthAccessToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeAPIKey]); value != "" {
			return value
		}
	}
	if auth.Metadata != nil {
		value, _ := auth.Metadata["access_token"].(string)
		return strings.TrimSpace(value)
	}
	return ""
}

func authMetadataInt64(auth *cliproxyauth.Auth, keys ...string) int64 {
	if auth == nil || auth.Metadata == nil {
		return 0
	}
	for _, key := range keys {
		switch value := auth.Metadata[key].(type) {
		case int64:
			return value
		case int:
			return int64(value)
		case float64:
			return int64(value)
		case json.Number:
			parsed, _ := value.Int64()
			if parsed > 0 {
				return parsed
			}
		case string:
			parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func authMetadataStringDefault(auth *cliproxyauth.Auth, key, fallback string) string {
	if auth != nil && auth.Metadata != nil {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return fallback
}

func subscriptionType(auth *cliproxyauth.Auth) string {
	for _, key := range []string{"subscription_type", "subscriptionType", "billing_type"} {
		if value := authMetadataStringDefault(auth, key, ""); value != "" {
			return value
		}
	}
	return "unknown"
}

func rendererDeviceClass(totalBytes uint64) string {
	switch {
	case totalBytes < 6*rendererGiB:
		return "le4"
	case totalBytes < 12*rendererGiB:
		return "8"
	case totalBytes < 20*rendererGiB:
		return "16"
	default:
		return "gt16"
	}
}

func randomHex(bytesCount int) string {
	value := make([]byte, bytesCount)
	if _, errRead := rand.Read(value); errRead != nil {
		return strings.Repeat("0", bytesCount*2)
	}
	return hex.EncodeToString(value)
}

func randomUint64String() string {
	value := make([]byte, 8)
	if _, errRead := rand.Read(value); errRead != nil {
		return "1"
	}
	number := binary.BigEndian.Uint64(value)
	if number == 0 {
		number = 1
	}
	return strconv.FormatUint(number, 10)
}

func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	message := strings.ToLower(err.Error())
	for _, category := range []string{"startup-url", "startup-query", "startup-request", "startup-header-profile", "startup-transport", "startup-eval"} {
		if strings.Contains(message, category) {
			return category
		}
	}
	return "network"
}

func httpStatusCategory(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication"
	case status == http.StatusTooManyRequests:
		return "rate-limit"
	case status >= 500:
		return "upstream-5xx"
	case status >= 400:
		return "upstream-4xx"
	default:
		return "http-status"
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
