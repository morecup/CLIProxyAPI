package controlplane

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	controlSessionStateVersion = 1
	maximumResponseBytes       = 4 << 20
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type DoerFactory func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error)

type Options struct {
	StatePath    string
	Bundle       *claudeprofile.Bundle
	DoerFactory  DoerFactory
	Now          func() time.Time
	Hostname     func() (string, error)
	WorkingDir   string
	DisableLoops bool
	Credentials  CredentialSource
}

// Manager owns the account-local Claude Desktop remote-control sessions. Its
// failures are observable and retryable, but they never reject message calls.
type Manager struct {
	root                   string
	bundle                 *claudeprofile.Bundle
	profile                claudeprofile.ControlPlaneProfile
	doerFactory            DoerFactory
	now                    func() time.Time
	hostname               func() (string, error)
	workingDir             string
	disableLoops           bool
	credentials            CredentialSource
	ctx                    context.Context
	cancel                 context.CancelFunc
	shutdownCtx            context.Context
	abortShutdown          context.CancelFunc
	closeOnce              sync.Once
	mu                     sync.Mutex
	closed                 bool
	gracefulClose          bool
	abort                  atomic.Bool
	sessions               map[string]*sessionRuntime
	pendingTitles          map[string]string
	wg                     sync.WaitGroup
	placeholderMu          sync.Mutex
	sweepMu                sync.Mutex
	processProbe           func(int) (processIdentity, error)
	placeholderPending     atomic.Int64
	placeholderStoreFailed atomic.Bool
	placeholderSweepFailed atomic.Bool
	placeholderUsed        atomic.Int64
	workerReadWait         func(context.Context, time.Duration) error
	workerReadOrderingWait func(context.Context, <-chan struct{}) error
}

type sessionRuntime struct {
	manager         *Manager
	key             string
	desktopID       string
	queryID         string
	workingDir      string
	ctx             context.Context
	cancel          context.CancelFunc
	unwatch         func() bool
	loops           sync.WaitGroup
	done            chan struct{}
	ready           chan struct{}
	admissionFailed bool
	retireErr       error // Published by closing done.
	initFailed      atomic.Bool
	retireMu        sync.Mutex
	shutdownReason  string
	statePath       string
	opMu            sync.Mutex
	state           controlSessionState
	auth            *cliproxyauth.Auth
	enrollment      claudedesktop.Enrollment
	streamStarted   bool
	// Request initialization belongs to this worker instance, not every API
	// call or human prompt. It is not restored across a process restart.
	requestInitSent         bool
	requestInitConfig       workerInitConfig
	heartOnce               sync.Once
	placeholderGate         func() (bool, error)
	placeholderRegistered   bool
	placeholderSweepStarted bool
	bridgeObserver          BridgeObserver
	bridgeStartObserver     BridgeObserver
	bridgeExpiresIn         int
	bridgeModel             string
	bridgePromptID          string
	bridgeStarted           bool
	remoteOrigin            *claudesessions.RemoteGrant
	remoteRevived           bool
	createdFallback         bool
	bindBridge              func(string) error
	bridgeRecord            *claudesessions.BridgeGrant
	bridgeCheckpoint        *claudesessions.BridgeState
	bridgeTranscript        BridgeTranscriptSink
	bridgeTranscriptFailed  atomic.Bool
	resumeBridge            bool
	inbound                 *InboundConsumer
	ingressMu               sync.Mutex
	inboundSeen             uuidRing
	outboundSeen            uuidRing
	lastSequence            atomic.Int64
	inboundFailed           atomic.Int64
	inboundUnhandled        atomic.Int64
	inboundDispatched       atomic.Int64
	inboundCompleted        atomic.Int64
	inboundCanceled         atomic.Int64
	inboundExecutionFailed  atomic.Int64
	deliveryMu              sync.Mutex
	deliveryPending         []deliveryUpdate
	deliveryWaiting         []deliveryUpdate
	deliveryRunning         bool
	workerRestoration       *WorkerRestoration
	workerRead              *workerReadFlight
	workerStateReadFailed   atomic.Bool
	workerHydrationFailed   atomic.Bool
	epochSuperseded         atomic.Bool
}

