package startup

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// SDKFeatureHost binds one query's evaluation and refresh lifetime. The
// callbacks resolve that host's current session; they do not choose the newest
// session on an account. Context cancellation terminates only this host.
type SDKFeatureHost struct {
	Context context.Context
	Prepare SDKFeaturesObserverFactory
	Cadence func(*cliproxyauth.Auth) (time.Duration, bool)
	Authed  func(*cliproxyauth.Auth) bool
	Failure func(*cliproxyauth.Auth) error
}

type sdkFeatureRunner struct {
	binding SDKFeatureHost
	ctx     context.Context
	wake    chan struct{}
	once    sync.Once
	fetchOK uint64 // Protected by Manager.mu, independently of other hosts.
}

// AddSDKFeatureHost starts only the SDK evaluation endpoint. It does not replay
// Renderer/bootstrap startup traffic, mutate the caller's identity or allocate
// a second application lifetime. Repeated installation of one handle is inert.
func (m *Manager) AddSDKFeatureHost(binding *SDKFeatureHost) error {
	if m == nil || binding == nil || binding.Context == nil || binding.Prepare == nil {
		return fmt.Errorf("Claude Desktop SDK feature host is unavailable")
	}
	m.activationMu.Lock()
	defer m.activationMu.Unlock()
	if !m.Enabled() || m.ctx.Err() != nil || binding.Context.Err() != nil {
		return fmt.Errorf("Claude Desktop SDK feature host cannot start")
	}
	var endpoint claudeprofile.StartupEndpointProfile
	for _, candidate := range m.profile.Endpoints {
		if candidate.EndpointRole == "startup-sdk-eval" {
			endpoint = candidate
			break
		}
	}
	if endpoint.EndpointRole == "" {
		return fmt.Errorf("Claude Desktop SDK evaluation endpoint is unavailable")
	}
	m.mu.Lock()
	if m.auth == nil {
		m.mu.Unlock()
		return fmt.Errorf("Claude Desktop SDK feature host has no active account")
	}
	if m.featureHosts[binding] != nil {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	stop := context.AfterFunc(binding.Context, cancel)
	runner := &sdkFeatureRunner{binding: *binding, ctx: ctx, wake: make(chan struct{}, 1)}
	if m.featureHosts == nil {
		m.featureHosts = make(map[*SDKFeatureHost]*sdkFeatureRunner)
	}
	m.featureHosts[binding] = runner
	m.status.FeatureRefreshAttempted++
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer stop()
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.featureHosts, binding)
			delete(m.featureHostErrors, runner)
			m.refreshFeatureErrorLocked()
			m.mu.Unlock()
			m.refreshState()
		}()
		m.executeForSDKHost(endpoint, true, runner)
		m.runFeatureRefreshLoopForHost(endpoint, runner)
	}()
	return nil
}

// Authenticated evaluation is an alternate route of the same SDK fetch, not
// another startup endpoint. A failed alternate request falls back with the
// original body, headers, owner and cancellation context. Native credential
// refresh/trust providers remain independent of this routing decision.
func (m *Manager) doStartupRequest(endpoint claudeprofile.StartupEndpointProfile, request *http.Request, doer HTTPDoer) (*http.Response, error) {
	observation, _ := request.Context().Value(sdkFeatureObservationKey{}).(sdkFeatureObservation)
	if endpoint.EndpointRole != "startup-sdk-eval" || !observation.authed {
		return doer.Do(request)
	}
	if !strings.HasPrefix(request.URL.Path, "/api/eval/") || request.GetBody == nil {
		return nil, fmt.Errorf("startup-sdk-eval: unsupported authenticated evaluation route")
	}
	authed := request.Clone(request.Context())
	authed.URL.Path = "/api/eval-authed/" + strings.TrimPrefix(request.URL.Path, "/api/eval/")
	authed.URL.RawPath = ""
	var errBody error
	authed.Body, errBody = request.GetBody()
	if errBody != nil {
		return nil, fmt.Errorf("startup-sdk-eval: replay evaluation body: %w", errBody)
	}
	response, err := doer.Do(authed)
	if err == nil && response != nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		_ = request.Body.Close()
		return response, nil
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if request.Context().Err() != nil {
		_ = request.Body.Close()
		return nil, request.Context().Err()
	}
	return doer.Do(request)
}

