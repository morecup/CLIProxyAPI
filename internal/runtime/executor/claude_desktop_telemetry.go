package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	claudedesktopauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

var errClaudeDesktopStreamIncomplete = errors.New("claude desktop stream ended before completion")

type claudeDesktopRetryTelemetryError struct {
	err  error
	span *claudeDesktopRequestSpan
}

func (e *claudeDesktopRetryTelemetryError) Error() string { return e.err.Error() }
func (e *claudeDesktopRetryTelemetryError) Unwrap() error { return e.err }

func (e *claudeDesktopRetryTelemetryError) RecordScheduledRetry(ctx context.Context, attempt int, delay time.Duration) {
	if e != nil && e.span != nil {
		e.span.RecordScheduledRetry(ctx, attempt, delay, e.err)
	}
}

func (e *claudeDesktopRetryTelemetryError) StatusCode() int {
	var statusError interface{ StatusCode() int }
	if e != nil && errors.As(e.err, &statusError) && statusError != nil {
		return statusError.StatusCode()
	}
	return 0
}

func (e *claudeDesktopRetryTelemetryError) RetryAfter() *time.Duration {
	var retryError interface{ RetryAfter() *time.Duration }
	if e != nil && errors.As(e.err, &retryError) && retryError != nil {
		return retryError.RetryAfter()
	}
	return nil
}

func (e *claudeDesktopRetryTelemetryError) IsRequestScoped() bool {
	var requestError interface{ IsRequestScoped() bool }
	return e != nil && errors.As(e.err, &requestError) && requestError != nil && requestError.IsRequestScoped()
}

func attachClaudeDesktopRetryTelemetry(err error, span *claudeDesktopRequestSpan) error {
	if err == nil || span == nil || !span.Active() {
		return err
	}
	var attached *claudeDesktopRetryTelemetryError
	if errors.As(err, &attached) {
		return err
	}
	return &claudeDesktopRetryTelemetryError{err: err, span: span}
}

// claudeDesktopRequestSpan is the shared lifecycle surface for renderer/SDK
// telemetry and the Desktop remote-control worker protocol. Keeping the two
// observers behind one span prevents Execute, ExecuteStream, and HttpRequest
// from drifting into subtly different event ordering.
type claudeDesktopRequestSpan struct {
	accounting     *helps.ClaudeDesktopSDKAccounting
	telemetry      *claudetelemetry.RequestSpan
	control        *claudecontrol.RequestSpan
	prompt         *claudeprompt.Request
	promptResponse claudeprompt.Response
	childNative    func([]claudeprompt.SDKNativeMessage, string) error
}

func (s *claudeDesktopRequestSpan) Active() bool {
	return s != nil && (s.accounting != nil || s.prompt != nil || s.childNative != nil || (s.telemetry != nil && s.telemetry.Active()) || (s.control != nil && s.control.Active()))
}

func (s *claudeDesktopRequestSpan) ObserveRequest(body []byte, headers http.Header) {
	if s == nil {
		return
	}
	s.accounting.ObserveRequest(body)
	if s.telemetry != nil {
		s.telemetry.ObserveRequest(body, headers)
	}
	// Canonical conversation state must not depend on telemetry being enabled
	// or its queue/enrollment material being available. The shared owner makes
	// this idempotent when the telemetry observer already recorded the query.
	if s.prompt != nil {
		s.prompt.ObserveSDKQuery(body)
	}
	s.checkpointSDKSessionState()
}

func (s *claudeDesktopRequestSpan) checkpointSDKSessionState() {
	if s == nil {
		return
	}
	s.prompt.ObserveNativeTranscriptFailure(s.telemetry.ObserveSDKSessionStateUnavailable)
	s.prompt.ObserveNativeContent(s.promptResponse.NativeContentMessages())
	if s.childNative != nil {
		if err := s.childNative(s.promptResponse.NativeContentMessages()); err != nil && !errors.Is(err, claudetasks.ErrNativeObserverRetired) {
			s.telemetry.ObserveSDKSessionStateUnavailable()
		}
	}
	if s.prompt.CheckpointSDKSessionState() != nil || s.accounting.SDKSessionStateError() != nil {
		s.telemetry.ObserveSDKSessionStateUnavailable()
	}
}

