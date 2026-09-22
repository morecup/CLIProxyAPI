package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const controlFailureSettleDelay = 250 * time.Millisecond

// RequestFacts are the request-local values needed to reproduce the Desktop
// worker event lifecycle. Captured account credentials never enter this type.
type RequestFacts struct {
	Prompt                *claudeprompt.Request
	Role                  claudeprofile.RequestRole
	LocalSessionID        string
	DesktopSessionID      string
	QueryID               string
	QueryLifetime         context.Context
	RequireQueryOwnership bool
	RemoteOrigin          *claudesessions.RemoteGrant
	Inbound               *InboundConsumer
	BindBridge            func(string) error
	BridgeRecord          *claudesessions.BridgeGrant
	BridgeTranscript      BridgeTranscriptSink
	RemoteInput           bool
	// The read belongs to the exact live Host, including experiment exposure.
	PlaceholderSweepEnabled func() (bool, error)
	BridgeObserver          BridgeObserver
	// BridgeStartObserver receives the single BridgeStarted event of a Host
	// generation; nil leaves the start unobserved.
	BridgeStartObserver BridgeObserver
	PromptID            string
	Model               string
	PermissionMode      string
	Effort              string
	Attempt             int
	Body                []byte
}

// RequestSpan joins one Messages request to its remote-control worker events.
// Auxiliary control-plane failures are logged and never returned to Messages.
type RequestSpan struct {
	manager   *Manager
	session   *sessionRuntime
	facts     RequestFacts
	startedAt time.Time

	mu           sync.Mutex
	finished     bool
	retryPlanned bool
	started      bool
	failureTimer *time.Timer
	headers      http.Header
	response     responseAccumulator
}

type userEventPayload struct {
	Type            string             `json:"type"`
	Message         userMessagePayload `json:"message"`
	SessionID       string             `json:"session_id"`
	ParentToolUseID any                `json:"parent_tool_use_id"`
	UUID            string             `json:"uuid"`
	Timestamp       time.Time          `json:"timestamp"`
	Origin          userOriginPayload  `json:"origin"`
}

type userMessagePayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type userOriginPayload struct {
	Kind string `json:"kind"`
}

type workerRunningPayload struct {
	WorkerEpoch           int64                         `json:"worker_epoch"`
	ExternalMetadata      workerRunningExternalMetadata `json:"external_metadata"`
	WorkerStatus          string                        `json:"worker_status"`
	RequiresActionDetails any                           `json:"requires_action_details"`
}

type workerRunningExternalMetadata struct {
	CrossSessionInbound string                `json:"cross_session_inbound"`
	EffortLevel         string                `json:"effort_level"`
	RateLimitInfo       *runningRateLimitInfo `json:"rate_limit_info,omitempty"`
}

type runningRateLimitInfo struct {
	Status         string `json:"status"`
	ResetsAt       int64  `json:"resetsAt"`
	RateLimitType  string `json:"rateLimitType"`
	IsUsingOverage bool   `json:"isUsingOverage"`
}

type rateLimitInfoPayload struct {
	Status                string                  `json:"status"`
	ResetsAt              int64                   `json:"resetsAt"`
	RateLimitType         string                  `json:"rateLimitType"`
	OverageStatus         string                  `json:"overageStatus"`
	OverageDisabledReason string                  `json:"overageDisabledReason"`
	IsUsingOverage        bool                    `json:"isUsingOverage"`
	UnifiedWindows        unifiedRateLimitWindows `json:"unifiedWindows"`
}

type unifiedRateLimitWindows struct {
	FiveHour rateLimitWindow `json:"five_hour"`
	SevenDay rateLimitWindow `json:"seven_day"`
}

type rateLimitWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