type controlSessionState struct {
	Version                  int    `json:"version"`
	LocalSessionID           string `json:"local_session_id"`
	RemoteSessionID          string `json:"remote_session_id,omitempty"`
	ArchiveSessionID         string `json:"archive_session_id,omitempty"`
	Model                    string `json:"model"`
	WorkerEpoch              string `json:"worker_epoch,omitempty"`
	WorkerJWT                string `json:"worker_jwt,omitempty"`
	APIBaseURL               string `json:"api_base_url,omitempty"`
	WorkerJWTExpiresAt       string `json:"worker_jwt_expires_at,omitempty"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	LastActivityAt           string `json:"last_activity_at"`
	WorkerInitialized        bool   `json:"worker_initialized"`
	Archived                 bool   `json:"archived"`
}

type createSessionRequest struct {
	Title  string              `json:"title"`
	Bridge struct{}            `json:"bridge"`
	Tags   []string            `json:"tags"`
	Config createSessionConfig `json:"config"`
}

type createSessionConfig struct {
	CWD   string `json:"cwd"`
	Model string `json:"model"`
}

type createSessionResponse struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
}

type bridgeResponse struct {
	APIBaseURL  string `json:"api_base_url"`
	ExpiresIn   int    `json:"expires_in"`
	WorkerEpoch string `json:"worker_epoch"`
	WorkerJWT   string `json:"worker_jwt"`
}

type heartbeatResponse struct {
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
}

type workerEventEnvelope struct {
	Payload any `json:"payload"`
}

type workerEventRequest struct {
	WorkerEpoch int64                 `json:"worker_epoch"`
	Events      []workerEventEnvelope `json:"events"`
}

type workerShutdownPayload struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Reason    string `json:"reason"`
	SessionID string `json:"session_id"`
	UUID      string `json:"uuid"`
}

// zeroTurnResultPayload is the remote-control stream sentinel, not an SDK
// result aggregate. All 65 original and 3 controlled v140609 worker results
// have zero counters/durations, an empty result and empty modelUsage. Never
// populate these fields from tengu_sdk_result or Messages token usage.
type zeroTurnResultPayload struct {
	Type              string           `json:"type"`
	Subtype           string           `json:"subtype"`
	DurationMS        int              `json:"duration_ms"`
	DurationAPIMS     int              `json:"duration_api_ms"`
	IsError           bool             `json:"is_error"`
	NumTurns          int              `json:"num_turns"`
	Result            string           `json:"result"`
	StopReason        any              `json:"stop_reason"`
	TotalCostUSD      int              `json:"total_cost_usd"`
	Usage             zeroUsagePayload `json:"usage"`
	ModelUsage        map[string]any   `json:"modelUsage"`
	PermissionDenials []any            `json:"permission_denials"`
	SessionID         string           `json:"session_id"`
	UUID              string           `json:"uuid"`
}

type zeroUsagePayload struct {
	OutputTokensDetails      zeroOutputTokenDetails `json:"output_tokens_details"`
	InputTokens              int                    `json:"input_tokens"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens"`
	OutputTokens             int                    `json:"output_tokens"`
	ServerToolUse            zeroServerToolUse      `json:"server_tool_use"`
	ServiceTier              string                 `json:"service_tier"`
	CacheCreation            zeroCacheCreation      `json:"cache_creation"`
	InferenceGeo             string                 `json:"inference_geo"`
	Iterations               []any                  `json:"iterations"`
	Speed                    string                 `json:"speed"`
}

type zeroOutputTokenDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

type zeroServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
	WebFetchRequests  int `json:"web_fetch_requests"`
}

type zeroCacheCreation struct {
	Ephemeral1HInputTokens int `json:"ephemeral_1h_input_tokens"`
	Ephemeral5MInputTokens int `json:"ephemeral_5m_input_tokens"`
}

type statusError struct {
	code            int
	untrustedDevice bool
	staleRelogin    bool
	retryAfter      time.Duration
	conflictReason  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("Claude Desktop control-plane returned HTTP %d", e.code)
}
func (e *statusError) StatusCode() int { return e.code }

func NewManager(options Options) *Manager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	hostname := options.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx, abortShutdown := context.WithCancel(context.Background())
	manager := &Manager{
		root:          strings.TrimSpace(options.StatePath),
		bundle:        options.Bundle,
		doerFactory:   options.DoerFactory,
		now:           now,
		hostname:      hostname,
		workingDir:    strings.TrimSpace(options.WorkingDir),
		disableLoops:  options.DisableLoops,
		credentials:   options.Credentials,
		ctx:           ctx,
		cancel:        cancel,
		shutdownCtx:   shutdownCtx,
		abortShutdown: abortShutdown,
		sessions:      make(map[string]*sessionRuntime),
		pendingTitles: make(map[string]string),
		processProbe:  inspectProcess,
	}
	if manager.bundle != nil {
		manager.profile = manager.bundle.ControlPlane
		if manager.workingDir == "" {
			manager.workingDir = strings.TrimSpace(manager.bundle.Environment.DefaultWorkingDir)
		}
	}
	if manager.root != "" {
		if absolute, errAbs := filepath.Abs(manager.root); errAbs == nil {
			manager.root = absolute
		}
		manager.placeholderMu.Lock()
		_, errLoad := manager.loadPlaceholdersLocked()
		manager.placeholderMu.Unlock()
		if errLoad != nil {
			log.WithError(errLoad).Warn("claude desktop control-plane: placeholder journal is unavailable")
		}
	}
	return manager
}

func (m *Manager) Enabled() bool {
	return m != nil && m.root != "" && m.bundle != nil && m.profile.SchemaVersion == 1 && m.doerFactory != nil
}

// EnsureSession creates and bridges the captured Desktop control-plane session
// on first use, initializes its worker, and keeps its liveness loops active.
func (m *Manager) EnsureSession(ctx context.Context, auth *cliproxyauth.Auth, localSessionID, model string) error {
	if !m.Enabled() {
		return nil
	}
	localSessionID = strings.TrimSpace(localSessionID)
	model = strings.TrimSpace(model)
	if localSessionID == "" || model == "" {
		return fmt.Errorf("Claude Desktop control-plane requires session and model identities")
	}
	if auth == nil {
		return fmt.Errorf("Claude Desktop control-plane account is missing")
	}
	enrollment, errEnrollment := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return fmt.Errorf("Claude Desktop control-plane enrollment: %w", errEnrollment)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, errSession := m.sessionFor(auth, enrollment, localSessionID, model)
	if errSession != nil {
		return errSession
	}
	return session.ensure(ctx)
}

func (m *Manager) sessionFor(auth *cliproxyauth.Auth, enrollment claudedesktop.Enrollment, localSessionID, model string) (*sessionRuntime, error) {
	return m.sessionForQuery(auth, enrollment, RequestFacts{LocalSessionID: localSessionID, Model: model})
}