func (s *claudeDesktopRequestSpan) ObserveFirstByte(at time.Time) {
	if s == nil {
		return
	}
	if s.telemetry != nil {
		s.telemetry.ObserveFirstByte(at)
	}
}

func (s *claudeDesktopRequestSpan) ObserveUsage(value claudetelemetry.Usage) {
	if s == nil {
		return
	}
	if s.telemetry != nil {
		s.telemetry.ObserveUsage(value)
	}
}

func (s *claudeDesktopRequestSpan) ObserveResponse(requestID, stopReason string) {
	if s == nil {
		return
	}
	if s.telemetry != nil {
		s.telemetry.ObserveResponse(requestID, stopReason)
	}
}

func (s *claudeDesktopRequestSpan) ObserveHTTPResponse(status int, headers http.Header) {
	if s == nil {
		return
	}
	if s.prompt != nil || s.childNative != nil {
		s.promptResponse.SetNativeRequestID(headers.Get("Request-Id"))
	}
	s.accounting.ObserveHTTPResponse(status)
	if s.telemetry != nil {
		s.telemetry.ObserveHTTPResponse(status, headers)
	}
	if s.control != nil {
		s.control.ObserveHTTPResponse(headers)
	}
}

func (s *claudeDesktopRequestSpan) ObserveResponsePayload(payload []byte, streaming bool) {
	if s != nil {
		s.accounting.ObservePayload(payload, streaming)
	}
	if s != nil && s.telemetry != nil {
		s.telemetry.ObserveResponsePayload(payload, streaming)
	}
	if s != nil && (s.prompt != nil || s.childNative != nil) {
		s.promptResponse.ObservePayload(payload, streaming)
		s.observeSDKAssistantMessage()
		s.checkpointSDKSessionState()
	}
	if s != nil && s.control != nil {
		s.control.ObserveResponsePayload(payload, streaming)
	}
}

func (s *claudeDesktopRequestSpan) ObserveStreamLine(line []byte) {
	if s != nil {
		s.accounting.ObserveStreamLine(line)
	}
	if s != nil && s.telemetry != nil {
		s.telemetry.ObserveStreamLine(line)
	}
	if s != nil && (s.prompt != nil || s.childNative != nil) {
		s.observePromptStreamLine(line)
	}
	if s != nil && s.control != nil {
		s.control.ObserveStreamLine(line)
	}
}

func (s *claudeDesktopRequestSpan) observePromptStreamLine(line []byte) {
	if s == nil || (s.prompt == nil && s.childNative == nil) {
		return
	}
	s.promptResponse.ObserveStreamLine(line)
	s.observeSDKAssistantMessage()
	if raw := bytes.TrimSpace(line); bytes.HasPrefix(raw, []byte("data:")) {
		switch gjson.GetBytes(bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("data:"))), "type").String() {
		case "content_block_stop", "message_delta", "message_stop":
			s.checkpointSDKSessionState()
		}
	}
}

func (s *claudeDesktopRequestSpan) observeSDKAssistantMessage() {
	if s == nil || s.prompt == nil {
		return
	}
	s.prompt.ObserveSDKHistory(s.promptResponse.SDKHistoryMessages())
	s.prompt.ObserveSDKWireResponse(s.promptResponse.SDKWireFingerprint())
	if at, tools := s.promptResponse.SDKAssistantMessage(); !at.IsZero() {
		s.prompt.ObserveSDKAssistantMessage(at, tools)
	}
}

func (s *claudeDesktopRequestSpan) ObserveResponseContentMetrics(requestID, stopReason string, textLength int, thinkingLength *int, toolUseLengths map[string]int) {
	if s == nil {
		return
	}
	if s.telemetry != nil {
		s.telemetry.ObserveResponseContentMetrics(requestID, stopReason, textLength, thinkingLength, toolUseLengths)
	}
}

