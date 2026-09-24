package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type Manager struct {
	updateMu             sync.Mutex
	updateChecks         map[string]string
	promptIssues         map[string]string
	promptIssuesOverflow bool
	sdkParentMu          sync.Mutex
	sdkParents           map[string]sdkPromptParent
	root                 string
	bundle               *claudeprofile.Bundle
	profile              claudeprofile.TelemetryProfile
	sdkProfile           claudeprofile.SDKTelemetryProfile
	sdkDesktopVersion    string
	sdkCodeVersion       string
	sdkAgentSDKVersion   string
	auxiliaryProfiles    map[string]claudeprofile.AuxiliaryTelemetryProfile
	rendererDelivery     deliveryProfile
	sdkDelivery          deliveryProfile
	auxiliaryDeliveries  map[string]deliveryProfile
	doerFactory          DoerFactory
	endpointDoerFactory  EndpointDoerFactory
	rendererSnapshot     RendererRuntimeSnapshotProvider
	hostSnapshot         HostSnapshotProvider
	sdkProcessProvider   SDKProcessSnapshotProvider
	now                  func() time.Time
	randomFloat          func() float64
	startedAt            time.Time
	appSessionID         string
	ctx                  context.Context
	cancel               context.CancelFunc
	shutdownCtx          context.Context
	abortShutdown        context.CancelFunc
	mu                   sync.RWMutex
	workers              map[string]*accountWorker
	endpointMu           sync.RWMutex
	endpointStates       map[string]EndpointStatus
	factRestoreFailures  map[string]bool
	wg                   sync.WaitGroup
	closeOnce            sync.Once
	freezeOnShutdown     atomic.Bool
	lifecycleMu          sync.Mutex
	sessions             map[string]*rendererSessionState
	lastSessionKey       string
	nextSessionSequence  uint64
	registryKey          string
	configuration        string
	globalProxyURL       string
	machineProfileID     string
	activationMu         sync.Mutex
	activationEmitted    map[string]bool
}

type rendererSessionState struct {
	worker                  *accountWorker
	rumWorker               *accountWorker
	facts                   RequestFacts
	sequence                uint64
	startedAt               time.Time
	pendingRequests         int
	pendingStartedAt        time.Time
	pendingHadFirstResponse bool
	lastActivityAt          time.Time
	queryClosed             bool
}

var activeManagers = struct {
	sync.RWMutex
	values map[string]*Manager
}{values: make(map[string]*Manager)}

func NewManager(options Options) *Manager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	randomFloat := options.RandomFloat
	if randomFloat == nil {
		randomFloat = mathrand.Float64
	}
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx, abortShutdown := context.WithCancel(context.Background())
	manager := &Manager{
		root:                strings.TrimSpace(options.StatePath),
		bundle:              options.Bundle,
		doerFactory:         options.DoerFactory,
		endpointDoerFactory: options.EndpointDoerFactory,
		rendererSnapshot:    options.RendererRuntimeSnapshotProvider,
		hostSnapshot:        options.HostSnapshotProvider,
		sdkProcessProvider:  options.SDKProcessSnapshotProvider,
		now:                 now,
		randomFloat:         randomFloat,
		startedAt:           now(),
		appSessionID:        strings.TrimSpace(options.ApplicationSessionID),
		ctx:                 ctx,
		cancel:              cancel,
		shutdownCtx:         shutdownCtx,
		abortShutdown:       abortShutdown,
		workers:             make(map[string]*accountWorker),
		endpointStates:      make(map[string]EndpointStatus),
		auxiliaryProfiles:   make(map[string]claudeprofile.AuxiliaryTelemetryProfile),
		auxiliaryDeliveries: make(map[string]deliveryProfile),
		sessions:            make(map[string]*rendererSessionState),
		activationEmitted:   make(map[string]bool),
		globalProxyURL:      strings.TrimSpace(options.GlobalProxyURL),
		machineProfileID:    strings.TrimSpace(options.MachineProfileID),
	}
	if manager.appSessionID == "" {
		manager.appSessionID = uuid.New().String()
	}
	if manager.bundle != nil {
		manager.profile = manager.bundle.Telemetry
		manager.sdkProfile = manager.bundle.SDKTelemetry
		manager.sdkProfile.Headers = append([]claudeprofile.TelemetryHeader(nil), manager.sdkProfile.Headers...)
		manager.sdkProfile.Environment = cloneSDKEnvironment(manager.sdkProfile.Environment)
		manager.sdkProfile.InputBetas = cloneSDKInputBetas(manager.sdkProfile.InputBetas)
		manager.sdkDesktopVersion = strings.TrimSpace(manager.bundle.DesktopVersion)
		manager.sdkCodeVersion = strings.TrimSpace(manager.bundle.CodeVersion)
		manager.sdkAgentSDKVersion = strings.TrimSpace(manager.bundle.AgentSDKVersion)
		if count := len(manager.bundle.RequestProfiles); count > 0 {
			current := manager.bundle.RequestProfiles[count-1]
			manager.sdkDesktopVersion = strings.TrimSpace(current.DesktopVersion)
			manager.sdkCodeVersion = strings.TrimSpace(current.CodeVersion)
			manager.sdkAgentSDKVersion = strings.TrimSpace(current.AgentSDKVersion)
			for model, betas := range current.SDKInputBetas {
				manager.sdkProfile.InputBetas[model] = append([]string(nil), betas...)
			}
		}
		if manager.sdkCodeVersion != "" {
			manager.sdkProfile.Environment["version"] = manager.sdkCodeVersion
			manager.sdkProfile.Environment["version_base"] = manager.sdkCodeVersion
			for index := range manager.sdkProfile.Headers {
				if strings.EqualFold(strings.TrimSpace(manager.sdkProfile.Headers[index].Name), "User-Agent") {
					manager.sdkProfile.Headers[index].Value = "claude-code/" + manager.sdkCodeVersion
				}
			}
		}
		manager.rendererDelivery = deliveryProfile{
			endpointRole:      manager.profile.EndpointRole,
			endpoint:          manager.profile.Endpoint,
			bodyFormat:        "events-wrapper-json",
			headers:           append([]claudeprofile.TelemetryHeader(nil), manager.profile.Headers...),
			headerOrder:       append([]string(nil), manager.profile.Transport.HeaderOrder...),
			rendererRuntime:   &manager.profile.Runtime,
			sentry:            &manager.profile.Sentry,
			batch:             manager.profile.Batch,
			protocol:          manager.profile.Transport.Protocol,
			userAgentPolicy:   manager.profile.Transport.UserAgentPolicy,
			transportRevision: manager.profile.Transport.Revision,
			queueNamespace:    "renderer-event-logging",
		}
		manager.sdkDelivery = deliveryProfile{
			endpointRole:      manager.sdkProfile.EndpointRole,
			endpoint:          manager.sdkProfile.Endpoint,
			bodyFormat:        "events-wrapper-json",
			headers:           append([]claudeprofile.TelemetryHeader(nil), manager.sdkProfile.Headers...),
			headerOrder:       append([]string(nil), manager.sdkProfile.HeaderOrder...),
			batch:             manager.sdkProfile.Batch,
			protocol:          "http/1.1",
			userAgentPolicy:   "profile",
			transportRevision: sdkTransportRevision(manager.sdkProfile),
			authPolicy:        manager.sdkProfile.AuthPolicy,
			queueNamespace:    manager.sdkProfile.EndpointRole,
		}
		manager.endpointStates[manager.rendererDelivery.endpointRole] = EndpointStatus{Role: manager.rendererDelivery.endpointRole, Status: "ready"}
		manager.endpointStates[manager.sdkDelivery.endpointRole] = EndpointStatus{Role: manager.sdkDelivery.endpointRole, Status: "ready"}
		for _, auxiliary := range manager.bundle.AuxiliaryTelemetry.All() {
			manager.auxiliaryProfiles[auxiliary.EndpointRole] = auxiliary
			manager.auxiliaryDeliveries[auxiliary.EndpointRole] = deliveryProfile{
				endpointRole:      auxiliary.EndpointRole,
				endpoint:          auxiliary.Endpoint,
				bodyFormat:        auxiliary.BodyFormat,
				headers:           append([]claudeprofile.TelemetryHeader(nil), auxiliary.Headers...),
				headerOrder:       append([]string(nil), auxiliary.HeaderOrder...),
				runtimeMaterials:  cloneStringMap(auxiliary.RuntimeMaterials),
				batch:             auxiliary.Batch,
				protocol:          auxiliary.Protocol,
				userAgentPolicy:   auxiliary.UserAgentPolicy,
				transportRevision: auxiliaryTransportRevision(auxiliary),
				authPolicy:        auxiliary.AuthPolicy,
				queueNamespace:    auxiliary.EndpointRole,
			}
			manager.endpointStates[auxiliary.EndpointRole] = EndpointStatus{Role: auxiliary.EndpointRole, Status: "awaiting-enrollment-material", Reason: "runtime material has not been observed on an enrolled account"}
		}
	}
	if manager.root != "" {
		if absolute, errAbs := filepath.Abs(manager.root); errAbs == nil {
			manager.root = absolute
		}
	}
	manager.registryKey = manager.root + "\x00" + manager.profileID()
	manager.configuration = manager.registryKey
	activeManagers.Lock()
	previous := activeManagers.values[manager.registryKey]
	activeManagers.values[manager.registryKey] = manager
	activeManagers.Unlock()
	if previous != nil && previous != manager {
		previous.close(false)
	}
	if errRestore := manager.restoreWorkers(); errRestore != nil {
		log.WithError(errRestore).Warn("claude desktop telemetry: persisted queues were not fully restored")
	}
	return manager
}