type backgroundTasksPayload struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Tasks     []any  `json:"tasks"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

type rateLimitEventPayload struct {
	Type          string               `json:"type"`
	RateLimitInfo rateLimitInfoPayload `json:"rate_limit_info"`
	UUID          string               `json:"uuid"`
	SessionID     string               `json:"session_id"`
}

type statusEventPayload struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	Status         any    `json:"status"`
	PermissionMode string `json:"permissionMode"`
	UUID           string `json:"uuid"`
	SessionID      string `json:"session_id"`
}

type initEventPayload struct {
	Type                    string   `json:"type"`
	Subtype                 string   `json:"subtype"`
	CWD                     string   `json:"cwd"`
	SessionID               string   `json:"session_id"`
	Tools                   []any    `json:"tools"`
	MCPServers              []any    `json:"mcp_servers"`
	Model                   string   `json:"model"`
	PermissionMode          string   `json:"permissionMode"`
	SlashCommands           []string `json:"slash_commands"`
	APIKeySource            string   `json:"apiKeySource"`
	ClaudeCodeVersion       string   `json:"claude_code_version"`
	OutputStyle             string   `json:"output_style"`
	Agents                  []string `json:"agents"`
	Skills                  []string `json:"skills"`
	Plugins                 []any    `json:"plugins"`
	AnalyticsDisabled       bool     `json:"analytics_disabled"`
	ProductFeedbackDisabled bool     `json:"product_feedback_disabled"`
	UUID                    string   `json:"uuid"`
	FastModeState           string   `json:"fast_mode_state"`
	FastModeDisabledReason  string   `json:"fast_mode_disabled_reason"`
	Effort                  string   `json:"effort"`
}

// The captured init-only updates change worker configuration without replaying
// background/status startup events. No caller content is retained here.
type workerInitConfig struct {
	model, permissionMode, effort, fastModeState, fastModeDisabledReason string
}

type assistantEventPayload struct {
	Type            string                  `json:"type"`
	Message         assistantMessagePayload `json:"message"`
	SessionID       string                  `json:"session_id"`
	ParentToolUseID any                     `json:"parent_tool_use_id"`
	UUID            string                  `json:"uuid"`
	Timestamp       time.Time               `json:"timestamp"`
	RequestID       string                  `json:"request_id"`
}

type assistantMessagePayload struct {
	Model        string          `json:"model"`
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   json.RawMessage `json:"stop_reason"`
	StopSequence json.RawMessage `json:"stop_sequence"`
	StopDetails  json.RawMessage `json:"stop_details"`
	Usage        json.RawMessage `json:"usage"`
	Diagnostics  json.RawMessage `json:"diagnostics"`
}

type thinkingTokensPayload struct {
	Type                 string `json:"type"`
	Subtype              string `json:"subtype"`
	EstimatedTokens      int    `json:"estimated_tokens"`
	EstimatedTokensDelta int    `json:"estimated_tokens_delta"`
	SessionID            string `json:"session_id"`
	UUID                 string `json:"uuid"`
}

type responseAction struct {
	Block                json.RawMessage
	EstimatedTokens      int
	EstimatedTokensDelta int
}

type taskStartedPayload struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id"`
	Description    string `json:"description"`
	SubagentType   string `json:"subagent_type"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	SpawnDepth     int    `json:"spawn_depth"`
	TaskType       string `json:"task_type"`
	Prompt         string `json:"prompt"`
	UUID           string `json:"uuid"`
	SessionID      string `json:"session_id"`
}

type taskUpdatedPayload struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	TaskID  string `json:"task_id"`
	Patch   struct {
		Status  string `json:"status"`
		EndTime int64  `json:"end_time"`
	} `json:"patch"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

type taskNotificationUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMS  int `json:"duration_ms"`
}

type responseAccumulator struct {
	messageID      string
	model          string
	messageType    string
	role           string
	stopReason     json.RawMessage
	stopSequence   json.RawMessage
	stopDetails    json.RawMessage
	usage          json.RawMessage
	finalUsage     json.RawMessage
	diagnostics    json.RawMessage
	blocks         []json.RawMessage
	actions        []responseAction
	streamBlocks   map[int]*streamBlock
	titleText      strings.Builder
	thinkingTokens int
}

type streamBlock struct {
	typeName  string
	id        string
	name      string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	inputJSON strings.Builder
}

type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Usage        json.RawMessage `json:"usage"`
	Delta        struct {
		Type            string `json:"type"`
		Text            string `json:"text"`
		Thinking        string `json:"thinking"`
		Signature       string `json:"signature"`
		PartialJSON     string `json:"partial_json"`
		EstimatedTokens *int   `json:"estimated_tokens"`
	} `json:"delta"`
}

// BeginRequest starts only request classes that participate in the captured
// worker protocol. Title helpers are observed so their generated title can be
// applied to the next main turn; other helper requests remain independent.
func (m *Manager) BeginRequest(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts) (*RequestSpan, error) {
	if m == nil || !m.Enabled() {
		return nil, nil
	}
	facts.LocalSessionID = strings.TrimSpace(facts.LocalSessionID)
	facts.DesktopSessionID = strings.TrimSpace(facts.DesktopSessionID)
	facts.QueryID = strings.TrimSpace(facts.QueryID)
	facts.Model = strings.TrimSpace(facts.Model)
	facts.PermissionMode = strings.TrimSpace(facts.PermissionMode)
	facts.Effort = strings.TrimSpace(facts.Effort)
	if facts.PermissionMode == "" {
		facts.PermissionMode = "auto"
	}
	if facts.Effort == "" {
		facts.Effort = "high"
	}
	if facts.LocalSessionID == "" {
		return nil, fmt.Errorf("Claude Desktop control-plane request session is missing")
	}
	if facts.RequireQueryOwnership || facts.QueryID != "" || facts.DesktopSessionID != "" || facts.QueryLifetime != nil {
		if facts.QueryID == "" || facts.DesktopSessionID == "" || facts.QueryLifetime == nil {
			return nil, fmt.Errorf("Claude Desktop control-plane query ownership is missing")
		}
		if err := facts.QueryLifetime.Err(); err != nil {
			return nil, err
		}
	}
	span := &RequestSpan{manager: m, facts: facts, startedAt: m.now().UTC()}
	if facts.Role == claudeprofile.RoleTitle {
		return span, nil
	}
	if facts.Role != claudeprofile.RoleMain && facts.Role != claudeprofile.RoleSubagent {
		return nil, nil
	}
	if facts.Model == "" {
		return nil, fmt.Errorf("Claude Desktop control-plane request model is missing")
	}
	if auth == nil {
		return nil, fmt.Errorf("Claude Desktop control-plane account is missing")
	}
	enrollment, errEnrollment := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return nil, fmt.Errorf("Claude Desktop control-plane enrollment: %w", errEnrollment)
	}
	session, errSession := m.sessionForQuery(auth, enrollment, facts)
	if errSession != nil {
		return nil, errSession
	}
	if errEnsure := session.ensure(ctx); errEnsure != nil {
		return nil, errEnsure
	}
	span.session = session
	return span, nil
}