func (s *claudeDesktopRequestSpan) FinishSuccess(ctx context.Context) {
	if s == nil {
		return
	}
	completedAt := time.Now()
	if s.prompt != nil {
		stop, tools, complete := s.promptResponse.Outcome()
		if !complete {
			s.FinishFailure(ctx, "incomplete_stream", errClaudeDesktopStreamIncomplete)
			return
		}
		cliproxyexecutor.ClearUpstreamFailureFinalizer(ctx, s.prompt.FinalizerKey())
		s.prompt.FinishSuccess(completedAt, stop, tools)
	}
	s.accounting.FinishSuccess(completedAt)
	s.checkpointSDKSessionState()
	if s.telemetry != nil {
		cliproxyexecutor.ClearUpstreamFailureFinalizer(ctx, s.telemetry.TitleFinalizerKey())
		s.telemetry.FinishSuccessAt(ctx, completedAt)
	}
	if s.control != nil {
		s.control.FinishSuccess(ctx)
	}
}

func (s *claudeDesktopRequestSpan) FinishFailure(ctx context.Context, category string, err error) {
	if s == nil {
		return
	}
	s.accounting.FinishFailure()
	if s.prompt != nil {
		s.prompt.FinishFailure()
	}
	s.checkpointSDKSessionState()
	if s.telemetry != nil {
		s.telemetry.FinishFailure(ctx, category, err)
		if key := s.telemetry.TitleFinalizerKey(); key != "" {
			if !cliproxyexecutor.RegisterUpstreamFailureFinalizer(ctx, key, s.telemetry.FinishTitleFailure) {
				s.telemetry.FinishTitleFailure()
			}
		}
	}
	if s.control != nil {
		s.control.FinishFailure(ctx, err)
	}
	if s.prompt != nil {
		finalize := func() {
			var finalized bool
			if errors.Is(err, context.Canceled) {
				finalized = s.prompt.FinalizeCancellation(time.Now())
			} else {
				finalized = s.prompt.FinalizeFailure(time.Now())
			}
			if !finalized {
				return
			}
			s.checkpointSDKSessionState()
			terminalCtx := context.Background()
			if ctx != nil {
				terminalCtx = context.WithoutCancel(ctx)
			}
			if s.telemetry != nil {
				s.telemetry.FinishPromptFailure(terminalCtx, category, err)
			}
			if s.control != nil {
				s.control.FinishPromptFailure(terminalCtx, err)
			}
		}
		if !cliproxyexecutor.RegisterUpstreamFailureFinalizer(ctx, s.prompt.FinalizerKey(), finalize) {
			finalize()
		}
	}
}

// FinishPhysicalFailure records an API failure without retiring its logical
// prompt. A reactive summary still needs the current prompt's owned history.
// The final request owner later supplies success or terminal failure.
func (s *claudeDesktopRequestSpan) FinishPhysicalFailure(ctx context.Context, err error) {
	if s == nil {
		return
	}
	s.accounting.FinishFailure()
	if s.telemetry != nil {
		s.telemetry.FinishFailure(ctx, claudeDesktopTelemetryFailureCategory(err), err)
	}
	if s.control != nil {
		s.control.FinishFailure(ctx, err)
	}
}

func (s *claudeDesktopRequestSpan) RecordScheduledRetry(ctx context.Context, attempt int, delay time.Duration, err error) {
	if s == nil {
		return
	}
	if s.prompt != nil {
		status := 0
		var statusError interface{ StatusCode() int }
		if errors.As(err, &statusError) {
			status = statusError.StatusCode()
		}
		s.prompt.ObserveSDKRetry(status)
	}
	if s.telemetry != nil {
		s.telemetry.RecordScheduledRetry(ctx, attempt, delay, err)
	}
	if s.control != nil {
		s.control.RecordScheduledRetry()
	}
}