func (m *Manager) sessionForQuery(auth *cliproxyauth.Auth, enrollment claudedesktop.Enrollment, facts RequestFacts) (*sessionRuntime, error) {
	localSessionID, model, key := facts.LocalSessionID, facts.Model, facts.sessionKey()
	// Record operations must precede Manager.mu: explicit Stop holds the
	// record admission lock while preparing this manager's query retirement.
	workingDir := m.workingDir
	if facts.RemoteOrigin != nil {
		_, _, _, folder, _, err := facts.RemoteOrigin.Read()
		if err != nil {
			return nil, err
		}
		workingDir = folder
	}
	var checkpoint *claudesessions.BridgeState
	if facts.BridgeRecord != nil {
		if err := facts.BridgeRecord.MatchQuery(facts.DesktopSessionID, facts.QueryID, facts.LocalSessionID); err != nil {
			return nil, err
		}
		var err error
		checkpoint, err = facts.BridgeRecord.Load(enrollment.AccountUUID, enrollment.OrganizationUUID)
		if err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("Claude Desktop control-plane manager is closed")
	}
	if session := m.sessions[key]; session != nil {
		// Never acquire opMu while holding the manager lock: title delivery
		// acquires these in the opposite order, and remote I/O may be pending.
		m.mu.Unlock()
		if session.ready != nil {
			<-session.ready
		}
		session.opMu.Lock()
		if session.ctx.Err() != nil || session.desktopID != facts.DesktopSessionID || session.state.LocalSessionID != localSessionID || session.auth.ID != auth.ID {
			session.opMu.Unlock()
			return nil, fmt.Errorf("Claude Desktop control-plane query is retired or belongs to another session")
		}
		session.auth = auth
		session.enrollment = enrollment
		session.state.Model = model
		session.state.LastActivityAt = m.now().UTC().Format(time.RFC3339Nano)
		if facts.PlaceholderSweepEnabled != nil {
			session.placeholderGate = facts.PlaceholderSweepEnabled
		}
		if facts.Inbound != nil {
			session.inbound = facts.Inbound
		}
		if facts.BindBridge != nil {
			session.bindBridge = facts.BindBridge
		}
		if facts.BridgeTranscript != nil {
			session.bridgeTranscript = facts.BridgeTranscript
		}
		if facts.Role == claudeprofile.RoleMain {
			session.bridgeObserver = facts.BridgeObserver
			session.bridgeStartObserver = facts.BridgeStartObserver
			session.bridgeModel, session.bridgePromptID = model, facts.PromptID
		}
		errPersist := session.persistLocked()
		session.opMu.Unlock()
		if errPersist != nil {
			return nil, errPersist
		}
		return session, nil
	}
	if facts.QueryLifetime != nil && facts.QueryLifetime.Err() != nil {
		m.mu.Unlock()
		return nil, facts.QueryLifetime.Err()
	}
	statePath := protectedSessionPath(m.root, key)
	state := controlSessionState{
		Version:                  controlSessionStateVersion,
		LocalSessionID:           localSessionID,
		Model:                    model,
		HeartbeatIntervalSeconds: m.profile.HeartbeatIntervalSeconds,
		LastActivityAt:           m.now().UTC().Format(time.RFC3339Nano),
	}
	if errLoad := readProtectedState(statePath, &state); errLoad != nil && !errors.Is(errLoad, os.ErrNotExist) {
		log.WithError(errLoad).Warn("claude desktop control-plane: persisted session state was ignored")
		state = controlSessionState{
			Version:                  controlSessionStateVersion,
			LocalSessionID:           localSessionID,
			Model:                    model,
			HeartbeatIntervalSeconds: m.profile.HeartbeatIntervalSeconds,
			LastActivityAt:           m.now().UTC().Format(time.RFC3339Nano),
		}
	}
	if state.Version != controlSessionStateVersion || state.LocalSessionID != localSessionID {
		state = controlSessionState{
			Version:                  controlSessionStateVersion,
			LocalSessionID:           localSessionID,
			Model:                    model,
			HeartbeatIntervalSeconds: m.profile.HeartbeatIntervalSeconds,
			LastActivityAt:           m.now().UTC().Format(time.RFC3339Nano),
		}
	}
	state.Model = model
	state.LastActivityAt = m.now().UTC().Format(time.RFC3339Nano)
	if checkpoint != nil {
		// Worker credentials remain query-owned. A record supplies only its
		// remote identity/cursor; it never imports the previous query's JWT.
		state.RemoteSessionID = checkpoint.SessionID
		state.ArchiveSessionID = "session_" + strings.TrimPrefix(checkpoint.SessionID, "cse_")
		state.WorkerJWT, state.WorkerEpoch, state.WorkerJWTExpiresAt, state.APIBaseURL = "", "", "", ""
		state.WorkerInitialized, state.Archived = false, false
	}
	lifetime, cancel := context.WithCancel(m.ctx)
	session := &sessionRuntime{
		manager:          m,
		key:              key,
		desktopID:        facts.DesktopSessionID,
		queryID:          facts.QueryID,
		workingDir:       workingDir,
		ctx:              lifetime,
		cancel:           cancel,
		done:             make(chan struct{}),
		ready:            make(chan struct{}),
		statePath:        statePath,
		state:            state,
		auth:             auth,
		enrollment:       enrollment,
		placeholderGate:  facts.PlaceholderSweepEnabled,
		remoteOrigin:     facts.RemoteOrigin,
		bindBridge:       facts.BindBridge,
		bridgeRecord:     facts.BridgeRecord,
		bridgeCheckpoint: checkpoint,
		bridgeTranscript: facts.BridgeTranscript,
		resumeBridge:     checkpoint != nil,
		inbound:          facts.Inbound,
	}
	if checkpoint != nil {
		session.lastSequence.Store(checkpoint.LastSequenceNum)
	}
	if facts.Role == claudeprofile.RoleMain {
		session.bridgeObserver = facts.BridgeObserver
		session.bridgeStartObserver = facts.BridgeStartObserver
		session.bridgeModel, session.bridgePromptID = model, facts.PromptID
	}
	errPersist := session.persistLocked()
	session.initFailed.Store(errPersist != nil)
	if facts.QueryID == "" || m.gracefulClose {
		session.shutdownReason = "host_exit"
	}
	m.sessions[key] = session
	if facts.QueryLifetime != nil {
		session.unwatch = context.AfterFunc(facts.QueryLifetime, cancel)
	}
	// One supervisor is registered before publishing the session. Close can
	// now wait safely even if ensure has not started its loops yet.
	m.wg.Add(1)
	go session.retirementLoop()
	m.mu.Unlock()
	if session.bridgeRecord != nil {
		if err := session.bridgeRecord.BindDone(session.done); err != nil {
			errPersist = errors.Join(errPersist, err)
			session.admissionFailed = true
			session.cancel()
		}
	}
	session.initFailed.Store(errPersist != nil)
	close(session.ready)
	return session, errPersist
}