func (s *RequestSpan) Active() bool {
	return s != nil && s.manager != nil
}

func (s *RequestSpan) start(ctx context.Context) error {
	ctx, release := s.session.operationContext(ctx)
	defer release()
	content := lastUserTextBlock(s.facts.Body)
	s.session.opMu.Lock()
	defer s.session.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if content != "" && !s.facts.RemoteInput {
		payload := userEventPayload{
			Type:      "user",
			Message:   userMessagePayload{Role: "user", Content: content},
			SessionID: s.session.state.RemoteSessionID,
			UUID:      uuid.NewString(),
			Timestamp: s.manager.now().UTC(),
			Origin:    userOriginPayload{Kind: "human"},
		}
		if errEvent := s.session.postWorkerEventLocked(ctx, payload); errEvent != nil {
			return fmt.Errorf("post Claude Desktop worker user event: %w", errEvent)
		}
	}
	if errRead := s.session.doJSON(ctx, claudeprofile.ControlEndpointSessionRead, s.session.state.ArchiveSessionID, nil, nil); errRead != nil {
		return fmt.Errorf("read Claude Desktop request session: %w", errRead)
	}
	return nil
}

func (s *RequestSpan) ObserveHTTPResponse(headers http.Header) {
	if !s.Active() || len(headers) == 0 {
		return
	}
	s.mu.Lock()
	s.headers = headers.Clone()
	shouldStart := s.facts.Role == claudeprofile.RoleMain && !s.started && s.session != nil
	if shouldStart {
		if s.facts.Prompt != nil {
			shouldStart = s.facts.Prompt.ClaimControlInput()
		} else {
			shouldStart = s.facts.Attempt <= 1
		}
	}
	if shouldStart {
		s.started = true
	}
	s.mu.Unlock()
	if shouldStart {
		if errStart := s.start(context.Background()); errStart != nil {
			log.WithError(errStart).Warn("claude desktop control-plane: request start events were incomplete")
		}
	}
}

func (s *RequestSpan) ObserveResponsePayload(payload []byte, streaming bool) {
	if !s.Active() || len(bytes.TrimSpace(payload)) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if streaming {
		for _, line := range bytes.Split(payload, []byte{'\n'}) {
			s.response.observeStreamLine(line)
		}
		return
	}
	s.response.observeMessage(payload)
}

func (s *RequestSpan) ObserveStreamLine(line []byte) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		s.response.observeStreamLine(line)
	}
}

func (s *RequestSpan) FinishSuccess(ctx context.Context) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()
	switch s.facts.Role {
	case claudeprofile.RoleTitle:
		if title := s.response.title(); title != "" {
			s.manager.rememberQueryTitle(s.facts, title)
		}
		return
	case claudeprofile.RoleSubagent:
		return
	}
	if s.session != nil {
		s.complete(ctx, nil)
	}
}

func (s *RequestSpan) FinishFailure(_ context.Context, err error) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	if s.facts.Role != claudeprofile.RoleMain || s.session == nil {
		s.mu.Unlock()
		return
	}
	if s.facts.Prompt != nil {
		// A timer cannot determine whether the conductor will retry. Managed
		// prompts remain open until an explicit terminal decision is available.
		s.mu.Unlock()
		return
	}
	s.failureTimer = time.AfterFunc(controlFailureSettleDelay, func() {
		s.mu.Lock()
		if s.retryPlanned {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.complete(context.Background(), err)
	})
	s.mu.Unlock()
}

// FinishPromptFailure receives the conductor's terminal decision, without a
// speculative settle timer. The owning prompt token deduplicates this call.
func (s *RequestSpan) FinishPromptFailure(ctx context.Context, err error) {
	if s == nil || s.session == nil || s.facts.Prompt == nil || !s.facts.Prompt.Snapshot().Failed {
		return
	}
	s.complete(ctx, err)
}

