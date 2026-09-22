package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// httpRequestClaudeDesktop brings the raw Executor.HttpRequest entry point
// through the same Desktop planner, identity renderer, and transport boundary
// as Execute, ExecuteStream, and CountTokens. SDK callers therefore cannot
// accidentally emit a generic Go/Claude Code request under the claude provider.
func (e *ClaudeExecutor) httpRequestClaudeDesktop(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (_ *http.Response, err error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("claude desktop executor: request is nil")
	}
	if req.Method != http.MethodPost || !isAnthropicUpstreamURL(req.URL) {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop has no captured request class for %s %s", req.Method, claudeDesktopSafeURL(req))}}
	}
	role := claudeprofile.RequestRole("")
	switch req.URL.Path {
	case "/v1/messages":
	case "/v1/messages/count_tokens":
		role = claudeprofile.RoleCountTokens
	default:
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop has no captured request class for %s %s", req.Method, req.URL.Path)}}
	}

	body, errBody := readClaudeDesktopRawRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "claude desktop request body must be a JSON object"}}
	}
	incomingHeaders := req.Header.Clone()
	codeWire := isClaudeDesktopCodeWireRequest(incomingHeaders)
	desktopInput := claudeprompt.ObserveSubmissionContext(ctx, body, time.Now())
	logicalModel := gjson.GetBytes(body, "model").String()
	sessionID := claudeDesktopOwnedSessionUUID(incomingHeaders, body, codeWire)
	if role == "" && codeWire {
		role = claudeDesktopRequestRoleForWireClass(claudeDesktopHeaderValue(incomingHeaders, "x-claude-code-request-class"))
	}
	if role == "" {
		role = e.ownedClaudeDesktopRequestRole(ctx, body)
	}
	ctx, sessionID = e.bindClaudeDesktopQueryContext(ctx, auth, sessionID, role)
	ctx, releaseQuery := e.beginClaudeDesktopQueryLifetime(ctx)
	queryHandedOff := false
	defer func() {
		if !queryHandedOff {
			releaseQuery()
		}
	}()
	inputLease, errInput := e.beginClaudeDesktopInput(ctx, role)
	if errInput != nil {
		return nil, &claudeDesktopCancellationError{cause: errInput}
	}
	defer inputLease.Close()
	if ctx.Err() != nil {
		return nil, claudeDesktopRequestContextError(ctx)
	}
	promptID, clientRequestID := claudeDesktopRequestUUIDForHTTPRequest(req)

	lineageState := claudeDesktopLineageRequestState{}
	previousRequestID := ""
	diagnosticsState := claudeDiagnosticsRequestState{}
	transportStream := gjson.GetBytes(body, "stream").Bool()
	if role != claudeprofile.RoleCountTokens {
		body, _ = injectClaudeDesktopContextManagement(body)
		body, diagnosticsState = injectClaudeDiagnosticsForRole(body, auth, sessionID, role)
		body = normalizeClaudeSamplingForUpstream(body, true)
		body = enforceCacheControlLimit(body, 4)
		body = normalizeCacheControlTTL(body)
		if role == claudeprofile.RoleMain || role == claudeprofile.RoleCompaction {
			lineageState, previousRequestID, err = e.beginClaudeDesktopRequestLineage(auth, sessionID)
			if err != nil {
				return nil, err
			}
		}
	}

	planningHeaders := incomingHeaders
	if codeWire {
		// A first-party Code request may identify its semantic request class and
		// session, but it does not own the outbound beta/header profile.
		planningHeaders = nil
	}
	plan, errPlan := e.planClaudeDesktopRequestWithHints(body, role, logicalModel, planningHeaders)
	if errPlan != nil {
		return nil, errPlan
	}
	plan.ProgramOwnedSystem = codeWire
	var desktopContext *helps.ClaudeDesktopContextLease
	var desktopContextErr error
	if role == claudeprofile.RoleMain {
		desktopContext, body, desktopContextErr = e.desktopContexts.Resume(ctx, auth, e.desktopProfile.ProfileID, sessionID, body)
		ctx, body, promptID, clientRequestID = helps.ResumeClaudeDesktopContinuation(ctx, auth, e.desktopProfile.ProfileID, sessionID, promptID, clientRequestID, body)
	}
	if errInput := inputLease.Accept(); errInput != nil {
		return nil, &claudeDesktopCancellationError{cause: errInput}
	}
	desktopPrompt := helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, string(role), sessionID, promptID, clientRequestID, body)
	if desktopPrompt != nil {
		promptID = desktopPrompt.Identity().PromptID
	}
	plan.PromptID = promptID
	plan.NativePrompt = desktopPrompt
	plan.ClientRequestID = clientRequestID
	facts := e.newClaudeDesktopRuntimeFacts(auth, sessionID, logicalModel, promptID, clientRequestID, previousRequestID)
	facts.Prompt = desktopPrompt
	facts.ContextLease, facts.ContextError = desktopContext, desktopContextErr
	facts.Input = desktopInput
	desktopSourceBody := body
	span := e.beginClaudeDesktopTelemetry(ctx, auth, role, facts, body)
	requestSent := false
	defer func() {
		if err != nil && !requestSent {
			finishClaudeDesktopTelemetryFailure(ctx, span, err)
		}
	}()

	if role == claudeprofile.RoleCountTokens {
		body, _, err = e.applyClaudeDesktopCountTokensProfile(body, plan)
	} else {
		cchSigning := claudeDesktopCCHSigningEnabled(claudeCredsToken(auth), req.URL.String())
		body, _, err = e.applyClaudeDesktopMessageProfile(ctx, auth, body, cchSigning, plan, facts)
		if err == nil {
			body = applyClaudeDesktopStreamPolicy(body, plan, transportStream)
			transportStream = gjson.GetBytes(body, "stream").Bool()
		}
	}
	if err != nil {
		return nil, err
	}

	_, body = extractAndRemoveBetas(body)
	if e.desktopCapabilities().ToolAliases {
		body, _ = prepareClaudeDesktopToolNamesForUpstream(body, resolveClaudeMCPAliasOptions(ctx))
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, logicalModel)
	if role == claudeprofile.RoleCountTokens {
		body, _ = sjson.DeleteBytes(body, "metadata")
		body, _ = sjson.DeleteBytes(body, "context_management")
		body, _ = sjson.DeleteBytes(body, "diagnostics")
		plan, err = e.planClaudeDesktopRequestWithHints(body, role, logicalModel, incomingHeaders)
		if err != nil {
			return nil, err
		}
		plan.PromptID = promptID
		plan.ClientRequestID = clientRequestID
	} else {
		body, err = e.applyClaudeDesktopIdentity(body, auth, sessionID)
		if err != nil {
			return nil, err
		}
		if claudeDesktopCCHSigningEnabled(claudeCredsToken(auth), req.URL.String()) {
			body, err = finalizeAnthropicMessagesBodyCCH(body)
			if err != nil {
				return nil, fmt.Errorf("finalize Claude CCH: %w", err)
			}
		}
		if err = validateClaudeDesktopMidSystemMessageModel(body); err != nil {
			return nil, err
		}
	}
	body, err = e.finalizeClaudeDesktopBody(body, plan)
	if err != nil {
		return nil, err
	}

	httpReq := req.Clone(ctx)
	httpReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	httpReq.ContentLength = int64(len(body))
	httpReq.Header = incomingHeaders
	apiKey, _ := claudeCreds(auth)
	if err = e.applyClaudeHeadersWithProfile(httpReq, auth, apiKey, transportStream, nil, body, plan, incomingHeaders, sessionID); err != nil {
		return nil, err
	}
	if span != nil {
		span.ObserveRequest(body, httpReq.Header)
	}
	client, errClient := e.newClaudeUpstreamHTTPClient(ctx, auth, plan)
	if errClient != nil {
		return nil, errClient
	}
	execution := claudeDesktopRequestExecution{ctx: ctx, body: body, sourceBody: desktopSourceBody, plan: plan, facts: facts, span: span,
		lineage: lineageState, diagnostics: diagnosticsState}
	response, errSend := e.doClaudeDesktopRecoverableRequest(client, httpReq, auth, &execution)
	ctx, body, span = execution.ctx, execution.body, execution.span
	lineageState, diagnosticsState = execution.lineage, execution.diagnostics
	if errSend != nil {
		return nil, errSend
	}
	requestSent = true
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if errLineage := e.commitClaudeDesktopRequestLineage(lineageState, claudeDesktopResponseRequestID(response.Header)); errLineage != nil {
			helps.LogWithRequestID(ctx).WithError(errLineage).Warn("claude desktop: failed to persist request lineage")
		}
	}
	onSuccess := func() {
		e.runClaudeDesktopCountTokensCalibration(ctx, auth, role, sessionID, body)
	}
	response = observeClaudeDesktopRawResponse(ctx, response, transportStream, diagnosticsState, span, onSuccess)
	queryHandedOff = true
	return helps.RetainClaudeDesktopResponseLifetime(response, releaseQuery), nil
}

func readClaudeDesktopRawRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "claude desktop request body is required"}}
	}
	originalBody := req.Body
	body, errRead := io.ReadAll(originalBody)
	errClose := originalBody.Close()
	if errRead != nil {
		return nil, fmt.Errorf("read Claude Desktop raw request body: %w", errRead)
	}
	if errClose != nil {
		return nil, fmt.Errorf("close Claude Desktop raw request body: %w", errClose)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func claudeCredsToken(auth *cliproxyauth.Auth) string {
	token, _ := claudeCreds(auth)
	return token
}

type claudeDesktopHTTPRequestIdentityContextKey struct{}

type claudeDesktopHTTPRequestIdentity struct {
	promptID        string
	clientRequestID string
}

// claudeDesktopRequestUUIDForHTTPRequest keeps the Desktop request identity on
// the caller's request itself. net/http may replay a request through GetBody,
// and SDK retry loops commonly reuse or clone the same *http.Request; both must
// retain the original x-client-request-id instead of looking like a new turn.
func claudeDesktopRequestUUIDForHTTPRequest(req *http.Request) (promptID, clientRequestID string) {
	if req != nil {
		if identity, ok := req.Context().Value(claudeDesktopHTTPRequestIdentityContextKey{}).(claudeDesktopHTTPRequestIdentity); ok {
			if identity.promptID != "" && identity.clientRequestID != "" {
				return identity.promptID, identity.clientRequestID
			}
		}
	}
	promptID, clientRequestID = claudeDesktopRequestUUID()
	if req != nil {
		identity := claudeDesktopHTTPRequestIdentity{promptID: promptID, clientRequestID: clientRequestID}
		*req = *req.WithContext(context.WithValue(req.Context(), claudeDesktopHTTPRequestIdentityContextKey{}, identity))
	}
	return promptID, clientRequestID
}

func observeClaudeDesktopRawResponse(
	ctx context.Context,
	response *http.Response,
	streaming bool,
	diagnostics claudeDiagnosticsRequestState,
	span *claudeDesktopRequestSpan,
	onSuccess func(),
) *http.Response {
	if response == nil {
		finishClaudeDesktopTelemetryFailure(ctx, span, errors.New("Claude Desktop upstream returned a nil response"))
		return response
	}
	if span != nil {
		span.ObserveHTTPResponse(response.StatusCode, response.Header)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		finishClaudeDesktopTelemetryFailure(ctx, span, statusErr{code: response.StatusCode, msg: response.Status})
		return response
	}
	if response.Body == nil || response.Body == http.NoBody || response.ContentLength == 0 {
		if span != nil {
			span.FinishSuccess(ctx)
		}
		if onSuccess != nil {
			onSuccess()
		}
		return response
	}
	response.Body = &claudeDesktopObservedResponseBody{
		body:        response.Body,
		ctx:         ctx,
		streaming:   streaming,
		diagnostics: diagnostics,
		span:        span,
		onSuccess:   onSuccess,
	}
	return response
}

type claudeDesktopObservedResponseBody struct {
	body        io.ReadCloser
	ctx         context.Context
	streaming   bool
	diagnostics claudeDiagnosticsRequestState
	span        *claudeDesktopRequestSpan
	onSuccess   func()

	readMu sync.Mutex
	mu     sync.Mutex
	closed bool

	buffer          bytes.Buffer
	line            bytes.Buffer
	messageID       string
	responseMetrics claudeDesktopResponseMetricState
	completed       bool
	firstByte       bool
	finished        bool
}

func (b *claudeDesktopObservedResponseBody) Read(destination []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.mu.Unlock()

	n, errRead := b.body.Read(destination)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		if !b.firstByte {
			b.firstByte = true
			if b.span != nil {
				b.span.ObserveFirstByte(timeNow())
			}
		}
		b.observe(destination[:n])
	}
	if errors.Is(errRead, io.EOF) {
		b.observeFinalStreamLine()
		b.finish(nil)
	} else if errRead != nil {
		b.finish(b.contextErrorOr(errRead))
	}
	return n, errRead
}

func (b *claudeDesktopObservedResponseBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	// Body implementations used by net/http are expected to let Close unblock a
	// pending Read. Do this before waiting on readMu, then serialize the final
	// observation with any bytes returned by that Read.
	errClose := b.body.Close()
	b.readMu.Lock()
	defer b.readMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finished {
		b.observeFinalStreamLine()
		cause := errClose
		if cause == nil && (!b.streaming || !b.completed) {
			cause = b.contextErrorOr(errClaudeDesktopStreamIncomplete)
		}
		b.finish(cause)
	}
	return errClose
}

func (b *claudeDesktopObservedResponseBody) observe(chunk []byte) {
	if !b.streaming {
		_, _ = b.buffer.Write(chunk)
		return
	}
	_, _ = b.line.Write(chunk)
	pending := b.line.Bytes()
	for {
		newline := bytes.IndexByte(pending, '\n')
		if newline < 0 {
			break
		}
		line := bytes.TrimSuffix(pending[:newline], []byte{'\r'})
		observeClaudeStreamLine(line, &b.messageID, &b.completed)
		b.responseMetrics.observeStreamLine(line)
		if b.span != nil {
			b.span.ObserveStreamLine(line)
		}
		if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
			observeClaudeDesktopTelemetryUsage(b.span, detail)
		}
		pending = pending[newline+1:]
	}
	b.line.Reset()
	_, _ = b.line.Write(pending)
}