func (e *ClaudeExecutor) beginClaudeDesktopTelemetry(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	role claudeprofile.RequestRole,
	facts claudeDesktopRuntimeFacts,
	body []byte,
	metadata ...map[string]any,
) *claudeDesktopRequestSpan {
	if e == nil {
		return nil
	}
	permissionMode := claudeDesktopTelemetryPermissionMode(metadata...)
	attempt := 1
	telemetryStartedAt := time.Now()
	var telemetryChainStartedAt time.Time
	if upstreamAttempt, ok := cliproxyexecutor.UpstreamAttemptFromContext(ctx); ok {
		attempt = upstreamAttempt.Number
		telemetryChainStartedAt = upstreamAttempt.ChainStartedAt
		telemetryStartedAt = upstreamAttempt.StartedAt
	}
	if telemetryStartedAt.IsZero() {
		telemetryStartedAt = time.Now()
	}
	if telemetryChainStartedAt.IsZero() || telemetryChainStartedAt.After(telemetryStartedAt) {
		telemetryChainStartedAt = telemetryStartedAt
	}
	var accounting *helps.ClaudeDesktopSDKAccounting
	if e.desktopOnly {
		accounting = helps.BeginClaudeDesktopSDKAccounting(&e.desktopPrompts, auth, e.desktopProfile, facts.Prompt, role,
			facts.SessionID, helps.ClaudeDesktopParentPromptID(ctx, metadata...), facts.ClientRequestID, telemetryStartedAt, telemetryChainStartedAt)
	}
	var controlSpan *claudecontrol.RequestSpan
	var desktopSessionID, queryID string
	var queryLifetime context.Context
	var placeholderSweepEnabled func() (bool, error)
	var bindBridge func(string) error
	var bridgeRecord *claudesessions.BridgeGrant
	remoteInput := false
	if ctx != nil {
		if owner, ok := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext); ok && owner.manager == e.desktopATIS && owner.host != nil {
			desktopSessionID, queryID, queryLifetime = owner.desktopSessionID, owner.host.ID(), owner.host.Context()
			remoteInput = owner.remoteInput
			bindBridge = func(id string) error { return owner.manager.desktopRecords.BindBridge(owner.host, id) }
			bridgeRecord = owner.manager.desktopRecords.BridgeForHost(owner.host)
			placeholderSweepEnabled = func() (bool, error) {
				if err := owner.host.Context().Err(); err != nil {
					return false, err
				}
				value, err := owner.manager.featureValueOnHost(auth, owner.host, "tengu_bridge_placeholder_sweep", json.RawMessage(`true`))
				return claudefeatures.Truthy(value.Raw), err
			}
		}
	}
	if e.desktopControlPlane != nil {
		var bridgeObserver, bridgeStartObserver claudecontrol.BridgeObserver
		if role == claudeprofile.RoleMain && e.desktopTelemetry != nil && queryID != "" {
			var errBridge error
			bridgeObserver, errBridge = e.desktopTelemetry.SDKBridgeObserver(auth, claudecontrol.BridgeOwner{
				DesktopSessionID: desktopSessionID, QueryID: queryID, SDKSessionID: facts.SessionID,
			})
			if errBridge == nil {
				// tengu_bridge_repl_started is observed by the resume/bridge
				// topic (sdk_resume_bridge.go) at the real bridge-lock moment.
				bridgeStartObserver, errBridge = e.desktopTelemetry.SDKResumeBridgeObserver(auth, claudecontrol.BridgeOwner{
					DesktopSessionID: desktopSessionID, QueryID: queryID, SDKSessionID: facts.SessionID,
				})
			}
			if errBridge != nil {
				helps.LogWithRequestID(ctx).WithError(errBridge).Warn("claude desktop: bridge telemetry initialization failed; continuing message request")
			}
		}
		var errControlPlane error
		controlSpan, errControlPlane = e.desktopControlPlane.BeginRequest(ctx, auth, claudecontrol.RequestFacts{
			Prompt:                  facts.Prompt,
			Role:                    role,
			LocalSessionID:          facts.SessionID,
			DesktopSessionID:        desktopSessionID,
			QueryID:                 queryID,
			QueryLifetime:           queryLifetime,
			RequireQueryOwnership:   e.desktopOnly,
			PlaceholderSweepEnabled: placeholderSweepEnabled,
			BridgeObserver:          bridgeObserver,
			BridgeStartObserver:     bridgeStartObserver,
			BindBridge:              bindBridge,
			BridgeRecord:            bridgeRecord,
			BridgeTranscript:        e.desktopBridgeTranscript(auth, facts.SessionID),
			RemoteInput:             remoteInput,
			PromptID:                facts.PromptID,
			Model:                   facts.LogicalModel,
			PermissionMode:          permissionMode,
			Effort:                  strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()),
			Attempt:                 attempt,
			Body:                    body,
		})
		if errControlPlane != nil {
			helps.LogWithRequestID(ctx).WithError(errControlPlane).Warn("claude desktop control-plane: request lifecycle initialization failed; continuing message request")
			controlSpan = nil
		}
	}
	var telemetrySpan *claudetelemetry.RequestSpan
	if e.desktopTelemetry != nil {
		queryChainID, queryDepth := helps.ClaudeDesktopQueryLineage(metadata...)
		if facts.Prompt != nil && queryChainID == "" {
			identity := facts.Prompt.Identity()
			queryChainID, queryDepth = identity.QueryChainID, &identity.QueryDepth
		}
		requestFacts := claudetelemetry.RequestFacts{
			ExternalSDKAccounting: accounting != nil,
			Input:                 facts.Input,
			Prompt:                facts.Prompt,
			Role:                  role,
			SessionID:             facts.SessionID,
			PromptID:              facts.PromptID,
			ParentPromptID:        helps.ClaudeDesktopParentPromptID(ctx, metadata...),
			ClientRequestID:       facts.ClientRequestID,
			PreviousRequestID:     facts.PreviousRequestID,
			Model:                 facts.LogicalModel,
			PermissionMode:        permissionMode,
			MCPServerCount:        claudeDesktopMCPServerCount(body),
			TranscriptSize:        claudeDesktopTranscriptSize(metadata...),
			QueryChainID:          queryChainID,
			QueryDepth:            queryDepth,
		}
		requestFacts.DesktopSessionID = desktopSessionID
		requestFacts.QueryID = queryID
		requestFacts.QueryLifetime = queryLifetime
		if attempt > 0 {
			requestFacts.Attempt = attempt
			requestFacts.ChainStartedAt = telemetryChainStartedAt
			requestFacts.StartedAt = telemetryStartedAt
		}
		telemetrySpan = e.desktopTelemetry.BeginRequest(ctx, auth, requestFacts)
	}
	if role == claudeprofile.RoleMain {
		if facts.ContextError != nil {
			helps.LogWithRequestID(ctx).Warn("claude desktop: owned context could not be verified or persisted; continuing request with visible degraded state")
			telemetrySpan.ObserveOwnedContextState(false)
		} else if facts.ContextSaved {
			telemetrySpan.ObserveOwnedContextState(true)
		}
	}
	var childNative func([]claudeprompt.SDKNativeMessage, string) error
	if role == claudeprofile.RoleSubagent && ctx != nil && auth != nil {
		invocation, ok := claudetasks.InvocationFromContext(ctx)
		owner, owned := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if ok && owned && owner.remoteInput && owner.agentID == invocation.AgentID && owner.host != nil {
			enrollment, err := claudedesktopauth.ValidateEnrollment(auth.ID, auth.Metadata)
			if err == nil && e.desktopATIS.queryHostFromContext(ctx, auth, claudeDesktopATISIdentityHash(enrollment), facts.SessionID) == owner.host && invocation.BeginNativeResponse != nil {
				childNative = invocation.BeginNativeResponse()
			}
		}
	}
	if accounting == nil && telemetrySpan == nil && controlSpan == nil && facts.Prompt == nil && childNative == nil {
		return nil
	}
	span := &claudeDesktopRequestSpan{accounting: accounting, telemetry: telemetrySpan, control: controlSpan, prompt: facts.Prompt, childNative: childNative}
	if facts.Prompt != nil || childNative != nil {
		span.promptResponse.EnableNativeContent()
	}
	// Owned requests replay the native query-loop events before
	// tengu_api_query, as the SDK query loop does: registered hooks ordered
	// before the tool-search decision (order < 500), the tool-search decision
	// and deferred-pool announcement, then the hooks ordered after it.
	hookRequest := claudeDesktopRequestTelemetryInput{Role: role, Facts: facts, Body: body, Attempt: attempt, Metadata: metadata}
	runClaudeDesktopRequestTelemetryHooks(ctx, span, hookRequest, func(order int) bool { return order < claudeDesktopToolSearchHookOrder })
	observeClaudeDesktopToolSearchRequest(ctx, span, role, facts.LogicalModel, body, attempt, false, nil)
	runClaudeDesktopRequestTelemetryHooks(ctx, span, hookRequest, func(order int) bool { return order >= claudeDesktopToolSearchHookOrder })
	return span
}