// RecordScheduledRetry suppresses the terminal worker error for an attempt
// once the conductor has committed to retrying it.
func (s *RequestSpan) RecordScheduledRetry() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retryPlanned = true
	if s.failureTimer != nil {
		s.failureTimer.Stop()
	}
	s.mu.Unlock()
}

func (s *RequestSpan) complete(ctx context.Context, terminalErr error) {
	ctx, release := s.session.operationContext(ctx)
	defer release()
	s.mu.Lock()
	headers := s.headers.Clone()
	response := s.response.snapshot()
	s.mu.Unlock()

	s.session.opMu.Lock()
	defer s.session.opMu.Unlock()
	if ctx.Err() != nil || s.session.state.Archived || strings.TrimSpace(s.session.state.RemoteSessionID) == "" {
		return
	}
	s.session.state.LastActivityAt = s.manager.now().UTC().Format(time.RFC3339Nano)
	var errs []error
	if errRunning := s.session.setWorkerRunningLocked(ctx, s.facts, headers); errRunning != nil {
		errs = append(errs, errRunning)
	}
	if errEvents := s.session.postRequestEventsLocked(ctx, s.facts, headers, response); errEvents != nil {
		errs = append(errs, errEvents)
	}
	if terminalErr == nil {
		if errTitle := s.session.patchPendingTitleLocked(ctx); errTitle != nil {
			errs = append(errs, errTitle)
		}
	}
	result := zeroTurnResultPayload{
		Type:              "result",
		Subtype:           "success",
		StopReason:        nil,
		Usage:             zeroUsage(),
		ModelUsage:        map[string]any{},
		PermissionDenials: []any{},
		SessionID:         s.session.state.RemoteSessionID,
		UUID:              uuid.NewString(),
	}
	if terminalErr != nil {
		result.Subtype = "error"
		result.IsError = true
		result.Result = terminalErr.Error()
	}
	// Tool requests/partial continuations do not terminate the owning prompt.
	// Attempt failures still use the existing explicit retry suppression path.
	if terminalErr != nil || s.facts.Prompt == nil || s.facts.Prompt.CompletedPrompt() {
		if errResult := s.session.postWorkerEventLocked(ctx, result); errResult != nil {
			errs = append(errs, errResult)
		}
	}
	if errPersist := s.session.persistLocked(); errPersist != nil {
		errs = append(errs, errPersist)
	}
	if errJoined := errors.Join(errs...); errJoined != nil {
		log.WithError(errJoined).Warn("claude desktop control-plane: request event lifecycle was incomplete")
	}
}

func (s *sessionRuntime) setWorkerRunningLocked(ctx context.Context, facts RequestFacts, headers http.Header) error {
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return errEpoch
	}
	running := workerRunningPayload{
		WorkerEpoch: epoch,
		ExternalMetadata: workerRunningExternalMetadata{
			CrossSessionInbound: "available",
			EffortLevel:         facts.Effort,
			RateLimitInfo:       runningRateLimitFromHeaders(headers),
		},
		WorkerStatus: "running",
	}
	return s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerUpdate, running, nil)
}