func cloneSDKEnvironment(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneSDKInputBetas(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source)+1)
	for model, betas := range source {
		result[model] = append([]string(nil), betas...)
	}
	return result
}

func (m *Manager) profileID() string {
	if m == nil || m.bundle == nil {
		return ""
	}
	return m.bundle.ProfileID
}

func (m *Manager) Enabled() bool {
	return m != nil && m.bundle != nil && m.profile.SchemaVersion == 2 && m.sdkProfile.SchemaVersion == 1 && len(m.auxiliaryDeliveries) == 5 && m.root != ""
}

// Activate attaches the current credential to every restorable delivery
// worker, persists the evidence-backed application-lifetime Sentry session,
// and wakes pending queues. Request/session telemetry remains tied to its
// observable request facts.
func (m *Manager) Activate(auth *cliproxyauth.Auth) error {
	if !m.Enabled() {
		return nil
	}
	var joined error
	workers := make(map[string]*accountWorker, len(m.auxiliaryDeliveries)+2)
	for _, delivery := range append([]deliveryProfile{m.rendererDelivery, m.sdkDelivery}, m.auxiliaryDeliveryList()...) {
		worker, errWorker := m.workerForDelivery(auth, delivery)
		if errWorker != nil {
			if len(delivery.runtimeMaterials) > 0 {
				m.setEndpointState(delivery.endpointRole, "awaiting-enrollment-material", "enrolled account does not contain the required encrypted runtime material")
				continue
			}
			joined = errors.Join(joined, fmt.Errorf("activate %s telemetry: %w", delivery.endpointRole, errWorker))
			continue
		}
		workers[delivery.endpointRole] = worker
		m.setEndpointState(delivery.endpointRole, "ready", "")
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}
	m.runDesktopActivationHooks(workers[m.rendererDelivery.endpointRole]) // W5: native app-ready main-process facts (Windows elevation).
	if worker := workers[segmentRole]; worker != nil {
		if errRuntime := m.ensureSegmentIdentified(worker); errRuntime != nil {
			worker.recordQueueFailure(errRuntime)
			joined = errors.Join(joined, fmt.Errorf("activate %s runtime telemetry: %w", segmentRole, errRuntime))
		}
	}
	if worker := workers[m.sdkDelivery.endpointRole]; worker != nil {
		if errRuntime := m.ensureSDKRuntimeStarted(worker); errRuntime != nil {
			worker.recordQueueFailure(errRuntime)
			joined = errors.Join(joined, fmt.Errorf("activate %s runtime telemetry: %w", m.sdkDelivery.endpointRole, errRuntime))
		}
	}
	if worker := workers[sentryRole]; worker != nil {
		if errRuntime := worker.ensureAuxiliaryRuntimeStarted(m.ctx); errRuntime != nil {
			worker.recordQueueFailure(errRuntime)
			joined = errors.Join(joined, fmt.Errorf("activate %s runtime telemetry: %w", sentryRole, errRuntime))
		}
	}
	return joined
}