func (s *sessionRuntime) ensure(ctx context.Context) (errResult error) {
	defer func() { s.initFailed.Store(errResult != nil) }()
	ctx, release := s.operationContext(ctx)
	defer release()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if (s.remoteOrigin != nil || s.resumeBridge) && !s.remoteRevived {
		if err := s.reviveRemoteLocked(ctx); err != nil {
			return err
		}
	}

	if s.state.Archived {
		s.resetRemoteLocked()
	}
	if strings.TrimSpace(s.state.RemoteSessionID) == "" {
		if errCreate := s.createSessionLocked(ctx); errCreate != nil {
			return errCreate
		}
	}
	if s.remoteOrigin == nil {
		s.preparePlaceholderLocked()
	}
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return errBridge
		}
	}
	if s.bridgeRecord != nil || s.bindBridge != nil {
		bind := s.bindBridge
		if s.bridgeRecord != nil {
			bind = s.bridgeRecord.Prepare
		}
		if err := bind(s.state.RemoteSessionID); err != nil {
			if errors.Is(err, claudesessions.ErrRemoteBound) {
				// Another record owns this ID. Never archive its session while
				// retiring the losing attachment.
				s.createdFallback = false
			}
			return err
		}
	}
	if s.remoteOrigin != nil && s.createdFallback {
		s.preparePlaceholderLocked()
	}
	if !s.state.WorkerInitialized {
		if errStream := s.startStreamLocked(); errStream != nil {
			return fmt.Errorf("initialize Claude Desktop worker event stream: %w", errStream)
		}
		if errWorker := s.initializeWorkerLocked(ctx); errWorker != nil {
			return errWorker
		}
		s.state.WorkerInitialized = true
		if errPersist := s.persistLocked(); errPersist != nil {
			return errPersist
		}
	}
	if errStream := s.startStreamLocked(); errStream != nil {
		return fmt.Errorf("restore Claude Desktop worker event stream: %w", errStream)
	}
	s.startHeartbeatLocked()
	if !s.bridgeStarted {
		if err := s.checkpointBridgeLocked(); err != nil {
			return err
		}
		// The start observer records its own delivery issues; a lost telemetry
		// occurrence must not fail the bridge itself.
		_ = s.observeBridgeStartedLocked()
	}
	s.bridgeStarted = true
	return nil
}

func (s *sessionRuntime) resetRemoteLocked() {
	if s.workerRead != nil {
		s.workerRead.cancel()
		s.workerRead = nil
	}
	s.workerRestoration = nil
	s.lastSequence.Store(0)
	s.bridgeStarted = false
	s.requestInitSent = false
	s.state.RemoteSessionID = ""
	s.state.ArchiveSessionID = ""
	s.state.WorkerEpoch = ""
	s.state.WorkerJWT = ""
	s.state.APIBaseURL = ""
	s.state.WorkerJWTExpiresAt = ""
	s.state.WorkerInitialized = false
	s.state.Archived = false
	s.placeholderRegistered = false
}

func (s *sessionRuntime) createSessionLocked(ctx context.Context) error {
	hostname, errHostname := s.manager.hostname()
	if errHostname != nil || strings.TrimSpace(hostname) == "" {
		hostname = "desktop"
	}
	request := createSessionRequest{
		Title: sessionTitle(hostname, s.state.LocalSessionID),
		Tags:  []string{"remote-control-sdk"},
		Config: createSessionConfig{
			CWD:   s.workingDir,
			Model: s.state.Model,
		},
	}
	var response createSessionResponse
	if errRequest := s.doJSON(ctx, claudeprofile.ControlEndpointCreateSession, "", request, &response); errRequest != nil {
		return fmt.Errorf("create Claude Desktop control-plane session: %w", errRequest)
	}
	remoteID := strings.TrimSpace(response.Session.ID)
	if !strings.HasPrefix(remoteID, "cse_") || len(remoteID) <= len("cse_") {
		return fmt.Errorf("create Claude Desktop control-plane session: response session id is invalid")
	}
	s.state.RemoteSessionID = remoteID
	s.state.ArchiveSessionID = "session_" + strings.TrimPrefix(remoteID, "cse_")
	s.state.Archived = false
	if errPersist := s.persistLocked(); errPersist != nil {
		return errPersist
	}
	return nil
}