func (s *sessionRuntime) postRequestEventsLocked(ctx context.Context, facts RequestFacts, headers http.Header, response responseAccumulator) error {
	// Renewal can select a different worker epoch and reset initialization.
	// Resolve that before choosing between startup and continuation batches.
	if s.bridgeExpiredLocked() {
		if errBridge := s.bridgeLocked(ctx); errBridge != nil {
			return errBridge
		}
	}
	remoteID := s.state.RemoteSessionID
	permissionMode := facts.PermissionMode
	worker := s.manager.profile.Worker
	initializing := !s.requestInitSent
	initConfig := workerInitConfig{facts.Model, permissionMode, facts.Effort, worker.FastModeState, worker.FastModeDisabledReason}
	initChanged := initializing || s.requestInitConfig != initConfig
	var events []workerEventEnvelope
	if initializing {
		events = append(events, workerEventEnvelope{Payload: backgroundTasksPayload{
			Type: "system", Subtype: "background_tasks_changed", Tasks: []any{}, UUID: uuid.NewString(), SessionID: remoteID,
		}})
	}
	if rate := rateLimitFromHeaders(headers); rate != nil {
		events = append(events, workerEventEnvelope{Payload: rateLimitEventPayload{
			Type: "rate_limit_event", RateLimitInfo: *rate, UUID: uuid.NewString(), SessionID: remoteID,
		}})
	}
	if initializing {
		events = append(events,
			workerEventEnvelope{Payload: statusEventPayload{
				Type: "system", Subtype: "status", Status: nil, PermissionMode: permissionMode, UUID: uuid.NewString(), SessionID: remoteID,
			}},
		)
	}
	if initChanged {
		events = append(events,
			workerEventEnvelope{Payload: initEventPayload{
				Type:                    "system",
				Subtype:                 "init",
				CWD:                     "",
				SessionID:               remoteID,
				Tools:                   []any{},
				MCPServers:              []any{},
				Model:                   facts.Model,
				PermissionMode:          permissionMode,
				SlashCommands:           append([]string(nil), worker.SlashCommands...),
				APIKeySource:            worker.APIKeySource,
				ClaudeCodeVersion:       worker.ClaudeCodeVersion,
				OutputStyle:             worker.OutputStyle,
				Agents:                  append([]string(nil), worker.Agents...),
				Skills:                  append([]string(nil), worker.Skills...),
				Plugins:                 []any{},
				AnalyticsDisabled:       worker.AnalyticsDisabled,
				ProductFeedbackDisabled: worker.ProductFeedbackDisabled,
				UUID:                    uuid.NewString(),
				FastModeState:           worker.FastModeState,
				FastModeDisabledReason:  worker.FastModeDisabledReason,
				Effort:                  facts.Effort,
			}},
		)
	}
	responseEvents := response.workerEvents(remoteID, s.manager.now().UTC(), controlHeader(headers, "request-id"))
	if initializing && len(responseEvents) == 1 {
		if assistant, ok := responseEvents[0].(assistantEventPayload); ok && blockType(assistant.Message.Content) == "text" {
			events = append(events, workerEventEnvelope{Payload: assistant})
			responseEvents = nil
		}
	}
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return errEpoch
	}
	if len(events) > 0 {
		if errBatch := s.workerJSONLocked(ctx, claudeprofile.ControlEndpointWorkerEvents, workerEventRequest{WorkerEpoch: epoch, Events: events}, nil); errBatch != nil {
			return errBatch
		}
		// A failed batch cannot suppress the next initialization attempt.
		if initChanged {
			s.requestInitSent = true
			s.requestInitConfig = initConfig
		}
	}
	for _, event := range responseEvents {
		if errEvent := s.postWorkerEventLocked(ctx, event); errEvent != nil {
			return errEvent
		}
	}
	return nil
}

func (s *sessionRuntime) patchPendingTitleLocked(ctx context.Context) error {
	title := s.manager.takeTitle(s.key)
	if title == "" {
		return nil
	}
	body := struct {
		Title string `json:"title"`
	}{Title: title}
	if errPatch := s.doJSON(ctx, claudeprofile.ControlEndpointSessionUpdate, s.state.ArchiveSessionID, body, nil); errPatch != nil {
		s.manager.rememberTitle(s.key, title)
		return errPatch
	}
	return nil
}

func (m *Manager) rememberTitle(localSessionID, title string) {
	if m == nil {
		return
	}
	localSessionID = strings.TrimSpace(localSessionID)
	title = strings.TrimSpace(title)
	if localSessionID == "" || title == "" {
		return
	}
	m.mu.Lock()
	m.pendingTitles[localSessionID] = title
	m.mu.Unlock()
}

func (m *Manager) takeTitle(localSessionID string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	title := m.pendingTitles[localSessionID]
	delete(m.pendingTitles, localSessionID)
	return title
}

func (r *responseAccumulator) observeMessage(payload []byte) {
	var message struct {
		Model        string            `json:"model"`
		ID           string            `json:"id"`
		Type         string            `json:"type"`
		Role         string            `json:"role"`
		Content      []json.RawMessage `json:"content"`
		StopReason   json.RawMessage   `json:"stop_reason"`
		StopSequence json.RawMessage   `json:"stop_sequence"`
		StopDetails  json.RawMessage   `json:"stop_details"`
		Usage        json.RawMessage   `json:"usage"`
		Diagnostics  json.RawMessage   `json:"diagnostics"`
	}
	if json.Unmarshal(payload, &message) != nil {
		return
	}
	r.messageID = message.ID
	r.model = message.Model
	r.messageType = defaultString(message.Type, "message")
	r.role = defaultString(message.Role, "assistant")
	r.stopReason = rawOrNull(message.StopReason)
	r.stopSequence = rawOrNull(message.StopSequence)
	r.stopDetails = rawOrNull(message.StopDetails)
	r.usage = rawOrNull(message.Usage)
	r.finalUsage = rawOrNull(message.Usage)
	r.diagnostics = rawOrNull(message.Diagnostics)
	r.blocks = r.blocks[:0]
	r.actions = r.actions[:0]
	r.thinkingTokens = 0
	for _, block := range message.Content {
		var text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(block, &text) == nil && text.Type == "text" {
			r.titleText.WriteString(text.Text)
		}
		normalized := normalizeAssistantBlock(block)
		if blockType(normalized) == "thinking" {
			r.appendSignatureEstimate(blockSignature(normalized))
		}
		if len(normalized) > 0 {
			r.blocks = append(r.blocks, normalized)
			r.actions = append(r.actions, responseAction{Block: normalized})
		}
	}
}