// claudeDesktopToolSearchHookOrder is the position of the native j1e
// tool-search decision within the request-build hooks.
const claudeDesktopToolSearchHookOrder = 500

// claudeDesktopRequestTelemetryInput is what a request-build telemetry hook
// sees for one owned or pass-through request.
type claudeDesktopRequestTelemetryInput struct {
	Role     claudeprofile.RequestRole
	Facts    claudeDesktopRuntimeFacts
	Body     []byte
	Attempt  int
	Metadata []map[string]any
}

// claudeDesktopRequestTelemetryHook observes one request before it is sent.
// Hooks run in ascending Order; they must not send, mutate the body or block.
type claudeDesktopRequestTelemetryHook struct {
	Name  string
	Order int
	Run   func(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput)
}

var claudeDesktopRequestTelemetryHooks []claudeDesktopRequestTelemetryHook

// registerClaudeDesktopRequestTelemetryHook is called by topic files at init.
func registerClaudeDesktopRequestTelemetryHook(hook claudeDesktopRequestTelemetryHook) {
	if hook.Run == nil {
		return
	}
	claudeDesktopRequestTelemetryHooks = append(claudeDesktopRequestTelemetryHooks, hook)
	sort.SliceStable(claudeDesktopRequestTelemetryHooks, func(i, j int) bool {
		return claudeDesktopRequestTelemetryHooks[i].Order < claudeDesktopRequestTelemetryHooks[j].Order
	})
}