func (m *Manager) auxiliaryDeliveryList() []deliveryProfile {
	if m == nil {
		return nil
	}
	roles := make([]string, 0, len(m.auxiliaryDeliveries))
	for role := range m.auxiliaryDeliveries {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	deliveries := make([]deliveryProfile, 0, len(roles))
	for _, role := range roles {
		deliveries = append(deliveries, m.auxiliaryDeliveries[role])
	}
	return deliveries
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.close(true)
}

// Quarantine stops delivery workers without generating normal application
// shutdown events. Persisted queue items remain bound to their original
// account and egress revision for an explicit recovery decision.
func (m *Manager) Quarantine() {
	if m == nil {
		return
	}
	m.shutdown(false, true)
}

// PrepareQuarantine freezes production and cancels both live and final-flush
// requests before waiting on any worker. It also interrupts an ongoing Close.
func (m *Manager) PrepareQuarantine() {
	if m == nil {
		return
	}
	m.freezeOnShutdown.Store(true)
	if m.abortShutdown != nil {
		m.abortShutdown()
	}
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Manager) close(unregister bool) {
	m.shutdown(true, unregister)
}

func (m *Manager) shutdown(emitLifecycle, unregister bool) {
	if !emitLifecycle {
		m.PrepareQuarantine()
	}
	m.closeOnce.Do(func() {
		if emitLifecycle && !m.freezeOnShutdown.Load() {
			m.emitAppQuitLifecycle()
			m.emitSDKShutdownPending()
			m.emitAuxiliaryRuntimeStopped()
		}
		if m.cancel != nil {
			m.updateMu.Lock()
			m.mu.Lock()
			m.cancel()
			m.mu.Unlock()
			m.updateMu.Unlock()
		}
		m.wg.Wait()
	})
	if unregister {
		activeManagers.Lock()
		if activeManagers.values[m.registryKey] == m {
			delete(activeManagers.values, m.registryKey)
		}
		activeManagers.Unlock()
	}
}

func (m *Manager) BeginRequest(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts) *RequestSpan {
	if !m.Enabled() || facts.Role == claudeprofile.RoleCountTokens {
		return &RequestSpan{}
	}
	facts.SessionID = strings.TrimSpace(facts.SessionID)
	facts.PromptID = strings.TrimSpace(facts.PromptID)
	facts.ClientRequestID = strings.TrimSpace(facts.ClientRequestID)
	facts.Model = strings.TrimSpace(facts.Model)
	if id, errParse := uuid.Parse(strings.TrimSpace(facts.ParentPromptID)); errParse == nil && id != uuid.Nil {
		facts.ParentPromptID = id.String()
	} else {
		facts.ParentPromptID = ""
	}
	// Session and prompt identities are generated by Claude Desktop itself, not
	// required caller inputs. Keep the telemetry boundary safe for future call
	// sites instead of silently disabling a span when either identity is absent.
	if facts.SessionID == "" {
		facts.SessionID = uuid.New().String()
	}
	if facts.PromptID == "" {
		facts.PromptID = uuid.New().String()
	}
	if facts.ClientRequestID == "" {
		facts.ClientRequestID = uuid.New().String()
	}
	if facts.StartedAt.IsZero() {
		facts.StartedAt = m.now()
	}
	if facts.ChainStartedAt.IsZero() || facts.ChainStartedAt.After(facts.StartedAt) {
		facts.ChainStartedAt = facts.StartedAt
	}
	if facts.Attempt < 1 {
		facts.Attempt = 1
	}
	// Model is a required Anthropic request field and must be rejected by the
	// request boundary with a 400 response; telemetry must never invent it.
	if facts.Model == "" {
		log.Error("claude desktop telemetry: request model is empty after request validation")
		return &RequestSpan{}
	}
	span := &RequestSpan{manager: m, facts: facts, auxiliaryWorkers: make(map[string]*accountWorker, len(m.auxiliaryDeliveries))}
	sdkWorker, errSDKWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errSDKWorker != nil {
		log.WithError(errSDKWorker).Warn("claude desktop SDK telemetry: account runtime unavailable")
	} else {
		span.sdkWorker = sdkWorker
	}
	for role, delivery := range m.auxiliaryDeliveries {
		worker, errWorker := m.workerForDelivery(auth, delivery)
		if errWorker != nil {
			m.setEndpointState(role, "awaiting-enrollment-material", "enrolled account does not contain the required encrypted runtime material")
			log.WithError(errWorker).WithField("endpoint_role", role).Debug("claude desktop auxiliary telemetry: account runtime unavailable")
			continue
		}
		m.setEndpointState(role, "ready", "")
		span.auxiliaryWorkers[role] = worker
	}
	if facts.Role != claudeprofile.RoleMain {
		return span
	}
	if facts.QueryLifetime != nil && facts.QueryLifetime.Err() != nil {
		return span
	}
	worker, errWorker := m.workerFor(auth)
	if errWorker != nil {
		log.WithError(errWorker).Warn("claude desktop renderer telemetry: account runtime unavailable")
		return span
	}
	span.worker = worker
	if facts.Prompt == nil || facts.Prompt.Identity().StartsPrompt {
		if !m.beginRendererRequest(span) {
			span.worker = nil
			return span
		}
	}
	firstTurn, errSession := worker.ensureSessionInitialized(ctx, facts)
	if errSession != nil {
		worker.recordQueueFailure(errSession)
		log.WithError(errSession).Warn("claude desktop renderer telemetry: session event was not persisted")
	}
	span.firstTurn = firstTurn
	if facts.Prompt != nil {
		facts.Prompt.ObserveSessionInitialized(firstTurn)
	}
	// Consecutive-request renderer analytics (desktop_renderer_session.go).
	m.runRendererRequestHooks(ctx, span)
	if rumWorker := span.auxiliaryWorkers[datadogRUMRole]; rumWorker != nil {
		if _, errRUMSession := rumWorker.ensureRUMSessionInitialized(ctx, facts); errRUMSession != nil {
			rumWorker.recordQueueFailure(errRUMSession)
			log.WithError(errRUMSession).Warn("claude desktop Datadog RUM telemetry: session-start view was not persisted")
		}
	}
	if facts.Prompt != nil && !facts.Prompt.Identity().StartsPrompt {
		return span
	}
	cliSessionID := any(facts.SessionID)
	if firstTurn {
		cliSessionID = nil
	}
	metadata := map[string]any{
		"session_id":        rendererRecordID(facts),
		"cli_session_id":    cliSessionID,
		"user_message_uuid": facts.PromptID,
		"is_ssh":            false,
		"backend_kind":      "local",
		"renderer_surface":  "epitaxy",
		"input_origin":      "user",
	}
	if errEnqueue := worker.enqueueProjected(ctx, FactRequestStarted, facts.SessionID, facts.ClientRequestID, metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: request-start event was not persisted")
	}
	return span
}

// BeginLineage reserves an account- and profile-bound position for a Desktop
// session and returns the last committed Anthropic request-id for that session.
// The reservation is durable before this method returns.
func (m *Manager) BeginLineage(auth *cliproxyauth.Auth, sessionID string) (*LineageToken, string, error) {
	if !m.Enabled() {
		return nil, "", nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, "", fmt.Errorf("Claude Desktop session id is empty")
	}
	worker, errWorker := m.workerFor(auth)
	if errWorker != nil {
		return nil, "", errWorker
	}
	return worker.beginLineage(sessionID)
}

func (m *Manager) finishRequest(ctx context.Context, span *RequestSpan, category, errorClass string) {
	m.finishRequestAt(ctx, span, category, errorClass, m.now())
}

func (m *Manager) finishRequestAt(ctx context.Context, span *RequestSpan, category, errorClass string, now time.Time) {
	if span == nil {
		return
	}
	duration := now.Sub(span.facts.StartedAt)
	if duration < 0 {
		duration = 0
	}
	if span.sdkWorker != nil {
		if errSDK := m.finishSDKRequest(span, category, errorClass, duration); errSDK != nil {
			span.sdkWorker.recordQueueFailure(errSDK)
			log.WithError(errSDK).Warn("claude desktop SDK telemetry: request-outcome event was not persisted")
		}
		if errTurn := m.finishSDKPrompt(span); errTurn != nil {
			span.sdkWorker.recordQueueFailure(errTurn)
			log.WithError(errTurn).Warn("claude desktop SDK telemetry: prompt end event was not persisted")
		}
		if category == "" && errorClass == "" {
			if errTitle := m.finishSDKTitle(span, category, errorClass); errTitle != nil {
				span.sdkWorker.recordQueueFailure(errTitle)
				log.WithError(errTitle).Warn("claude desktop SDK telemetry: title event was not persisted")
			}
		}
	}
	m.finishAuxiliaryRequest(ctx, span, category, errorClass, duration)
	m.finishRendererOutcome(ctx, span, category, errorClass, now, duration)
}

func (m *Manager) finishRendererOutcome(ctx context.Context, span *RequestSpan, category, errorClass string, now time.Time, duration time.Duration) {
	if span.facts.QueryID != "" && span.worker != nil {
		m.lifecycleMu.Lock()
		state := m.sessions[rendererSessionKey(span.worker, rendererRecordKey(span.facts))]
		current := state != nil && !state.queryClosed && state.facts.QueryID == span.facts.QueryID
		m.lifecycleMu.Unlock()
		if !current {
			return
		}
	}
	metadata := map[string]any{
		"session_id":                  rendererRecordID(span.facts),
		"cli_session_id":              span.facts.SessionID,
		"user_message_uuid":           span.facts.PromptID,
		"model":                       span.facts.Model,
		"is_ssh":                      false,
		"backend_kind":                "local",
		"renderer_surface":            "epitaxy",
		"input_origin":                "user",
		"spawn_source":                "cold",
		"cycle_health":                "healthy",
		"had_first_response":          !span.firstByte.IsZero(),
		"is_first_turn":               span.firstTurn,
		"seconds_to_outcome":          int64(duration.Round(time.Second) / time.Second),
		"ms_to_first_token":           int64(0),
		"input_tokens":                span.usage.InputTokens,
		"cache_creation_input_tokens": span.usage.CacheCreationInputTokens,
		"cache_read_input_tokens":     span.usage.CacheReadInputTokens,
	}
	if span.facts.TranscriptSize != nil {
		metadata["transcript_size_bytes"] = *span.facts.TranscriptSize
	}
	metadata["permission_mode"] = defaultString(span.facts.PermissionMode, "default")
	if !span.firstByte.IsZero() {
		metadata["ms_to_first_token"] = span.firstByte.Sub(span.facts.StartedAt).Milliseconds()
	}
	fact := FactRequestSucceeded
	if category != "" || errorClass != "" {
		fact = FactRequestFailed
		metadata["cycle_health"] = "unhealthy"
		metadata["unhealthy_reason"] = normalizeErrorCategory(category)
		metadata["error_category"] = normalizeErrorCategory(errorClass)
	}
	if span.worker != nil {
		emitOutcome := true
		if prompt := span.facts.Prompt; prompt != nil {
			snapshot := prompt.Snapshot()
			if snapshot.SDK.CancelledStreaming {
				// Desktop closes an explicitly interrupted cycle as healthy;
				// this is independent of the SDK result's is_error=true.
				fact = FactRequestSucceeded
				metadata["cycle_health"] = "healthy"
				delete(metadata, "unhealthy_reason")
				delete(metadata, "error_category")
			}
			emitOutcome = prompt.CompletedPrompt() || snapshot.Failed
			m.recordPromptIssue(span, category != "" || errorClass != "")
			metadata["seconds_to_outcome"] = maxInt64(0, int64(snapshot.FinishedAt.Sub(snapshot.StartedAt).Round(time.Second)/time.Second))
			metadata["is_first_turn"] = snapshot.FirstSessionPrompt
			metadata["had_first_response"] = !snapshot.FirstByteAt.IsZero()
			if !snapshot.FirstByteAt.IsZero() {
				metadata["ms_to_first_token"] = maxInt64(0, snapshot.FirstByteAt.Sub(snapshot.StartedAt).Milliseconds())
			}
		}
		if emitOutcome {
			if errEnqueue := span.worker.enqueueProjected(ctx, fact, span.facts.SessionID, span.facts.ClientRequestID, metadata); errEnqueue != nil {
				span.worker.recordQueueFailure(errEnqueue)
				log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: request-outcome event was not persisted")
			}
		}
	}
	if span.facts.Prompt == nil || span.facts.Prompt.CompletedPrompt() || span.facts.Prompt.Snapshot().Failed {
		m.finishRendererRequest(span, now)
	}
}

func rendererSessionKey(worker *accountWorker, sessionID string) string {
	if worker == nil {
		return ""
	}
	return worker.binding.BindingRevision + "\x00" + strings.TrimSpace(sessionID)
}

func (m *Manager) beginRendererRequest(span *RequestSpan) bool {
	if m == nil || span == nil || span.worker == nil || span.facts.Role != claudeprofile.RoleMain {
		return false
	}
	key := rendererSessionKey(span.worker, rendererRecordKey(span.facts))
	if key == "" {
		return false
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if span.facts.QueryLifetime != nil && span.facts.QueryLifetime.Err() != nil {
		return false
	}
	state := m.sessions[key]
	if state != nil && state.queryClosed && span.facts.QueryID != "" && state.facts.QueryID == span.facts.QueryID {
		return false
	}
	if state == nil {
		m.nextSessionSequence++
		state = &rendererSessionState{worker: span.worker, sequence: m.nextSessionSequence, startedAt: span.facts.StartedAt}
		m.sessions[key] = state
	}
	if state.queryClosed || (span.facts.QueryID != "" && state.facts.QueryID != span.facts.QueryID) {
		state.pendingRequests = 0
		state.queryClosed = false
	}
	state.facts = span.facts
	state.rumWorker = span.auxiliaryWorkers[datadogRUMRole]
	if state.pendingRequests == 0 {
		state.pendingStartedAt = span.facts.StartedAt
		state.pendingHadFirstResponse = false
	}
	state.pendingRequests++
	state.lastActivityAt = span.facts.StartedAt
	m.lastSessionKey = key
	return true
}

func (m *Manager) observeRendererFirstByte(span *RequestSpan) {
	if m == nil || span == nil || span.worker == nil || span.facts.Role != claudeprofile.RoleMain {
		return
	}
	key := rendererSessionKey(span.worker, rendererRecordKey(span.facts))
	m.lifecycleMu.Lock()
	if state := m.sessions[key]; state != nil && !state.queryClosed && state.facts.QueryID == span.facts.QueryID && state.pendingRequests > 0 {
		state.pendingHadFirstResponse = true
	}
	m.lifecycleMu.Unlock()
}

func (m *Manager) finishRendererRequest(span *RequestSpan, at time.Time) {
	if m == nil || span == nil || span.worker == nil || span.facts.Role != claudeprofile.RoleMain {
		return
	}
	key := rendererSessionKey(span.worker, rendererRecordKey(span.facts))
	m.lifecycleMu.Lock()
	if state := m.sessions[key]; state != nil && !state.queryClosed && state.facts.QueryID == span.facts.QueryID {
		if state.pendingRequests > 0 {
			state.pendingRequests--
		}
		state.lastActivityAt = at
		state.facts = span.facts
		if state.pendingRequests == 0 {
			state.pendingStartedAt = time.Time{}
			state.pendingHadFirstResponse = false
		}
		m.lastSessionKey = key
	}
	m.lifecycleMu.Unlock()
}

func (m *Manager) emitAppQuitLifecycle() {
	if m == nil || !m.Enabled() {
		return
	}
	type shutdownSession struct {
		key   string
		state rendererSessionState
	}
	m.lifecycleMu.Lock()
	sessions := make([]shutdownSession, 0, len(m.sessions))
	for key, state := range m.sessions {
		if state == nil || state.worker == nil {
			continue
		}
		sessions = append(sessions, shutdownSession{key: key, state: *state})
	}
	lastSessionKey := m.lastSessionKey
	m.lifecycleMu.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].state.sequence < sessions[j].state.sequence })
	if len(sessions) == 0 {
		return
	}
	now := m.now()
	for _, session := range sessions {
		state := session.state
		if state.queryClosed {
			continue
		}
		metadata := map[string]any{
			"session_id":                    rendererRecordID(state.facts),
			"cli_session_id":                state.facts.SessionID,
			"had_pending_cycle":             state.pendingRequests > 0,
			"pending_had_first_response":    nil,
			"pending_seconds":               nil,
			"is_ssh":                        false,
			"backend_kind":                  "local",
			"trigger":                       "app_quit",
			"armed_cron_count":              0,
			"armed_durable_cron_count":      0,
			"had_live_wakeup":               false,
			"wakeup_in_ms":                  nil,
			"losable_background_task_count": 0,
		}
		if state.pendingRequests > 0 {
			metadata["pending_had_first_response"] = state.pendingHadFirstResponse
			pendingFor := now.Sub(state.pendingStartedAt)
			if pendingFor < 0 {
				pendingFor = 0
			}
			metadata["pending_seconds"] = int64(pendingFor.Round(time.Second) / time.Second)
		}
		for key, value := range m.rendererCLIProcessMetadata() {
			metadata[key] = value
		}
		if errEnqueue := state.worker.enqueueProjected(context.Background(), FactSessionStopped, state.facts.SessionID, state.facts.ClientRequestID, metadata); errEnqueue != nil {
			state.worker.recordQueueFailure(errEnqueue)
			log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: app-quit session event was not persisted")
		}
		if state.rumWorker != nil {
			startedAt := state.startedAt
			if startedAt.IsZero() {
				startedAt = state.facts.StartedAt
			}
			duration := now.Sub(startedAt)
			if duration < 0 {
				duration = 0
			}
			payload, eventName, errProject := m.projectRUM(state.rumWorker, FactSessionStopped, state.facts, duration, "", "")
			m.enqueueAuxiliary(context.Background(), state.rumWorker, FactSessionStopped, eventName, state.facts, payload, errProject)
		}
	}
	for _, session := range sessions {
		if session.key != lastSessionKey {
			continue
		}
		state := session.state
		visibility := map[string]any{
			"session_id":       rendererRecordID(state.facts),
			"is_visible":       false,
			"has_active_query": state.pendingRequests > 0,
			"is_ssh":           false,
			"backend_kind":     "local",
		}
		if errEnqueue := state.worker.enqueueProjected(context.Background(), FactSessionVisibility, state.facts.SessionID, state.facts.ClientRequestID, visibility); errEnqueue != nil {
			state.worker.recordQueueFailure(errEnqueue)
			log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: app-quit visibility event was not persisted")
		}
		lastActivityAt := state.lastActivityAt
		if lastActivityAt.IsZero() {
			lastActivityAt = state.facts.StartedAt
		}
		idleFor := now.Sub(lastActivityAt)
		if idleFor < 0 {
			idleFor = 0
		}
		idle := map[string]any{
			"session_id":                  rendererRecordID(state.facts),
			"timeout_ms":                  900000,
			"configured_timeout_ms":       900000,
			"seconds_since_last_activity": int64(idleFor.Round(time.Second) / time.Second),
		}
		if errEnqueue := state.worker.enqueueProjected(context.Background(), FactSessionIdleTimeout, state.facts.SessionID, state.facts.ClientRequestID, idle); errEnqueue != nil {
			state.worker.recordQueueFailure(errEnqueue)
			log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: app-quit idle event was not persisted")
		}
		break
	}
}