// One loop belongs to the owned SDK host. Credential changes wake this loop;
// they never replay the other application-startup endpoints.
func (m *Manager) runFeatureRefreshLoop(endpoint claudeprofile.StartupEndpointProfile) {
	m.runFeatureRefreshLoopForHost(endpoint, nil)
}

func (m *Manager) runFeatureRefreshLoopForHost(endpoint claudeprofile.StartupEndpointProfile, host *sdkFeatureRunner) {
	ctx, wake, once, cadence := m.sdkFeatureContext, m.featureRefreshWake, &m.featureLoopOnce, m.sdkFeatureRefreshCadence
	if host != nil {
		ctx, wake, once, cadence = host.ctx, host.wake, &host.once, host.binding.Cadence
		if cadence == nil {
			cadence = func(*cliproxyauth.Auth) (time.Duration, bool) { return 6 * time.Hour, false }
		}
	}
	if cadence == nil {
		return
	}
	fetchedCount := func() uint64 {
		if host != nil {
			return host.fetchOK
		}
		return m.sdkFetchOK
	}
	once.Do(func() {
		for ctx.Err() == nil {
			m.mu.Lock()
			auth := m.auth.Clone()
			m.mu.Unlock()
			interval, flagged := cadence(auth)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			m.status.FeatureRefreshAttempted++
			beforeFetch := fetchedCount()
			m.mu.Unlock()
			m.executeForSDKHost(endpoint, true, host)
			m.mu.Lock()
			fetched := fetchedCount() != beforeFetch
			m.mu.Unlock()
			if !fetched && flagged && ctx.Err() == nil {
				retry := time.NewTimer(time.Duration(math.Floor(m.randomFloat()*5000+0.5)) * time.Millisecond)
				select {
				case <-ctx.Done():
					retry.Stop()
					return
				case <-wake:
					retry.Stop()
				case <-retry.C:
				}
				if ctx.Err() != nil {
					return
				}
				m.mu.Lock()
				m.status.FeatureRefreshAttempted++
				m.mu.Unlock()
				m.executeForSDKHost(endpoint, true, host)
			}
		}
	})
}

func (m *Manager) recordFeatureRefresh(success bool, category string) {
	m.recordHostFeatureRefresh(nil, success, category)
}

func (m *Manager) recordHostFeatureRefresh(host *sdkFeatureRunner, success bool, category string) {
	m.mu.Lock()
	if (host == nil && m.sdkFeatureContext.Err() != nil) || (host != nil && host.ctx != nil && host.ctx.Err() != nil) {
		m.mu.Unlock()
		return
	}
	if success {
		m.status.FeatureRefreshCompleted++
	} else {
		m.status.FeatureRefreshFailed++
	}
	if host == nil {
		m.featureDefaultError = category
	} else {
		if m.featureHostErrors == nil {
			m.featureHostErrors = make(map[*sdkFeatureRunner]string)
		}
		if category == "" {
			delete(m.featureHostErrors, host)
		} else {
			m.featureHostErrors[host] = category
		}
	}
	m.refreshFeatureErrorLocked()
	m.mu.Unlock()
	m.refreshState()
}

func (m *Manager) retireDefaultFeatureHost() {
	m.mu.Lock()
	m.featureDefaultError = ""
	m.refreshFeatureErrorLocked()
	m.mu.Unlock()
	m.refreshState()
}

// Retiring a host removes its active fault, without recording a successful
// refresh or erasing the historical attempted/failed counters.
func (m *Manager) refreshFeatureErrorLocked() {
	errors := []string{}
	if m.featureDefaultError != "" {
		errors = append(errors, m.featureDefaultError)
	}
	for _, value := range m.featureHostErrors {
		errors = append(errors, value)
	}
	sort.Strings(errors)
	m.status.FeatureRefreshError = ""
	if len(errors) > 0 {
		m.status.FeatureRefreshError = errors[0]
	}
}