func (r *responseAccumulator) observeStreamLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var event streamEvent
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "message_start":
		var message struct {
			Model        string          `json:"model"`
			ID           string          `json:"id"`
			Type         string          `json:"type"`
			Role         string          `json:"role"`
			StopReason   json.RawMessage `json:"stop_reason"`
			StopSequence json.RawMessage `json:"stop_sequence"`
			StopDetails  json.RawMessage `json:"stop_details"`
			Usage        json.RawMessage `json:"usage"`
			Diagnostics  json.RawMessage `json:"diagnostics"`
		}
		if json.Unmarshal(event.Message, &message) != nil {
			return
		}
		r.messageID = message.ID
		r.model = message.Model
		r.messageType = defaultString(message.Type, "message")
		r.role = defaultString(message.Role, "assistant")
		r.stopReason = rawOrNull(message.StopReason)
		r.stopSequence = rawOrNull(message.StopSequence)
		r.stopDetails = rawOrNull(message.StopDetails)
		r.usage = rawOrNull(message.Usage)
		r.diagnostics = rawOrNull(message.Diagnostics)
	case "content_block_start":
		var block struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Input     json.RawMessage `json:"input"`
		}
		if json.Unmarshal(event.ContentBlock, &block) != nil {
			return
		}
		if r.streamBlocks == nil {
			r.streamBlocks = make(map[int]*streamBlock)
		}
		current := &streamBlock{typeName: block.Type, id: block.ID, name: block.Name}
		current.text.WriteString(block.Text)
		current.thinking.WriteString(block.Thinking)
		current.signature.WriteString(block.Signature)
		if len(bytes.TrimSpace(block.Input)) > 0 && !bytes.Equal(bytes.TrimSpace(block.Input), []byte("{}")) {
			current.inputJSON.Write(block.Input)
		}
		r.streamBlocks[event.Index] = current
	case "content_block_delta":
		current := r.streamBlocks[event.Index]
		if current == nil {
			return
		}
		switch event.Delta.Type {
		case "text_delta":
			current.text.WriteString(event.Delta.Text)
		case "thinking_delta":
			current.thinking.WriteString(event.Delta.Thinking)
			if event.Delta.EstimatedTokens != nil {
				r.appendThinkingEstimate(*event.Delta.EstimatedTokens)
			}
		case "signature_delta":
			current.signature.WriteString(event.Delta.Signature)
		case "input_json_delta":
			current.inputJSON.WriteString(event.Delta.PartialJSON)
		}
	case "content_block_stop":
		current := r.streamBlocks[event.Index]
		if current == nil {
			return
		}
		if current.typeName == "thinking" {
			r.appendSignatureEstimate(current.signature.String())
		}
		block := current.raw()
		if len(block) > 0 {
			r.blocks = append(r.blocks, block)
			r.actions = append(r.actions, responseAction{Block: block})
			if current.typeName == "text" {
				r.titleText.WriteString(current.text.String())
			}
		}
		delete(r.streamBlocks, event.Index)
	case "message_delta":
		if len(bytes.TrimSpace(event.Usage)) > 0 {
			r.finalUsage = append(json.RawMessage(nil), event.Usage...)
		}
	}
}

func (r responseAccumulator) snapshot() responseAccumulator {
	var snapshot responseAccumulator
	snapshot.messageID = r.messageID
	snapshot.model = r.model
	snapshot.messageType = r.messageType
	snapshot.role = r.role
	snapshot.stopReason = append(json.RawMessage(nil), r.stopReason...)
	snapshot.stopSequence = append(json.RawMessage(nil), r.stopSequence...)
	snapshot.stopDetails = append(json.RawMessage(nil), r.stopDetails...)
	snapshot.usage = append(json.RawMessage(nil), r.usage...)
	snapshot.finalUsage = append(json.RawMessage(nil), r.finalUsage...)
	snapshot.diagnostics = append(json.RawMessage(nil), r.diagnostics...)
	snapshot.blocks = append([]json.RawMessage(nil), r.blocks...)
	snapshot.actions = make([]responseAction, 0, len(r.actions))
	for _, action := range r.actions {
		action.Block = append(json.RawMessage(nil), action.Block...)
		snapshot.actions = append(snapshot.actions, action)
	}
	snapshot.thinkingTokens = r.thinkingTokens
	snapshot.titleText.WriteString(r.titleText.String())
	return snapshot
}