func (s *sessionRuntime) bridgeLocked(ctx context.Context) error {
	if strings.TrimSpace(s.state.RemoteSessionID) == "" {
		return fmt.Errorf("bridge Claude Desktop control-plane session: remote session id is missing")
	}
	var response bridgeResponse
	if errBridge := s.doJSON(ctx, claudeprofile.ControlEndpointBridge, s.state.RemoteSessionID, struct{}{}, &response); errBridge != nil {
		return fmt.Errorf("bridge Claude Desktop control-plane session: %w", errBridge)
	}
	apiBaseURL, errURL := url.Parse(strings.TrimSpace(response.APIBaseURL))
	if errURL != nil || apiBaseURL.Scheme != "https" || !strings.EqualFold(apiBaseURL.Hostname(), "api.anthropic.com") {
		return fmt.Errorf("bridge Claude Desktop control-plane session: api_base_url is invalid")
	}
	if _, errEpoch := strconv.ParseInt(strings.TrimSpace(response.WorkerEpoch), 10, 64); errEpoch != nil {
		return fmt.Errorf("bridge Claude Desktop control-plane session: worker_epoch is invalid")
	}
	if strings.TrimSpace(response.WorkerJWT) == "" || response.ExpiresIn <= 0 {
		return fmt.Errorf("bridge Claude Desktop control-plane session: worker credential is incomplete")
	}
	if s.state.WorkerEpoch != strings.TrimSpace(response.WorkerEpoch) {
		s.requestInitSent = false
	}
	s.state.APIBaseURL = strings.TrimRight(apiBaseURL.String(), "/")
	s.state.WorkerEpoch = strings.TrimSpace(response.WorkerEpoch)
	s.state.WorkerJWT = strings.TrimSpace(response.WorkerJWT)
	s.state.WorkerJWTExpiresAt = s.manager.now().Add(time.Duration(response.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	s.bridgeExpiresIn = response.ExpiresIn
	return s.persistLocked()
}

func (s *sessionRuntime) bridgeExpiredLocked() bool {
	if strings.TrimSpace(s.state.WorkerJWT) == "" || strings.TrimSpace(s.state.WorkerEpoch) == "" {
		return true
	}
	expiresAt, errExpires := time.Parse(time.RFC3339Nano, s.state.WorkerJWTExpiresAt)
	if errExpires != nil {
		return true
	}
	lead := time.Duration(s.manager.profile.WorkerJWTRefreshLeadSeconds) * time.Second
	return !expiresAt.After(s.manager.now().Add(lead))
}

func (s *sessionRuntime) initializeWorkerLocked(ctx context.Context) error {
	if errRead := s.beginWorkerRestorationLocked(ctx); errRead != nil {
		return fmt.Errorf("read Claude Desktop worker state: %w", errRead)
	}
	if flight := s.workerRead; flight != nil {
		wait := s.manager.workerReadOrderingWait
		if wait == nil {
			wait = waitWorkerReadOrdering
		}
		if err := wait(ctx, flight.done); err != nil {
			if conflict := flight.conflict(); conflict != nil {
				s.stopForEpochConflict()
				return conflict
			}
			return err
		}
		if conflict := flight.conflict(); conflict != nil {
			s.stopForEpochConflict()
			return conflict
		}
	}
	if err := errors.Join(ctx.Err(), s.ctx.Err()); err != nil {
		return err
	}
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return errEpoch
	}
	idle := struct {
		WorkerStatus     string `json:"worker_status"`
		WorkerEpoch      int64  `json:"worker_epoch"`
		ExternalMetadata struct {
			PendingAction  any `json:"pending_action"`
			PendingActions any `json:"pending_actions"`
			TaskSummary    any `json:"task_summary"`
		} `json:"external_metadata"`
	}{WorkerStatus: "idle", WorkerEpoch: epoch}
	if errIdle := s.registerWorkerLocked(ctx, idle); errIdle != nil {
		return fmt.Errorf("initialize Claude Desktop worker idle state: %w", errIdle)
	}
	permission := struct {
		WorkerEpoch      int64 `json:"worker_epoch"`
		ExternalMetadata struct {
			PermissionMode string `json:"permission_mode"`
		} `json:"external_metadata"`
	}{WorkerEpoch: epoch}
	permission.ExternalMetadata.PermissionMode = "auto"
	if errPermission := s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerUpdate, permission, nil); errPermission != nil {
		return fmt.Errorf("initialize Claude Desktop worker permission mode: %w", errPermission)
	}
	if current, err := s.workerEpochLocked(); err != nil || current != epoch {
		return errors.Join(errWorkerRestorationStale, err)
	}
	if err := s.readWorkerRestorationLocked(ctx); err != nil {
		return err
	}
	// A renewal during registration must not lend authority to the old read.
	// Keep the actor paused; the next initialization reads the current epoch.
	if errRestore := s.restoreWorkerLocked(ctx); errRestore != nil {
		return errRestore
	}
	return nil
}

func (s *sessionRuntime) workerEpochLocked() (int64, error) {
	epoch, errEpoch := strconv.ParseInt(strings.TrimSpace(s.state.WorkerEpoch), 10, 64)
	if errEpoch != nil {
		return 0, fmt.Errorf("Claude Desktop control-plane worker epoch is invalid")
	}
	return epoch, nil
}

func (s *sessionRuntime) workerJSONLocked(ctx context.Context, endpointName string, body, output any) error {
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return errBridge
		}
	}
	send := func() error {
		requestBody, errBody := s.bindWorkerEpochLocked(body)
		if errBody != nil {
			return errBody
		}
		return s.doJSON(ctx, endpointName, s.state.RemoteSessionID, requestBody, output)
	}
	errRequest := send()
	var status *statusError
	if errors.As(errRequest, &status) && (status.code == http.StatusUnauthorized || status.code == http.StatusForbidden) {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return errors.Join(errRequest, errBridge)
		}
		return send()
	}
	return errRequest
}

func (s *sessionRuntime) startStreamLocked() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.manager.disableLoops {
		return nil
	}
	if s.streamStarted {
		return nil
	}
	decoded, errOpen := s.openStreamLocked(s.ctx)
	if errOpen != nil {
		return errOpen
	}
	s.streamStarted = true
	s.loops.Add(1)
	go s.streamLoop(decoded)
	return nil
}