func runClaudeDesktopRequestTelemetryHooks(ctx context.Context, span *claudeDesktopRequestSpan, input claudeDesktopRequestTelemetryInput, selected func(order int) bool) {
	if span == nil {
		return
	}
	for _, hook := range claudeDesktopRequestTelemetryHooks {
		if selected == nil || selected(hook.Order) {
			hook.Run(ctx, span, input)
		}
	}
}

func claudeDesktopTranscriptSize(metadata ...map[string]any) *int64 {
	for _, values := range metadata {
		for _, key := range []string{"claude_desktop_transcript_size_bytes", "transcript_size_bytes"} {
			value, ok := values[key]
			if !ok {
				continue
			}
			var size int64
			switch typed := value.(type) {
			case int:
				size = int64(typed)
			case int32:
				size = int64(typed)
			case int64:
				size = typed
			case float64:
				if typed != float64(int64(typed)) {
					continue
				}
				size = int64(typed)
			case json.Number:
				parsed, errParse := typed.Int64()
				if errParse != nil {
					continue
				}
				size = parsed
			default:
				continue
			}
			if size < 0 {
				continue
			}
			return &size
		}
	}
	return nil
}

func claudeDesktopMCPServerCount(body []byte) int {
	servers := make(map[string]struct{})
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		rest, ok := strings.CutPrefix(name, "mcp__")
		if !ok {
			continue
		}
		server, _, ok := strings.Cut(rest, "__")
		server = strings.TrimSpace(server)
		if !ok || server == "" {
			continue
		}
		servers[server] = struct{}{}
	}
	return len(servers)
}

func observeClaudeDesktopTelemetryUsage(span *claudeDesktopRequestSpan, detail usage.Detail) {
	if span == nil || !span.Active() {
		return
	}
	span.ObserveUsage(claudetelemetry.Usage{
		InputTokens:              detail.InputTokens,
		CacheCreationInputTokens: detail.CacheCreationTokens,
		CacheReadInputTokens:     detail.CacheReadTokens,
		OutputTokens:             detail.OutputTokens,
	})
}

type claudeDesktopResponseMetricState struct {
	stopReason            string
	textContentLength     int
	thinkingContentLength int
	hasThinkingContent    bool
	toolNames             map[int]string
	toolInputLengths      map[int]int
}

func claudeDesktopResponseContentMetrics(payload []byte) claudeDesktopResponseMetricState {
	metrics := claudeDesktopResponseMetricState{
		stopReason:       strings.TrimSpace(gjson.GetBytes(payload, "stop_reason").String()),
		toolNames:        make(map[int]string),
		toolInputLengths: make(map[int]int),
	}
	for _, block := range gjson.GetBytes(payload, "content").Array() {
		switch block.Get("type").String() {
		case "text":
			metrics.textContentLength += claudeDesktopJavaScriptStringLength(block.Get("text").String())
		case "thinking":
			metrics.hasThinkingContent = true
			metrics.thinkingContentLength += claudeDesktopJavaScriptStringLength(block.Get("thinking").String())
		case "tool_use":
			index := len(metrics.toolNames)
			metrics.toolNames[index] = strings.TrimSpace(block.Get("name").String())
			metrics.toolInputLengths[index] = claudeDesktopCompactJSONLength(block.Get("input").Raw)
		}
	}
	return metrics
}