func (r responseAccumulator) workerEvents(sessionID string, at time.Time, requestID string) []any {
	if strings.TrimSpace(r.messageID) == "" || len(r.actions) == 0 {
		return nil
	}
	events := make([]any, 0, len(r.actions))
	for _, action := range r.actions {
		if len(action.Block) == 0 {
			events = append(events, thinkingTokensPayload{
				Type:                 "system",
				Subtype:              "thinking_tokens",
				EstimatedTokens:      action.EstimatedTokens,
				EstimatedTokensDelta: action.EstimatedTokensDelta,
				SessionID:            sessionID,
				UUID:                 uuid.NewString(),
			})
			continue
		}
		events = append(events, assistantEventPayload{
			Type: "assistant",
			Message: assistantMessagePayload{
				Model:        r.model,
				ID:           r.messageID,
				Type:         defaultString(r.messageType, "message"),
				Role:         defaultString(r.role, "assistant"),
				Content:      action.Block,
				StopReason:   rawOrNull(r.stopReason),
				StopSequence: rawOrNull(r.stopSequence),
				StopDetails:  rawOrNull(r.stopDetails),
				Usage:        rawOrNull(r.usage),
				Diagnostics:  rawOrNull(r.diagnostics),
			},
			SessionID: sessionID,
			UUID:      uuid.NewString(),
			Timestamp: at,
			RequestID: requestID,
		})
	}
	return events
}

func (r *responseAccumulator) appendThinkingEstimate(estimated int) {
	if r == nil || estimated <= r.thinkingTokens {
		return
	}
	r.actions = append(r.actions, responseAction{
		EstimatedTokens:      estimated,
		EstimatedTokensDelta: estimated - r.thinkingTokens,
	})
	r.thinkingTokens = estimated
}

func (r *responseAccumulator) appendSignatureEstimate(signature string) {
	if estimated := signatureTokenEstimate(signature); estimated > 0 {
		r.appendThinkingEstimate(estimated)
	}
}

func signatureTokenEstimate(signature string) int {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return 0
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(signature)
	if errDecode != nil {
		decoded, errDecode = base64.RawStdEncoding.DecodeString(signature)
	}
	if errDecode != nil || len(decoded) == 0 {
		return 0
	}
	return (len(decoded) + 3) / 4
}

func (r responseAccumulator) title() string {
	text := strings.TrimSpace(r.titleText.String())
	if text == "" {
		return ""
	}
	var value struct {
		Title string `json:"title"`
	}
	if json.Unmarshal([]byte(text), &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.Title)
}

func (r responseAccumulator) textContent() string {
	var output strings.Builder
	for _, block := range r.blocks {
		var value struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(block, &value) == nil && value.Type == "text" {
			output.WriteString(value.Text)
		}
	}
	return output.String()
}

func (r responseAccumulator) toolUseCount() int {
	count := 0
	for _, block := range r.blocks {
		if blockType(block) == "tool_use" {
			count++
		}
	}
	return count
}

func (r responseAccumulator) totalTokens() (int, bool) {
	type usageValue struct {
		InputTokens              *int `json:"input_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		OutputTokens             *int `json:"output_tokens"`
	}
	decode := func(raw json.RawMessage) usageValue {
		var value usageValue
		_ = json.Unmarshal(rawOrNull(raw), &value)
		return value
	}
	initial := decode(r.usage)
	final := decode(r.finalUsage)
	if final.InputTokens == nil {
		final.InputTokens = initial.InputTokens
	}
	if final.CacheCreationInputTokens == nil {
		final.CacheCreationInputTokens = initial.CacheCreationInputTokens
	}
	if final.CacheReadInputTokens == nil {
		final.CacheReadInputTokens = initial.CacheReadInputTokens
	}
	if final.OutputTokens == nil {
		final.OutputTokens = initial.OutputTokens
	}
	if final.InputTokens == nil || final.OutputTokens == nil {
		return 0, false
	}
	total := *final.InputTokens + *final.OutputTokens
	if final.CacheCreationInputTokens != nil {
		total += *final.CacheCreationInputTokens
	}
	if final.CacheReadInputTokens != nil {
		total += *final.CacheReadInputTokens
	}
	if total < 0 {
		return 0, false
	}
	return total, true
}

func (b *streamBlock) raw() json.RawMessage {
	if b == nil {
		return nil
	}
	var value any
	switch b.typeName {
	case "text":
		value = struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: b.text.String()}
	case "thinking":
		value = struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature,omitempty"`
		}{Type: "thinking", Thinking: "", Signature: b.signature.String()}
	case "tool_use":
		input := json.RawMessage(bytes.TrimSpace([]byte(b.inputJSON.String())))
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		value = struct {
			Type   string          `json:"type"`
			ID     string          `json:"id"`
			Name   string          `json:"name"`
			Input  json.RawMessage `json:"input"`
			Caller struct {
				Type string `json:"type"`
			} `json:"caller"`
		}{Type: "tool_use", ID: b.id, Name: b.name, Input: input, Caller: struct {
			Type string `json:"type"`
		}{Type: "direct"}}
	default:
		return nil
	}
	payload, _ := json.Marshal(value)
	return payload
}