func (s *sessionRuntime) streamLoop(initial io.ReadCloser) {
	defer s.loops.Done()
	backoff := time.Duration(s.manager.profile.StreamReconnectBackoffMillis) * time.Millisecond
	current := initial
	for {
		if s.ctx.Err() != nil {
			if current != nil {
				_ = current.Close()
			}
			return
		}
		var errStream error
		if current != nil {
			errStream = consumeQueryStream(s.ctx, current, func(frame workerFrame) error { return s.consumeInboundFrame(s.ctx, frame) })
			current = nil
		} else {
			errStream = s.consumeStreamOnce(s.ctx)
		}
		if s.ctx.Err() != nil {
			return
		}
		if errStream != nil {
			log.WithError(errStream).Debug("claude desktop control-plane: worker stream disconnected")
		}
		timer := time.NewTimer(backoff)
		select {
		case <-s.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (s *sessionRuntime) openStreamLocked(ctx context.Context) (io.ReadCloser, error) {
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return nil, errBridge
		}
	}
	request, doer, errRequest := s.buildRequest(ctx, claudeprofile.ControlEndpointWorkerStream, s.state.RemoteSessionID, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	response, errDo := doer.Do(request)
	if errDo != nil {
		return nil, errDo
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("Claude Desktop control-plane worker stream returned no response body")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		_ = response.Body.Close()
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return nil, errBridge
		}
		request, doer, errRequest = s.buildRequest(ctx, claudeprofile.ControlEndpointWorkerStream, s.state.RemoteSessionID, nil)
		if errRequest != nil {
			return nil, errRequest
		}
		response, errDo = doer.Do(request)
		if errDo != nil {
			return nil, errDo
		}
		if response == nil || response.Body == nil {
			return nil, fmt.Errorf("Claude Desktop control-plane worker stream returned no response body")
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = response.Body.Close()
		return nil, &statusError{code: response.StatusCode}
	}
	return decodeBody(response.Body, response.Header.Values("Content-Encoding"))
}

func (s *sessionRuntime) consumeStreamOnce(ctx context.Context) error {
	s.opMu.Lock()
	if err := ctx.Err(); err != nil {
		s.opMu.Unlock()
		return err
	}
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			s.opMu.Unlock()
			return errBridge
		}
	}
	request, doer, errRequest := s.buildRequest(ctx, claudeprofile.ControlEndpointWorkerStream, s.state.RemoteSessionID, nil)
	s.opMu.Unlock()
	if errRequest != nil {
		return errRequest
	}
	response, errDo := doer.Do(request)
	if errDo != nil {
		return errDo
	}
	if response == nil || response.Body == nil {
		return fmt.Errorf("Claude Desktop control-plane worker stream returned no response body")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		_ = response.Body.Close()
		s.opMu.Lock()
		errBridge := s.bridgeLocked(ctx)
		s.opMu.Unlock()
		return errBridge
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = response.Body.Close()
		return &statusError{code: response.StatusCode}
	}
	decoded, errDecode := decodeBody(response.Body, response.Header.Values("Content-Encoding"))
	if errDecode != nil {
		return errDecode
	}
	return consumeQueryStream(ctx, decoded, func(frame workerFrame) error { return s.consumeInboundFrame(ctx, frame) })
}

func (s *sessionRuntime) startHeartbeatLocked() {
	if s.manager.disableLoops || s.ctx.Err() != nil {
		return
	}
	s.heartOnce.Do(func() {
		s.loops.Add(1)
		go s.heartbeatLoop()
	})
}

func (s *sessionRuntime) heartbeatLoop() {
	defer s.loops.Done()
	for {
		interval := s.heartbeatInterval()
		timer := time.NewTimer(interval)
		select {
		case <-s.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		s.opMu.Lock()
		if s.ctx.Err() != nil {
			s.opMu.Unlock()
			return
		}
		errHeartbeat := s.heartbeatLocked(s.ctx)
		s.opMu.Unlock()
		if errHeartbeat != nil && s.ctx.Err() == nil {
			log.WithError(errHeartbeat).Warn("claude desktop control-plane: worker heartbeat failed")
		}
	}
}

func (s *sessionRuntime) heartbeatInterval() time.Duration {
	s.opMu.Lock()
	seconds := s.state.HeartbeatIntervalSeconds
	s.opMu.Unlock()
	if seconds <= 0 {
		seconds = s.manager.profile.HeartbeatIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (s *sessionRuntime) heartbeatLocked(ctx context.Context) error {
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return errEpoch
	}
	lastActivityAt, _ := time.Parse(time.RFC3339Nano, s.state.LastActivityAt)
	localSessionID := s.state.LocalSessionID
	interval := s.state.HeartbeatIntervalSeconds
	idleSeconds := int64(s.manager.now().Sub(lastActivityAt).Seconds())
	if idleSeconds < 0 {
		idleSeconds = 0
	}
	request := struct {
		SessionID              string `json:"session_id"`
		WorkerEpoch            int64  `json:"worker_epoch"`
		SupportsHeartbeatProbe bool   `json:"supports_heartbeat_probe"`
		CurrentIntervalSeconds int    `json:"current_interval_seconds"`
		IdleSeconds            int64  `json:"idle_seconds"`
	}{
		SessionID:              localSessionID,
		WorkerEpoch:            epoch,
		SupportsHeartbeatProbe: true,
		CurrentIntervalSeconds: interval,
		IdleSeconds:            idleSeconds,
	}
	var response heartbeatResponse
	if errHeartbeat := s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerHeartbeat, request, &response); errHeartbeat != nil {
		return errHeartbeat
	}
	if response.HeartbeatIntervalSeconds > 0 {
		s.state.HeartbeatIntervalSeconds = response.HeartbeatIntervalSeconds
		return s.persistLocked()
	}
	return nil
}

func (m *Manager) Close() {
	m.close(true)
}

// Quarantine cancels local work without sending a normal shutdown or archive
// with a credential/binding that is no longer approved for network use.
func (m *Manager) Quarantine() {
	m.close(false)
}

// PrepareQuarantine revokes network work without waiting for worker teardown.
// Coordinators use it to signal every account-local owner before joining any.
func (m *Manager) PrepareQuarantine() {
	if m == nil {
		return
	}
	m.abort.Store(true)
	m.abortShutdown()
	m.cancel()
}

func (m *Manager) close(graceful bool) {
	if m == nil {
		return
	}
	// Quarantine must also abort a graceful Close already inside sync.Once.
	if !graceful {
		m.PrepareQuarantine()
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		if graceful {
			for _, session := range m.sessions {
				session.setShutdownReason("host_exit")
			}
		}
		m.mu.Unlock()
		m.cancel()
		m.wg.Wait()
	})
}