func (b *claudeDesktopObservedResponseBody) observeFinalStreamLine() {
	if !b.streaming || b.line.Len() == 0 {
		return
	}
	line := bytes.TrimSuffix(b.line.Bytes(), []byte{'\r'})
	observeClaudeStreamLine(line, &b.messageID, &b.completed)
	b.responseMetrics.observeStreamLine(line)
	if b.span != nil {
		b.span.ObserveStreamLine(line)
	}
	if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
		observeClaudeDesktopTelemetryUsage(b.span, detail)
	}
	b.line.Reset()
}

func (b *claudeDesktopObservedResponseBody) contextErrorOr(fallback error) error {
	if b.ctx != nil {
		if errContext := b.ctx.Err(); errContext != nil {
			return errContext
		}
	}
	return fallback
}

func (b *claudeDesktopObservedResponseBody) finish(cause error) {
	if b.finished {
		return
	}
	b.finished = true
	if cause == nil && b.streaming && !b.completed {
		cause = errClaudeDesktopStreamIncomplete
	}
	if cause != nil {
		finishClaudeDesktopTelemetryFailure(b.ctx, b.span, cause)
		return
	}
	if b.streaming {
		commitClaudeDiagnostics(b.diagnostics, b.messageID)
		if b.span != nil {
			b.span.ObserveResponseContentMetrics("", b.responseMetrics.stopReason, b.responseMetrics.textContentLength, b.responseMetrics.thinkingLength(), b.responseMetrics.toolUseContentLengths())
		}
	} else {
		payload := b.buffer.Bytes()
		commitClaudeDiagnostics(b.diagnostics, claudeMessageIDFromResponse(payload))
		observeClaudeDesktopTelemetryUsage(b.span, helps.ParseClaudeUsage(payload))
		if b.span != nil {
			b.span.ObserveResponsePayload(payload, false)
			metrics := claudeDesktopResponseContentMetrics(payload)
			b.span.ObserveResponseContentMetrics("", metrics.stopReason, metrics.textContentLength, metrics.thinkingLength(), metrics.toolUseContentLengths())
		}
	}
	if b.span != nil {
		b.span.FinishSuccess(b.ctx)
	}
	if b.onSuccess != nil {
		b.onSuccess()
	}
}

var timeNow = func() time.Time { return time.Now() }

var _ io.ReadCloser = (*claudeDesktopObservedResponseBody)(nil)

// Keep request URL logging and error strings free of OAuth query fragments.
func claudeDesktopSafeURL(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return strings.TrimSpace(req.URL.Scheme + "://" + req.URL.Host + req.URL.Path)
}