func normalizeAssistantBlock(block json.RawMessage) json.RawMessage {
	var value struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Input     json.RawMessage `json:"input"`
	}
	if json.Unmarshal(block, &value) != nil {
		return append(json.RawMessage(nil), block...)
	}
	switch value.Type {
	case "text", "thinking", "tool_use":
		streamed := &streamBlock{typeName: value.Type, id: value.ID, name: value.Name}
		streamed.text.WriteString(value.Text)
		streamed.thinking.WriteString(value.Thinking)
		streamed.signature.WriteString(value.Signature)
		if len(bytes.TrimSpace(value.Input)) > 0 {
			streamed.inputJSON.Write(value.Input)
		}
		return streamed.raw()
	default:
		return append(json.RawMessage(nil), block...)
	}
}

func blockSignature(block json.RawMessage) string {
	var value struct {
		Signature string `json:"signature"`
	}
	_ = json.Unmarshal(block, &value)
	return value.Signature
}

func lastUserTextBlock(body []byte) string {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	for messageIndex := len(request.Messages) - 1; messageIndex >= 0; messageIndex-- {
		message := request.Messages[messageIndex]
		if message.Role != "user" {
			continue
		}
		var direct string
		if json.Unmarshal(message.Content, &direct) == nil {
			return strings.TrimSpace(direct)
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for blockIndex := len(blocks) - 1; blockIndex >= 0; blockIndex-- {
			if blocks[blockIndex].Type == "text" && strings.TrimSpace(blocks[blockIndex].Text) != "" {
				return blocks[blockIndex].Text
			}
		}
	}
	return ""
}

func runningRateLimitFromHeaders(headers http.Header) *runningRateLimitInfo {
	status := controlHeader(headers, "anthropic-ratelimit-unified-status")
	typeName := controlHeader(headers, "anthropic-ratelimit-unified-representative-claim")
	reset, okReset := parseHeaderInt64(headers, "anthropic-ratelimit-unified-reset")
	if status == "" || typeName == "" || !okReset {
		return nil
	}
	return &runningRateLimitInfo{
		Status: status, ResetsAt: reset, RateLimitType: typeName,
		IsUsingOverage: strings.EqualFold(controlHeader(headers, "anthropic-ratelimit-unified-is-using-overage"), "true"),
	}
}

func rateLimitFromHeaders(headers http.Header) *rateLimitInfoPayload {
	if len(headers) == 0 {
		return nil
	}
	status := controlHeader(headers, "anthropic-ratelimit-unified-status")
	typeName := controlHeader(headers, "anthropic-ratelimit-unified-representative-claim")
	reset, okReset := parseHeaderInt64(headers, "anthropic-ratelimit-unified-reset")
	if status == "" || typeName == "" || !okReset {
		return nil
	}
	overageStatus := controlHeader(headers, "anthropic-ratelimit-unified-overage-status")
	overageDisabledReason := controlHeader(headers, "anthropic-ratelimit-unified-overage-disabled-reason")
	if overageStatus == "" || overageDisabledReason == "" {
		return nil
	}
	fiveReset, okFiveReset := parseHeaderInt64(headers, "anthropic-ratelimit-unified-5h-reset")
	sevenReset, okSevenReset := parseHeaderInt64(headers, "anthropic-ratelimit-unified-7d-reset")
	fiveUtilization, okFiveUtilization := parseHeaderFloat(headers, "anthropic-ratelimit-unified-5h-utilization")
	sevenUtilization, okSevenUtilization := parseHeaderFloat(headers, "anthropic-ratelimit-unified-7d-utilization")
	if !okFiveReset || !okSevenReset || !okFiveUtilization || !okSevenUtilization {
		return nil
	}
	return &rateLimitInfoPayload{
		Status:                status,
		ResetsAt:              reset,
		RateLimitType:         typeName,
		OverageStatus:         overageStatus,
		OverageDisabledReason: overageDisabledReason,
		IsUsingOverage:        strings.EqualFold(controlHeader(headers, "anthropic-ratelimit-unified-is-using-overage"), "true"),
		UnifiedWindows: unifiedRateLimitWindows{
			FiveHour: rateLimitWindow{Utilization: fiveUtilization, ResetsAt: fiveReset},
			SevenDay: rateLimitWindow{Utilization: sevenUtilization, ResetsAt: sevenReset},
		},
	}
}

func parseHeaderInt64(headers http.Header, name string) (int64, bool) {
	value, errParse := strconv.ParseInt(controlHeader(headers, name), 10, 64)
	return value, errParse == nil
}

func parseHeaderFloat(headers http.Header, name string) (float64, bool) {
	value, errParse := strconv.ParseFloat(controlHeader(headers, name), 64)
	return value, errParse == nil
}

func controlHeader(headers http.Header, name string) string {
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func blockType(block json.RawMessage) string {
	var value struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(block, &value)
	return value.Type
}

func rawOrNull(value json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(value)) == 0 {
		return json.RawMessage("null")
	}
	return value
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