func (s *sessionRuntime) shutdown(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.epochSuperseded.Load() {
		// A superseded/archived worker owns no remote mutations. In particular,
		// teardown must not renew credentials or archive its successor's session.
		return nil
	}
	if s.admissionFailed || (s.remoteOrigin != nil || s.resumeBridge) && !s.bridgeStarted && !s.createdFallback {
		return nil
	}
	if strings.TrimSpace(s.state.RemoteSessionID) == "" || s.state.Archived {
		return nil
	}
	var errs []error
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			errs = append(errs, errBridge)
		}
	}
	if strings.TrimSpace(s.state.WorkerJWT) != "" {
		if reason := s.getShutdownReason(); reason != "" {
			shutdownEvent := workerShutdownPayload{
				Type:      "system",
				Subtype:   "worker_shutting_down",
				Reason:    reason,
				SessionID: s.state.RemoteSessionID,
				UUID:      uuid.NewString(),
			}
			if errEvent := s.postWorkerEventLocked(ctx, shutdownEvent); errEvent != nil {
				errs = append(errs, errEvent)
			}
		}
		resultEvent := zeroTurnResultPayload{
			Type:              "result",
			Subtype:           "success",
			StopReason:        nil,
			Usage:             zeroUsage(),
			ModelUsage:        map[string]any{},
			PermissionDenials: []any{},
			SessionID:         s.state.RemoteSessionID,
			UUID:              uuid.NewString(),
		}
		if errResult := s.postWorkerEventLocked(ctx, resultEvent); errResult != nil {
			errs = append(errs, errResult)
		}
	}
	archive, errArchive := s.archiveOnTeardownObservedLocked(ctx)
	if s.getShutdownReason() != "" {
		if errFlush := s.flushDeliveryLocked(ctx); errFlush != nil {
			errs = append(errs, errFlush)
		}
	}
	if archiveTerminal(errArchive) {
		if errRemove := s.manager.removePlaceholder(s.state.RemoteSessionID, nil); errRemove != nil {
			errs = append(errs, errRemove)
		}
	}
	if errArchive != nil {
		errs = append(errs, errArchive)
	} else {
		s.state.Archived = true
		s.state.WorkerJWT = ""
		s.state.WorkerJWTExpiresAt = ""
		if errPersist := s.persistLocked(); errPersist != nil {
			errs = append(errs, errPersist)
		}
	}
	// Cleanup of a failed provisional admission is not the teardown of a
	// running bridge. Initialization failure remains visible in control status.
	if s.bridgeStarted {
		if errEvent := s.observeBridgeLocked(BridgeTeardown, &archive); errEvent != nil {
			errs = append(errs, errEvent)
		}
	}
	return errors.Join(errs...)
}

func (s *sessionRuntime) postWorkerEventLocked(ctx context.Context, payload any) error {
	s.rememberOutbound(payload)
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return errEpoch
	}
	request := workerEventRequest{
		WorkerEpoch: epoch,
		Events:      []workerEventEnvelope{{Payload: payload}},
	}
	return s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerEvents, request, nil)
}

func zeroUsage() zeroUsagePayload {
	return zeroUsagePayload{
		ServiceTier: "standard",
		Iterations:  []any{},
		Speed:       "standard",
	}
}

func (s *sessionRuntime) doJSON(ctx context.Context, endpointName, sessionID string, body, output any) error {
	return s.doJSONObserved(ctx, endpointName, sessionID, body, output, nil)
}

func (s *sessionRuntime) doJSONObserved(ctx context.Context, endpointName, sessionID string, body, output any, observedStatus *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, doer, errRequest := s.buildRequest(ctx, endpointName, sessionID, body)
	if errRequest != nil {
		return errRequest
	}
	return s.manager.performJSON(request, doer, output, observedStatus)
}