func claudeDesktopResponseMetrics(payload []byte) (string, int) {
	metrics := claudeDesktopResponseContentMetrics(payload)
	return metrics.stopReason, metrics.textContentLength
}

func (m *claudeDesktopResponseMetricState) observeStreamLine(line []byte) {
	if m == nil {
		return
	}
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" || !gjson.Valid(payload) {
		return
	}
	if value := strings.TrimSpace(gjson.Get(payload, "delta.stop_reason").String()); value != "" {
		m.stopReason = value
	}
	index := int(gjson.Get(payload, "index").Int())
	switch gjson.Get(payload, "type").String() {
	case "content_block_start":
		blockType := gjson.Get(payload, "content_block.type").String()
		switch blockType {
		case "thinking":
			m.hasThinkingContent = true
			m.thinkingContentLength += claudeDesktopJavaScriptStringLength(gjson.Get(payload, "content_block.thinking").String())
		case "tool_use":
			if m.toolNames == nil {
				m.toolNames = make(map[int]string)
			}
			if m.toolInputLengths == nil {
				m.toolInputLengths = make(map[int]int)
			}
			m.toolNames[index] = strings.TrimSpace(gjson.Get(payload, "content_block.name").String())
		}
	case "content_block_delta":
		switch gjson.Get(payload, "delta.type").String() {
		case "text_delta":
			m.textContentLength += claudeDesktopJavaScriptStringLength(gjson.Get(payload, "delta.text").String())
		case "thinking_delta":
			m.hasThinkingContent = true
			m.thinkingContentLength += claudeDesktopJavaScriptStringLength(gjson.Get(payload, "delta.thinking").String())
		case "input_json_delta":
			if m.toolInputLengths == nil {
				m.toolInputLengths = make(map[int]int)
			}
			m.toolInputLengths[index] += claudeDesktopJavaScriptStringLength(gjson.Get(payload, "delta.partial_json").String())
		}
	}
}

func observeClaudeDesktopStreamMetrics(line []byte, stopReason *string, textLength *int) {
	metrics := claudeDesktopResponseMetricState{}
	metrics.observeStreamLine(line)
	if stopReason != nil && metrics.stopReason != "" {
		*stopReason = metrics.stopReason
	}
	if textLength != nil {
		*textLength += metrics.textContentLength
	}
}

func (m *claudeDesktopResponseMetricState) thinkingLength() *int {
	if m == nil || !m.hasThinkingContent {
		return nil
	}
	value := m.thinkingContentLength
	return &value
}

func (m *claudeDesktopResponseMetricState) toolUseContentLengths() map[string]int {
	if m == nil || len(m.toolNames) == 0 {
		return nil
	}
	result := make(map[string]int)
	for index, name := range m.toolNames {
		if name = strings.TrimSpace(name); name != "" {
			result[name] += m.toolInputLengths[index]
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func claudeDesktopJavaScriptStringLength(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func claudeDesktopCompactJSONLength(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(raw)) == nil {
		raw = compact.String()
	}
	return claudeDesktopJavaScriptStringLength(raw)
}

func finishClaudeDesktopTelemetryFailure(ctx context.Context, span *claudeDesktopRequestSpan, err error) {
	if span == nil || !span.Active() {
		return
	}
	if err == nil {
		err = errClaudeDesktopStreamIncomplete
	}
	span.FinishFailure(ctx, claudeDesktopTelemetryFailureCategory(err), err)
}

func claudeDesktopTelemetryPermissionMode(metadata ...map[string]any) string {
	for _, values := range metadata {
		for _, key := range []string{"permission_mode", "permissionMode", "claude_permission_mode"} {
			value, _ := values[key].(string)
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func claudeDesktopTelemetryFailureCategory(err error) string {
	if err == nil {
		return "incomplete_stream"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errClaudeDesktopStreamIncomplete):
		return "incomplete_stream"
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