func (m *Manager) rendererCLIProcessMetadata() map[string]any {
	metadata := make(map[string]any)
	if m == nil || m.sdkProcessProvider == nil {
		return metadata
	}
	snapshot, ok := m.sdkProcessProvider()
	if !ok {
		return metadata
	}
	uptime := snapshot.UptimeSeconds + m.now().Sub(m.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	metadata["cli_uptime_s"] = int64(uptime)
	metadata["cli_rss_bytes"] = snapshot.RSS
	metadata["cli_footprint_bytes"] = snapshot.FootprintBytes
	metadata["cli_commit_bytes"] = snapshot.CommitBytes
	metadata["cli_peak_footprint_bytes"] = snapshot.PeakFootprintBytes
	metadata["cli_mem_sample_age_ms"] = snapshot.MemorySampleAgeMS
	return metadata
}

func (m *Manager) workerFor(auth *cliproxyauth.Auth) (*accountWorker, error) {
	return m.workerForDelivery(auth, m.rendererDelivery)
}

func (m *Manager) setEndpointState(role, status, reason string) {
	if m == nil || strings.TrimSpace(role) == "" {
		return
	}
	m.endpointMu.Lock()
	m.endpointStates[role] = EndpointStatus{Role: role, Status: status, Reason: reason}
	m.endpointMu.Unlock()
}

func (m *Manager) workerForDelivery(auth *cliproxyauth.Auth, delivery deliveryProfile) (*accountWorker, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if _, errMaterials := runtimeMaterialsForDelivery(auth, delivery); errMaterials != nil {
		return nil, errMaterials
	}
	binding, errBinding := m.bindingForDelivery(auth, delivery)
	if errBinding != nil {
		return nil, errBinding
	}
	key := delivery.endpointRole + "\x00" + binding.BindingRevision
	m.mu.RLock()
	worker := m.workers[key]
	m.mu.RUnlock()
	if worker != nil {
		worker.updateAuth(auth)
		return worker, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if worker = m.workers[key]; worker != nil {
		worker.updateAuth(auth)
		return worker, nil
	}
	worker, errWorker := newAccountWorker(m, binding, delivery, auth)
	if errWorker != nil {
		return nil, errWorker
	}
	m.workers[key] = worker
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		worker.run(m.ctx)
	}()
	return worker, nil
}

func (m *Manager) bindingFor(auth *cliproxyauth.Auth) (Binding, error) {
	return m.bindingForDelivery(auth, m.rendererDelivery)
}

func (m *Manager) bindingForDelivery(auth *cliproxyauth.Auth, delivery deliveryProfile) (Binding, error) {
	if auth == nil {
		return Binding{}, fmt.Errorf("auth is nil")
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return Binding{}, errEnrollment
	}
	effectiveProxyURL := strings.TrimSpace(auth.ProxyURL)
	if effectiveProxyURL == "" {
		effectiveProxyURL = m.globalProxyURL
	}
	egressRevision := scopeRevision("telemetry-egress-v1", effectiveProxyURL)
	materialRevision := ""
	if materials, errMaterials := runtimeMaterialsForDelivery(auth, delivery); errMaterials == nil && len(materials) > 0 {
		keys := make([]string, 0, len(materials))
		for key := range materials {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		values := make([]string, 0, len(keys)*2)
		for _, key := range keys {
			values = append(values, key, materials[key])
		}
		materialRevision = scopeRevision("telemetry-runtime-material-v1", values...)
	}
	destinationValues := []string{strings.TrimSpace(delivery.endpoint)}
	if materialRevision != "" {
		destinationValues = append(destinationValues, materialRevision)
	}
	destinationRevision := scopeRevision("telemetry-destination-v1", destinationValues...)
	transportRevision := strings.TrimSpace(delivery.transportRevision)
	digest := bindingDigest(auth.ID, enrollment.AccountUUID, enrollment.OrganizationUUID, enrollment.DeviceID, m.bundle.ProfileID, m.bundle.DesktopVersion, egressRevision, destinationRevision, transportRevision)
	revision := hex.EncodeToString(digest[:])
	runtimeUUID := uuid.NewSHA1(uuid.NameSpaceOID, digest[:]).String()
	return Binding{
		AuthID:              auth.ID,
		AccountUUID:         enrollment.AccountUUID,
		OrganizationUUID:    enrollment.OrganizationUUID,
		DeviceID:            enrollment.DeviceID,
		ProfileID:           m.bundle.ProfileID,
		DesktopVersion:      m.bundle.DesktopVersion,
		EgressProxyURL:      effectiveProxyURL,
		EgressRevision:      egressRevision,
		DestinationRevision: destinationRevision,
		TransportRevision:   transportRevision,
		BindingRevision:     revision,
		RuntimeUUID:         runtimeUUID,
	}, nil
}

func sdkTransportRevision(profile claudeprofile.SDKTelemetryProfile) string {
	return scopeRevision("sdk-event-logging-transport-v1", profile.TransportProfile, strings.Join(profile.HeaderOrder, "\x00"))
}

func auxiliaryTransportRevision(profile claudeprofile.AuxiliaryTelemetryProfile) string {
	return scopeRevision("auxiliary-telemetry-transport-v1", profile.EndpointRole, profile.TransportProfile, profile.Protocol, strings.Join(profile.HeaderOrder, "\x00"))
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func runtimeMaterialsForDelivery(auth *cliproxyauth.Auth, delivery deliveryProfile) (map[string]string, error) {
	if len(delivery.runtimeMaterials) == 0 {
		return nil, nil
	}
	if auth == nil || auth.Metadata == nil {
		return nil, fmt.Errorf("telemetry runtime materials for %q are unavailable", delivery.endpointRole)
	}
	materials, errMaterials := claudedesktop.TelemetryMaterialsFromMetadata(auth.Metadata)
	if errMaterials != nil {
		return nil, fmt.Errorf("telemetry runtime materials for %q are unavailable: %w", delivery.endpointRole, errMaterials)
	}
	resolved := make(map[string]string, len(delivery.runtimeMaterials))
	for semantic, materialName := range delivery.runtimeMaterials {
		value := materials.Value(materialName)
		if value == "" {
			return nil, fmt.Errorf("telemetry runtime material %q for %q is unavailable", materialName, delivery.endpointRole)
		}
		resolved[semantic] = value
	}
	return resolved, nil
}

func bindingDigest(authID, accountUUID, organizationUUID, deviceID, profileID, desktopVersion, egressRevision, destinationRevision, transportRevision string) [32]byte {
	material := strings.Join([]string{authID, accountUUID, organizationUUID, deviceID, profileID, desktopVersion, egressRevision, destinationRevision, transportRevision}, "\x00")
	return sha256.Sum256([]byte(material))
}

func scopeRevision(namespace string, values ...string) string {
	material := append([]string{namespace}, values...)
	digest := sha256.Sum256([]byte(strings.Join(material, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (m *Manager) project(binding Binding, fact, sessionID, clientRequestID string, metadata map[string]any) (Envelope, error) {
	eventProfile, ok := m.profile.Events[fact]
	if !ok || strings.TrimSpace(eventProfile.EventName) == "" {
		return Envelope{}, fmt.Errorf("telemetry fact %q is not mapped by profile", fact)
	}
	for _, required := range eventProfile.RequiredFacts {
		if !hasRequiredFact(required, metadata) {
			return Envelope{}, fmt.Errorf("telemetry fact %q is missing required field %q", fact, required)
		}
	}
	merged := make(map[string]any, len(m.profile.BaseMetadata)+len(metadata)+10)
	for key, value := range m.profile.BaseMetadata {
		if isUpdateCheckFact(fact) && (key == "renderer_surface" || key == "backend_kind" || key == "is_ssh") {
			continue
		}
		merged[key] = value
	}
	for key, value := range rendererRuntimeMetadata(m.rendererSnapshot, m.hostSnapshot) {
		merged[key] = value
	}
	for key, value := range metadata {
		merged[key] = value
	}
	merged["app_version"] = m.profile.Runtime.ClientVersion
	merged["commit_hash"] = m.profile.DesktopCommit
	merged["app_session_id"] = m.appSessionID
	merged["app_uptime_seconds"] = int64(m.now().Sub(m.startedAt).Seconds())
	merged["organization_id"] = binding.OrganizationUUID
	metadataJSON, errMetadata := json.Marshal(merged)
	if errMetadata != nil {
		return Envelope{}, fmt.Errorf("marshal Desktop telemetry metadata: %w", errMetadata)
	}
	timestamp := m.now().UTC()
	payload := map[string]any{
		"event_type": m.profile.EventType,
		"event_data": map[string]any{
			"event_name":      eventProfile.EventName,
			"timestamp":       timestamp.Format("2006-01-02T15:04:05.000Z"),
			"user_properties": map[string]any{"user_id": binding.AccountUUID},
			"metadata":        string(metadataJSON),
			"auth": map[string]any{
				"organization_uuid": binding.OrganizationUUID,
				"account_uuid":      binding.AccountUUID,
			},
		},
	}
	payloadJSON, errPayload := json.Marshal(payload)
	if errPayload != nil {
		return Envelope{}, fmt.Errorf("marshal Desktop telemetry event: %w", errPayload)
	}
	return Envelope{
		Version:         1,
		EventUUID:       uuid.New().String(),
		EndpointRole:    m.profile.EndpointRole,
		CatalogFact:     fact,
		CatalogEvent:    eventProfile.EventName,
		OccurredAt:      timestamp.Format(time.RFC3339Nano),
		Binding:         binding,
		SessionID:       sessionID,
		ClientRequestID: clientRequestID,
		Payload:         payloadJSON,
	}, nil
}

func hasRequiredFact(name string, metadata map[string]any) bool {
	switch name {
	case "duration":
		_, ok := metadata["seconds_to_outcome"]
		return ok
	case "error_category":
		value, _ := metadata["error_category"].(string)
		return strings.TrimSpace(value) != ""
	default:
		value, ok := metadata[name]
		if !ok || value == nil {
			return false
		}
		if text, okText := value.(string); okText {
			return strings.TrimSpace(text) != ""
		}
		return true
	}
}

func normalizeErrorCategory(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	var normalized strings.Builder
	lastUnderscore := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			normalized.WriteRune(char)
			lastUnderscore = false
		case normalized.Len() > 0 && !lastUnderscore:
			normalized.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(normalized.String(), "_")
}

func safeErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var statusError interface{ StatusCode() int }
	if errors.As(err, &statusError) {
		switch status := statusError.StatusCode(); {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return "authentication_error"
		case status == http.StatusTooManyRequests:
			return "rate_limit"
		case status >= 400 && status < 500:
			return "invalid_request"
		case status >= 500:
			return "server_error"
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "timeout"
		}
		return "network_error"
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return "network_error"
	}
	return "upstream_error"
}

func authIDHash(binding Binding) string {
	digest := sha256.Sum256([]byte(binding.AuthID))
	return hex.EncodeToString(digest[:12])
}

func StatePath(configured, authDir string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return ""
	}
	if strings.HasPrefix(authDir, "~/") || strings.HasPrefix(authDir, "~\\") {
		if home, errHome := os.UserHomeDir(); errHome == nil {
			authDir = filepath.Join(home, authDir[2:])
		}
	}
	return filepath.Join(authDir, "claude-desktop-state")
}

func StatusSnapshots() []Status {
	activeManagers.RLock()
	managers := make([]*Manager, 0, len(activeManagers.values))
	for _, manager := range activeManagers.values {
		managers = append(managers, manager)
	}
	activeManagers.RUnlock()
	statuses := make([]Status, 0, len(managers))
	for _, manager := range managers {
		statuses = append(statuses, manager.Status())
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ProfileID < statuses[j].ProfileID })
	return statuses
}

func FlushAll(ctx context.Context) error {
	activeManagers.RLock()
	managers := make([]*Manager, 0, len(activeManagers.values))
	for _, manager := range activeManagers.values {
		managers = append(managers, manager)
	}
	activeManagers.RUnlock()
	var joined error
	for _, manager := range managers {
		if errFlush := manager.Flush(ctx); errFlush != nil {
			joined = errors.Join(joined, errFlush)
		}
	}
	return joined
}

func RetryDeadLettersAll() (int, error) {
	activeManagers.RLock()
	managers := make([]*Manager, 0, len(activeManagers.values))
	for _, manager := range activeManagers.values {
		managers = append(managers, manager)
	}
	activeManagers.RUnlock()
	total := 0
	var joined error
	for _, manager := range managers {
		count, errRetry := manager.RetryDeadLetters()
		total += count
		if errRetry != nil {
			joined = errors.Join(joined, errRetry)
		}
	}
	return total, joined
}

func (m *Manager) Status() Status {
	startedAt := m.startedAt.UTC()
	sessionDigest := sha256.Sum256([]byte(m.appSessionID))
	status := Status{
		Enabled:              m.Enabled(),
		Required:             m.profile.Required || m.sdkProfile.Required,
		ProfileID:            m.profileID(),
		TransportRevision:    m.profile.Transport.Revision,
		TransportProtocol:    m.profile.Transport.Protocol,
		TransportEvidence:    m.profile.Transport.EvidenceStatus,
		UserAgentPolicy:      m.profile.Transport.UserAgentPolicy,
		StatePathConfigured:  m.root != "",
		MachineProfileID:     m.machineProfileID,
		AppSessionIDHash:     hex.EncodeToString(sessionDigest[:8]),
		RuntimeStartedAt:     &startedAt,
		Accounts:             []AccountStatus{},
		DeliveryEndpoints:    []DeliveryEndpointStatus{},
		ObservedEndpoints:    []ObservedEndpointStatus{},
		UnsupportedEndpoints: make([]EndpointStatus, 0, len(m.profile.Unsupported)),
		LiveEmitterCoverage: LiveEmitterCoverageStatus{
			Status:                       "partial",
			CoveragePolicy:               "observable-fact-mapped-subset",
			LiveEventNames:               []string{},
			RendererRuntimeMetricsSource: "unavailable",
			SDKProcessMetricsSource:      "unavailable",
			TranscriptSizeSource:         "caller-supplied",
			PayloadContractStatus:        "unavailable",
		},
		TransportFidelity: TransportFidelityStatus{
			ClientHelloStatus: m.profile.Transport.EvidenceStatus,
			ClientHelloPreset: m.profile.Transport.ClientHelloPreset,
			HTTP2StreamMode:   "multiplexed",
		},
	}
	if m.rendererSnapshot != nil {
		status.LiveEmitterCoverage.RendererRuntimeMetricsSource = "desktop-companion"
	}
	if m.sdkProcessProvider != nil {
		status.LiveEmitterCoverage.SDKProcessMetricsSource = "provided-snapshot"
	}
	status.LiveEmitterCoverage.ObservedCompanionEventCount = len(claudeprofile.V140609ObservedExecutableEvents())
	if contractSummary, errContracts := claudeprofile.V140609ObservedEventPayloadContractSummaryValue(); errContracts == nil {
		status.LiveEmitterCoverage.PayloadContractStatus = "complete"
		if contractSummary.EndpointOnlyEventCount > 0 {
			status.LiveEmitterCoverage.PayloadContractStatus = "partial"
		}
		status.LiveEmitterCoverage.CapturedPayloadContractEventCount = contractSummary.CapturedContractEventCount
		status.LiveEmitterCoverage.SpecializedPayloadContractEventCount = contractSummary.SpecializedContractEventCount
		status.LiveEmitterCoverage.EndpointOnlyPayloadContractEventCount = contractSummary.EndpointOnlyEventCount
		status.LiveEmitterCoverage.UncapturedPayloadContractEventCount = contractSummary.EndpointOnlyEventCount
		status.LiveEmitterCoverage.PayloadContractArtifact = contractSummary.CapturedSourceArtifact
		status.LiveEmitterCoverage.PayloadContractSHA256 = contractSummary.CapturedSourceSHA256
		status.LiveEmitterCoverage.SpecializedPayloadContractArtifact = contractSummary.SpecializedSourceArtifact
		status.LiveEmitterCoverage.SpecializedPayloadContractSHA256 = contractSummary.SpecializedSourceSHA256
	}
	if m.bundle != nil {
		status.DesktopVersion = m.bundle.DesktopVersion
		status.TransportFidelity.ObservedChromiumVersion = m.profile.Runtime.ChromiumVersion
		evidence := m.bundle.TelemetryEvidence
		status.TelemetryEvidence = &EvidenceStatus{
			SourceManifestSHA256: evidence.SourceManifestSHA256,
			CaptureWindow: EvidenceCaptureWindowStatus{
				FirstCapturedAt: evidence.CaptureWindow.FirstCapturedAt,
				LastCapturedAt:  evidence.CaptureWindow.LastCapturedAt,
			},
			Corpus: EvidenceCorpusStatus{
				FlowCount:               evidence.Corpus.FlowCount,
				HTTPScenarioCount:       evidence.Corpus.HTTPScenarioCount,
				EligibleScenarioCount:   evidence.Corpus.EligibleScenarioCount,
				TelemetryBatchFlowCount: evidence.Corpus.TelemetryBatchFlowCount,
				EventCount:              evidence.Corpus.EventCount,
				EventNameCount:          evidence.Corpus.EventNameCount,
			},
			Artifacts:     make([]EvidenceArtifactStatus, 0, len(evidence.Artifacts)),
			ObservedScope: observedScopeStatus(evidence.EmitterCoverage.ObservedScope),
		}
		status.LiveEmitterCoverage.CapturedEventNameCount = len(evidence.EmitterCoverage.ObservableEventNames)
		status.LiveEmitterCoverage.ObservableEventNameCount = len(evidence.EmitterCoverage.ObservableEventNames)
		status.LiveEmitterCoverage.CoveragePolicy = "event-state-transition-observed-names"
		for _, artifact := range evidence.Artifacts {
			status.TelemetryEvidence.Artifacts = append(status.TelemetryEvidence.Artifacts, EvidenceArtifactStatus{
				Name:   artifact.Name,
				SHA256: artifact.SHA256,
			})
		}
		status.ObservedEndpoints = make([]ObservedEndpointStatus, 0, len(evidence.ObservedEndpoints))
		for _, endpoint := range evidence.ObservedEndpoints {
			status.ObservedEndpoints = append(status.ObservedEndpoints, ObservedEndpointStatus{
				Role:                 endpoint.Role,
				TelemetryClass:       endpoint.TelemetryClass,
				Status:               endpoint.Status,
				TransportProtocol:    endpoint.TransportProtocol,
				BodyCoverage:         endpoint.BodyCoverage,
				FlowCount:            endpoint.FlowCount,
				ScenarioCount:        endpoint.ScenarioCount,
				JSONBodyFlowCount:    endpoint.JSONBodyFlowCount,
				OpaqueBodyFlowCount:  endpoint.OpaqueBodyFlowCount,
				MissingBodyFlowCount: endpoint.MissingBodyFlowCount,
				EventCount:           endpoint.EventCount,
				MaximumBatchEvents:   endpoint.MaximumBatchEvents,
				MaximumBatchBytes:    endpoint.MaximumBatchBytes,
				Reason:               endpoint.Reason,
			})
		}
	}
	declaredEvents := make(map[string]struct{}, len(m.profile.Events)+len(m.sdkProfile.Events)+16)
	for _, event := range m.profile.Events {
		if name := strings.TrimSpace(event.EventName); name != "" {
			declaredEvents[name] = struct{}{}
		}
	}
	for _, event := range m.sdkProfile.Events {
		if name := strings.TrimSpace(event.EventName); name != "" {
			declaredEvents[name] = struct{}{}
		}
	}
	for _, auxiliary := range m.auxiliaryProfiles {
		for _, event := range auxiliary.Events {
			if name := strings.TrimSpace(event.EventName); name != "" {
				declaredEvents[name] = struct{}{}
			}
		}
	}
	status.LiveEmitterCoverage.DeclaredEventNameCount = len(declaredEvents)
	executable := m.executableEndpointEvents()
	liveEvents := make(map[string]struct{}, len(executable))
	for _, event := range executable {
		liveEvents[event.EventName] = struct{}{}
	}
	status.LiveEmitterCoverage.UnverifiedDeclaredEventNames = []string{}
	for name := range declaredEvents {
		if _, ok := liveEvents[name]; !ok {
			status.LiveEmitterCoverage.UnverifiedDeclaredEventNames = append(status.LiveEmitterCoverage.UnverifiedDeclaredEventNames, name)
		}
	}
	sort.Strings(status.LiveEmitterCoverage.UnverifiedDeclaredEventNames)
	status.LiveEmitterCoverage.LiveEndpointEventCount = len(executable)
	if m.bundle != nil {
		observed := m.bundle.TelemetryEvidence.EmitterCoverage.ObservableEndpointEvents
		status.LiveEmitterCoverage.ObservableEndpointEventCount = len(observed)
		if len(observed) > 0 {
			observedPairs := make(map[string]bool, len(observed))
			status.LiveEmitterCoverage.CoveragePolicy = "executable-fact-endpoint-pairs"
			status.LiveEmitterCoverage.LiveEndpointEventCount = 0
			status.LiveEmitterCoverage.EndpointEvents = make([]EndpointEventCoverageStatus, 0, len(observed))
			for _, event := range observed {
				observedPairs[event.EndpointRole+"\x00"+event.EventName] = true
				_, ok := executable[event.EndpointRole+"\x00"+event.EventName]
				status.LiveEmitterCoverage.EndpointEvents = append(status.LiveEmitterCoverage.EndpointEvents, EndpointEventCoverageStatus{
					EndpointRole: event.EndpointRole, EventName: event.EventName, Executable: ok,
				})
				if !ok {
					status.LiveEmitterCoverage.UnmodeledEndpointEventCount++
				} else {
					status.LiveEmitterCoverage.LiveEndpointEventCount++
				}
			}
			for key, event := range executable {
				if !observedPairs[key] {
					status.LiveEmitterCoverage.UncapturedExecutableEndpointEvents = append(status.LiveEmitterCoverage.UncapturedExecutableEndpointEvents,
						EndpointEventCoverageStatus{EndpointRole: event.EndpointRole, EventName: event.EventName, Executable: true})
				}
			}
			sort.Slice(status.LiveEmitterCoverage.UncapturedExecutableEndpointEvents, func(i, j int) bool {
				a, b := status.LiveEmitterCoverage.UncapturedExecutableEndpointEvents[i], status.LiveEmitterCoverage.UncapturedExecutableEndpointEvents[j]
				return a.EndpointRole+"\x00"+a.EventName < b.EndpointRole+"\x00"+b.EventName
			})
		}
	}
	observedNames := map[string]bool{}
	if m.bundle != nil {
		for _, name := range m.bundle.TelemetryEvidence.EmitterCoverage.ObservableEventNames {
			observedNames[name] = true
		}
	}
	status.LiveEmitterCoverage.LiveEventNames = make([]string, 0, len(liveEvents))
	for name := range liveEvents {
		if len(observedNames) > 0 && !observedNames[name] {
			continue
		}
		status.LiveEmitterCoverage.LiveEventNames = append(status.LiveEmitterCoverage.LiveEventNames, name)
	}
	sort.Strings(status.LiveEmitterCoverage.LiveEventNames)
	status.LiveEmitterCoverage.LiveEventNameCount = len(status.LiveEmitterCoverage.LiveEventNames)
	coveredObservableEvents := 0
	if m.bundle != nil {
		for _, eventName := range m.bundle.TelemetryEvidence.EmitterCoverage.ObservableEventNames {
			if _, exists := liveEvents[eventName]; exists {
				coveredObservableEvents++
			}
		}
	}
	status.LiveEmitterCoverage.UnmodeledCapturedEventCount = max(0, status.LiveEmitterCoverage.ObservableEventNameCount-coveredObservableEvents)
	if status.LiveEmitterCoverage.ObservableEndpointEventCount > 0 && status.LiveEmitterCoverage.UnmodeledCapturedEventCount == 0 &&
		status.LiveEmitterCoverage.UnmodeledEndpointEventCount == 0 && status.LiveEmitterCoverage.PayloadContractStatus == "complete" {
		status.LiveEmitterCoverage.Status = "complete"
	}
	for _, endpoint := range m.profile.Unsupported {
		status.UnsupportedEndpoints = append(status.UnsupportedEndpoints, EndpointStatus{Role: endpoint.Role, Status: endpoint.Status, Reason: endpoint.Reason})
	}
	if m.rendererDelivery.endpointRole != "" {
		status.DeliveryEndpoints = append(status.DeliveryEndpoints, DeliveryEndpointStatus{
			Role:              m.rendererDelivery.endpointRole,
			TelemetryClass:    "renderer",
			Required:          m.profile.Required,
			Status:            "ready",
			TransportRevision: m.rendererDelivery.transportRevision,
			TransportProtocol: m.rendererDelivery.protocol,
			TransportEvidence: m.profile.Transport.EvidenceStatus,
			UserAgentPolicy:   m.rendererDelivery.userAgentPolicy,
			AuthPolicy:        m.rendererDelivery.authPolicy,
		})
	}
	if m.sdkDelivery.endpointRole != "" {
		status.DeliveryEndpoints = append(status.DeliveryEndpoints, DeliveryEndpointStatus{
			Role:              m.sdkDelivery.endpointRole,
			TelemetryClass:    "sdk",
			Required:          m.sdkProfile.Required,
			Status:            "ready",
			TransportRevision: m.sdkDelivery.transportRevision,
			TransportProtocol: m.sdkDelivery.protocol,
			UserAgentPolicy:   m.sdkDelivery.userAgentPolicy,
			AuthPolicy:        m.sdkDelivery.authPolicy,
		})
	}
	m.endpointMu.RLock()
	endpointStates := make(map[string]EndpointStatus, len(m.endpointStates))
	for role, endpoint := range m.endpointStates {
		endpointStates[role] = endpoint
	}
	m.endpointMu.RUnlock()
	for index := range status.DeliveryEndpoints {
		if availability, ok := endpointStates[status.DeliveryEndpoints[index].Role]; ok && availability.Status != "" {
			status.DeliveryEndpoints[index].Status = availability.Status
			status.DeliveryEndpoints[index].Reason = availability.Reason
		}
	}
	auxiliaryRoles := make([]string, 0, len(m.auxiliaryDeliveries))
	for role := range m.auxiliaryDeliveries {
		auxiliaryRoles = append(auxiliaryRoles, role)
	}
	sort.Strings(auxiliaryRoles)
	for _, role := range auxiliaryRoles {
		delivery := m.auxiliaryDeliveries[role]
		profile := m.auxiliaryProfiles[role]
		availability := endpointStates[role]
		status.DeliveryEndpoints = append(status.DeliveryEndpoints, DeliveryEndpointStatus{
			Role:              role,
			TelemetryClass:    profile.TelemetryClass,
			Required:          profile.Required,
			Status:            availability.Status,
			Reason:            availability.Reason,
			TransportRevision: delivery.transportRevision,
			TransportProtocol: delivery.protocol,
			UserAgentPolicy:   delivery.userAgentPolicy,
			AuthPolicy:        delivery.authPolicy,
		})
	}
	m.mu.RLock()
	workers := make([]*accountWorker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.mu.RUnlock()
	for _, worker := range workers {
		account := worker.statusSnapshot()
		if account.FactIssues == nil && account.Pending == 0 && account.Sending == 0 && account.DeadLetters == 0 && account.ConsecutiveFailures == 0 && account.LastSuccessAt == nil && account.LastFailureAt == nil {
			continue
		}
		status.Accounts = append(status.Accounts, account)
	}
	m.applyFactIssueStatus(&status)
	sort.Slice(status.Accounts, func(i, j int) bool {
		if status.Accounts[i].AuthIDHash == status.Accounts[j].AuthIDHash {
			return status.Accounts[i].EndpointRole < status.Accounts[j].EndpointRole
		}
		return status.Accounts[i].AuthIDHash < status.Accounts[j].AuthIDHash
	})
	return status
}

func (m *Manager) Flush(ctx context.Context) error {
	m.mu.RLock()
	workers := make([]*accountWorker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.mu.RUnlock()
	var joined error
	for _, worker := range workers {
		if errFlush := worker.flush(ctx); errFlush != nil {
			joined = errors.Join(joined, errFlush)
		}
	}
	return joined
}

func (m *Manager) RetryDeadLetters() (int, error) {
	m.mu.RLock()
	workers := make([]*accountWorker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.mu.RUnlock()
	total := 0
	var joined error
	for _, worker := range workers {
		count, errRetry := worker.retryDeadLetters()
		total += count
		if errRetry != nil {
			joined = errors.Join(joined, errRetry)
		}
	}
	return total, joined
}

func defaultHTTPDoer(string) HTTPDoer {
	return http.DefaultClient
}

func randomID() string {
	data := make([]byte, 8)
	if _, errRead := rand.Read(data); errRead != nil {
		return strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	}
	return hex.EncodeToString(data)
}