// The request is an immutable credential/header snapshot. Delivery uploaders
// can perform its I/O without locking query input or interrupt dispatch.
func (m *Manager) performJSON(request *http.Request, doer HTTPDoer, output any, observedStatus *int) error {
	response, errDo := doer.Do(request)
	if errDo != nil {
		return errDo
	}
	if response == nil {
		return fmt.Errorf("Claude Desktop control-plane returned no response")
	}
	if response.Body == nil {
		if observedStatus != nil {
			*observedStatus = response.StatusCode
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return controlResponseStatus(response, nil, m.now())
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && output == nil {
			return nil
		}
		return fmt.Errorf("Claude Desktop control-plane returned no response body")
	}
	decoded, errDecode := decodeBody(response.Body, response.Header.Values("Content-Encoding"))
	if errDecode != nil {
		return errDecode
	}
	payload, errRead := io.ReadAll(io.LimitReader(decoded, maximumResponseBytes+1))
	errClose := decoded.Close()
	if errRead != nil || errClose != nil {
		return errors.Join(errRead, errClose)
	}
	if len(payload) > maximumResponseBytes {
		return fmt.Errorf("Claude Desktop control-plane response exceeds size limit")
	}
	if observedStatus != nil {
		*observedStatus = response.StatusCode
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return controlResponseStatus(response, payload, m.now())
	}
	if output != nil && len(bytes.TrimSpace(payload)) > 0 {
		if errJSON := json.Unmarshal(payload, output); errJSON != nil {
			return fmt.Errorf("parse Claude Desktop control-plane response: %w", errJSON)
		}
	}
	return nil
}

func (s *sessionRuntime) buildRequest(ctx context.Context, endpointName, sessionID string, body any) (*http.Request, HTTPDoer, error) {
	endpoint, okEndpoint := s.manager.profile.Endpoints[endpointName]
	if !okEndpoint {
		return nil, nil, fmt.Errorf("Claude Desktop control-plane endpoint %q is unavailable", endpointName)
	}
	path := strings.ReplaceAll(endpoint.Path, "{session_id}", url.PathEscape(strings.TrimSpace(sessionID)))
	baseURL := strings.TrimRight(s.manager.profile.BaseURL, "/")
	if strings.HasPrefix(endpointName, "worker_") && strings.TrimSpace(s.state.APIBaseURL) != "" {
		baseURL = strings.TrimRight(s.state.APIBaseURL, "/")
	}
	var encodedBody []byte
	var errMarshal error
	if body != nil {
		encodedBody, errMarshal = json.Marshal(body)
		if errMarshal != nil {
			return nil, nil, fmt.Errorf("marshal Claude Desktop control-plane request: %w", errMarshal)
		}
	}
	request, errRequest := http.NewRequestWithContext(ctx, endpoint.Method, baseURL+path, bytes.NewReader(encodedBody))
	if errRequest != nil {
		return nil, nil, errRequest
	}
	for _, header := range endpoint.Headers {
		request.Header.Set(header.Name, header.Value)
	}
	if endpointName == claudeprofile.ControlEndpointWorkerStream {
		if cursor := s.lastSequence.Load(); cursor > 0 {
			query := request.URL.Query()
			query.Set("from_sequence_num", strconv.FormatInt(cursor, 10))
			request.URL.RawQuery = query.Encode()
			request.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
		}
	}
	switch endpoint.AuthPolicy {
	case "oauth-bearer":
		if s.manager.credentials != nil {
			current, err := s.manager.credentials.Current(s.auth.Clone())
			if err != nil {
				return nil, nil, err
			}
			if err = s.useCredentialLocked(current); err != nil {
				return nil, nil, err
			}
		}
		accessToken := strings.TrimSpace(claudeauth.ReadMetadataString(&s.auth.Metadata, "access_token"))
		if accessToken == "" {
			return nil, nil, errMissingOAuthAccessToken
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
	case "worker-jwt":
		workerJWT := strings.TrimSpace(s.state.WorkerJWT)
		if workerJWT == "" {
			return nil, nil, fmt.Errorf("Claude Desktop control-plane worker credential is missing")
		}
		request.Header.Set("Authorization", "Bearer "+workerJWT)
	default:
		return nil, nil, fmt.Errorf("Claude Desktop control-plane auth policy %q is unsupported", endpoint.AuthPolicy)
	}
	if headerOrdered(endpoint.HeaderOrder, "x-organization-uuid") {
		request.Header.Set("x-organization-uuid", s.enrollment.OrganizationUUID)
	}
	request.Close = strings.EqualFold(request.Header.Get("Connection"), "close")
	doer, errDoer := s.manager.doerFactory(ctx, endpoint.EndpointRole, s.auth)
	if errDoer != nil {
		return nil, nil, errDoer
	}
	if doer == nil {
		return nil, nil, fmt.Errorf("Claude Desktop control-plane transport %q is unavailable", endpoint.EndpointRole)
	}
	return request, doer, nil
}

func (s *sessionRuntime) persistLocked() error {
	return writeProtectedState(s.statePath, s.state)
}

func sessionTitle(hostname, localSessionID string) string {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	var normalized strings.Builder
	for _, character := range hostname {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			normalized.WriteRune(character)
		case normalized.Len() > 0 && !strings.HasSuffix(normalized.String(), "-"):
			normalized.WriteByte('-')
		}
	}
	prefix := strings.Trim(normalized.String(), "-")
	if prefix == "" {
		prefix = "desktop"
	}
	adjectives := []string{"bright", "calm", "curious", "gentle", "lively", "nimble", "steady", "whimsical"}
	nouns := []string{"comet", "forest", "harbor", "meadow", "rocket", "signal", "summit", "voyage"}
	digest := sha256.Sum256([]byte("claude-desktop-control-title-v1\x00" + localSessionID))
	return prefix + "-" + adjectives[int(digest[0])%len(adjectives)] + "-" + nouns[int(digest[1])%len(nouns)]
}

func headerOrdered(order []string, name string) bool {
	for _, header := range order {
		if strings.EqualFold(strings.TrimSpace(header), name) {
			return true
		}
	}
	return false
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var errs []error
	for index := len(c.closers) - 1; index >= 0; index-- {
		errs = append(errs, c.closers[index]())
	}
	return errors.Join(errs...)
}

func decodeBody(body io.ReadCloser, headerValues []string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("Claude Desktop control-plane response body is nil")
	}
	encodings := strings.Split(strings.Join(headerValues, ","), ",")
	reader := io.Reader(body)
	closers := []func() error{body.Close}
	for index := len(encodings) - 1; index >= 0; index-- {
		switch encoding := strings.ToLower(strings.TrimSpace(encodings[index])); encoding {
		case "", "identity":
		case "gzip":
			gzipReader, errGzip := gzip.NewReader(reader)
			if errGzip != nil {
				_ = body.Close()
				return nil, errGzip
			}
			reader = gzipReader
			closers = append(closers, gzipReader.Close)
		case "deflate":
			deflateReader := flate.NewReader(reader)
			reader = deflateReader
			closers = append(closers, deflateReader.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, errZstd := zstd.NewReader(reader)
			if errZstd != nil {
				_ = body.Close()
				return nil, errZstd
			}
			reader = decoder
			closers = append(closers, func() error { decoder.Close(); return nil })
		default:
			_ = body.Close()
			return nil, fmt.Errorf("Claude Desktop control-plane response encoding %q is unsupported", encoding)
		}
	}
	return &compositeReadCloser{Reader: reader, closers: closers}, nil
}
