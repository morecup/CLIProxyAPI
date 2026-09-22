package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if errEligibility := e.validateClaudeDesktopAuth(auth); errEligibility != nil {
		return nil, errEligibility
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	desktopCapabilities := e.desktopCapabilities()
	callerContext := ctx
	defer func() {
		if cancelErr := newClaudeDesktopCancellationError(callerContext, desktopCapabilities.Cancellation, err); cancelErr != nil {
			err = cancelErr
		}
	}()
	cchSigning := e.desktopOnly && claudeDesktopCCHSigningEnabled(apiKey, url)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	var replayScope claudeThinkingReplayScope
	if claudeThinkingReplayEnabled(auth, req, opts) {
		req, replayScope = prepareClaudeThinkingReplayRequest(ctx, auth, req, opts)
	}
	defer func() {
		if err != nil && replayScope.replayApplied && shouldClearKimiThinkingReplayAfterError(err) {
			clearClaudeThinkingReplayContent(ctx, replayScope)
		}
	}()
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	incomingHeaders := resolveIncomingClaudeHeaders(ctx, opts.Headers)
	claudeSessionID := ""
	if desktopCapabilities.CredentialMetadata {
		claudeSessionID = helps.ClaudeAgentSessionUUIDForRequest(incomingHeaders, originalPayload, req.Payload, false, opts.Metadata, req.Metadata)
	}
	promptID, clientRequestID := claudeDesktopRequestUUID(opts.Metadata, req.Metadata)
	lineageState := claudeDesktopLineageRequestState{}
	previousRequestID := ""
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, true, helps.APIKeyModelIsCompat(req))
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, true, helps.APIKeyModelIsCompat(req))
	desktopInput := claudeprompt.Submission{}
	if e.desktopOnly {
		desktopInput = claudeprompt.ObserveSubmissionContext(ctx, body, time.Now())
	}
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.thinkingProvider())
	if err != nil {
		return nil, err
	}
	desktopRole := e.ownedClaudeDesktopRequestRole(ctx, body)
	ctx, claudeSessionID = e.bindClaudeDesktopQueryContext(ctx, auth, claudeSessionID, desktopRole, opts.Metadata, req.Metadata)
	ctx, releaseQuery := e.beginClaudeDesktopQueryLifetime(ctx)
	queryHandedOff := false
	defer func() {
		if !queryHandedOff {
			releaseQuery()
		}
	}()
	inputLease, errInput := e.beginClaudeDesktopInput(ctx, desktopRole)
	if errInput != nil {
		return nil, &claudeDesktopCancellationError{cause: errInput}
	}
	defer inputLease.Close()
	if e.desktopOnly && ctx.Err() != nil {
		return nil, claudeDesktopRequestContextError(ctx)
	}
	if e.desktopOnly && (desktopRole == "main" || desktopRole == "compaction") {
		lineageState, previousRequestID, err = e.beginClaudeDesktopRequestLineage(auth, claudeSessionID)
		if err != nil {
			return nil, err
		}
	}
	desktopPlan := claudeDesktopRequestPlan{}
	var desktopProfileApplied bool
	// Only the Messages endpoint on Anthropic itself was captured; count_tokens
	// keeps its own shape and other gateways never see this field.
	diagnosticsState := claudeDiagnosticsRequestState{}
	contextManagementState := claudeDesktopContextManagementState{
		eligible:    e.desktopOnly && isAnthropicUpstreamBase(baseURL),
		callerOwned: gjson.GetBytes(body, "context_management").Exists(),
	}
	if contextManagementState.eligible {
		body, contextManagementState.automaticallyInjected = injectClaudeDesktopContextManagement(body)
		if desktopCapabilities.Diagnostics {
			body, diagnosticsState = injectClaudeDiagnosticsForRole(body, auth, claudeSessionID, desktopRole)
		}
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body, contextManagementState.payloadRuleTouched = helps.ApplyPayloadConfigWithRequestTracked(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers, "context_management")
	body = ensureModelMaxTokens(body, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = reconcileClaudeDesktopContextManagement(body, contextManagementState)
	body = normalizeClaudeSamplingForUpstream(body, e.desktopOnly)

	// The compatibility provider adds cache breakpoints only when the caller did
	// not provide any. Desktop placement is owned by the selected bundle variant.
	cpaOwnsCacheControl := !e.desktopOnly && shouldEnsureCacheControl(body)
	if cpaOwnsCacheControl {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	body = normalizeCacheControlTTL(body)
	var errPlan error
	desktopPlan, errPlan = e.planClaudeDesktopRequestWithHints(body, desktopRole, baseModel, incomingHeaders)
	if errPlan != nil {
		return nil, errPlan
	}
	var desktopPrompt *claudeprompt.Request
	var desktopContext *helps.ClaudeDesktopContextLease
	var desktopContextErr error
	if e.desktopOnly && e.desktopProfile != nil {
		if desktopRole == "main" {
			desktopContext, body, desktopContextErr = e.desktopContexts.Resume(ctx, auth, e.desktopProfile.ProfileID, claudeSessionID, body)
			ctx, body, promptID, clientRequestID = helps.ResumeClaudeDesktopContinuation(ctx, auth, e.desktopProfile.ProfileID, claudeSessionID, promptID, clientRequestID, body)
		}
		if errInput := inputLease.Accept(); errInput != nil {
			return nil, &claudeDesktopCancellationError{cause: errInput}
		}
		desktopPrompt = helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, string(desktopRole), claudeSessionID, promptID, clientRequestID, body, opts.Metadata, req.Metadata)
		if desktopPrompt != nil {
			promptID = desktopPrompt.Identity().PromptID
		}
	}
	desktopPlan.PromptID = promptID
	desktopPlan.NativePrompt = desktopPrompt
	desktopPlan.ClientRequestID = clientRequestID
	desktopFacts := e.newClaudeDesktopRuntimeFacts(auth, claudeSessionID, baseModel, promptID, clientRequestID, previousRequestID, opts.Metadata, req.Metadata)
	desktopFacts.Prompt = desktopPrompt
	desktopFacts.ContextLease, desktopFacts.ContextError = desktopContext, desktopContextErr
	desktopFacts.Input = desktopInput
	desktopSourceBody := body
	desktopTelemetrySpan := e.beginClaudeDesktopTelemetry(ctx, auth, desktopRole, desktopFacts, body, opts.Metadata, req.Metadata)
	desktopTelemetryHandedOff := false
	defer func() {
		if desktopTelemetryHandedOff || desktopTelemetrySpan == nil || !desktopTelemetrySpan.Active() {
			return
		}
		finishClaudeDesktopTelemetryFailure(ctx, desktopTelemetrySpan, err)
		err = attachClaudeDesktopRetryTelemetry(err, desktopTelemetrySpan)
	}()
	body, desktopProfileApplied, err = e.applyClaudeDesktopMessageProfile(ctx, auth, body, cchSigning, desktopPlan, desktopFacts)
	if err != nil {
		return nil, err
	}
	if e.desktopOnly {
		body = applyClaudeDesktopStreamPolicy(body, desktopPlan, true)
	}

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	var oauthToolNamesReverseMap map[string]string
	if desktopCapabilities.ToolAliases && desktopProfileApplied {
		mcpAliases := resolveClaudeMCPAliasOptions(ctx)
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeDesktopToolNamesForUpstream(bodyForUpstream, mcpAliases)
	}
	bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, bodyForUpstream, baseModel, helps.APIKeyModelIsCompat(req))
	if desktopCapabilities.CredentialMetadata {
		bodyForUpstream, err = e.applyClaudeDesktopIdentity(bodyForUpstream, auth, claudeSessionID)
		if err != nil {
			return nil, err
		}
	}
	if cchSigning {
		bodyForUpstream, err = finalizeAnthropicMessagesBodyCCH(bodyForUpstream)
		if err != nil {
			return nil, fmt.Errorf("finalize Claude CCH: %w", err)
		}
	}
	if e.desktopOnly {
		bodyForUpstream, err = e.finalizeClaudeDesktopBody(bodyForUpstream, desktopPlan)
		if err != nil {
			return nil, err
		}
	}
	// Runs on the finished body: payload rules can rewrite model and messages
	// long after translation, so an earlier check would not describe the request
	// that is about to be sent.
	if e.desktopOnly {
		if errMidSystem := validateClaudeDesktopMidSystemMessageModel(bodyForUpstream); errMidSystem != nil {
			return nil, errMidSystem
		}
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	if errHeaders := e.applyClaudeHeadersWithProfile(
		httpReq,
		auth,
		apiKey,
		true,
		extraBetas,
		bodyForUpstream,
		desktopPlan,
		incomingHeaders,
		claudeSessionID,
	); errHeaders != nil {
		return nil, errHeaders
	}
	if desktopTelemetrySpan != nil {
		desktopTelemetrySpan.ObserveRequest(bodyForUpstream, httpReq.Header)
	}
	fastRequest := isAnthropicUpstreamBase(baseURL) && claudeRequestIsFast(httpReq, bodyForUpstream)
	authID, authLabel, authType, authValue := claudeAuthLogIdentity(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      bodyForUpstream,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient, err := e.newClaudeUpstreamHTTPClient(ctx, auth, desktopPlan)
	if err != nil {
		return nil, err
	}
	httpClient = reporter.TrackHTTPClient(httpClient)
	execution := claudeDesktopRequestExecution{ctx: ctx, body: bodyForUpstream, sourceBody: desktopSourceBody, plan: desktopPlan, facts: desktopFacts,
		span: desktopTelemetrySpan, lineage: lineageState, diagnostics: diagnosticsState}
	httpResp, err := e.doClaudeDesktopRecoverableRequest(httpClient, httpReq, auth, &execution, opts.Metadata, req.Metadata)
	ctx, bodyForUpstream, desktopTelemetrySpan = execution.ctx, execution.body, execution.span
	lineageState, diagnosticsState = execution.lineage, execution.diagnostics
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, wrapClaudeFastRequestError(fastRequest, 0, err)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			errClassified := classifyClaudeUpstreamError(httpResp.StatusCode, httpResp.Header, []byte(msg))
			if fastRequest {
				return nil, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errClassified)
			}
			return nil, errClassified
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		if fastRequest {
			return nil, newClaudeFastDirectResponseError(httpResp, b)
		}
		return nil, classifyClaudeUpstreamError(httpResp.StatusCode, httpResp.Header, b)
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, err)
	}
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	upstreamRequestID := claudeDesktopResponseRequestID(httpResp.Header)
	go func() {
		defer close(out)
		defer releaseQuery()
		telemetryCompleted := false
		var telemetryErr error
		defer func() {
			if telemetryCompleted {
				e.runClaudeDesktopCountTokensCalibration(ctx, auth, desktopRole, claudeSessionID, bodyForUpstream)
			}
		}()
		defer func() {
			if desktopTelemetrySpan == nil || !desktopTelemetrySpan.Active() {
				return
			}
			if telemetryCompleted {
				desktopTelemetrySpan.FinishSuccess(ctx)
				return
			}
			finishClaudeDesktopTelemetryFailure(ctx, desktopTelemetrySpan, telemetryErr)
		}()
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()
		emitCancellation := func(cause error) bool {
			cancelErr := newClaudeDesktopCancellationError(ctx, desktopCapabilities.Cancellation, cause)
			if cancelErr == nil {
				return false
			}
			telemetryErr = cancelErr
			helps.RecordAPIResponseError(ctx, e.cfg, cancelErr)
			reporter.PublishFailure(ctx, cancelErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: cancelErr}:
			default:
			}
			return true
		}
		emitResponseError := func(errResponse error) {
			errResponse = wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errResponse)
			telemetryErr = errResponse
			streamErr := attachClaudeDesktopRetryTelemetry(errResponse, desktopTelemetrySpan)
			helps.RecordAPIResponseError(ctx, e.cfg, errResponse)
			reporter.PublishFailure(ctx, errResponse)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
		}

		// If the response target is Claude, directly forward complete SSE events without translation.
		if responseFormat == to {
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			var event bytes.Buffer
			var upstreamMessageID string
			responseMetrics := claudeDesktopResponseMetricState{}
			upstreamCompleted := false
			flushEvent := func() bool {
				if event.Len() == 0 {
					return true
				}
				cloned := bytes.Clone(event.Bytes())
				event.Reset()
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
					return true
				case <-ctx.Done():
					return false
				}
			}
			for scanner.Scan() {
				line := scanner.Bytes()
				observeClaudeStreamLine(line, &upstreamMessageID, &upstreamCompleted)
				responseMetrics.observeStreamLine(line)
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
					observeClaudeDesktopTelemetryUsage(desktopTelemetrySpan, detail)
				}
				restoredLine, errRestore := restoreClaudeDesktopToolNamesFromStreamLine(line, oauthToolNamesReverseMap)
				if errRestore != nil {
					emitResponseError(fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore))
					return
				}
				if desktopTelemetrySpan != nil {
					desktopTelemetrySpan.ObserveStreamLine(restoredLine)
				}
				line = e.restoreResponseModel(restoredLine, req.Model)
				event.Write(line)
				event.WriteByte('\n')
				if len(bytes.TrimSpace(line)) == 0 && !flushEvent() {
					emitCancellation(ctx.Err())
					return
				}
			}
			if !flushEvent() {
				emitCancellation(ctx.Err())
				return
			}
			if emitCancellation(scanner.Err()) {
				return
			}
			if errScan := scanner.Err(); errScan != nil {
				emitResponseError(errScan)
				return
			}
			if upstreamCompleted {
				if errLineage := e.commitClaudeDesktopRequestLineage(lineageState, upstreamRequestID); errLineage != nil {
					helps.LogWithRequestID(ctx).WithError(errLineage).Warn("claude desktop: failed to persist request lineage")
				}
				commitClaudeDiagnostics(diagnosticsState, upstreamMessageID)
				if desktopTelemetrySpan != nil {
					desktopTelemetrySpan.ObserveResponseContentMetrics(upstreamRequestID, responseMetrics.stopReason, responseMetrics.textContentLength, responseMetrics.thinkingLength(), responseMetrics.toolUseContentLengths())
				}
				telemetryCompleted = true
			} else if e.desktopOnly {
				emitResponseError(errClaudeDesktopStreamIncomplete)
			}
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		var upstreamMessageID string
		responseMetrics := claudeDesktopResponseMetricState{}
		upstreamCompleted := false
		for scanner.Scan() {
			line := scanner.Bytes()
			observeClaudeStreamLine(line, &upstreamMessageID, &upstreamCompleted)
			responseMetrics.observeStreamLine(line)
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
				observeClaudeDesktopTelemetryUsage(desktopTelemetrySpan, detail)
			}
			restoredLine, errRestore := restoreClaudeDesktopToolNamesFromStreamLine(line, oauthToolNamesReverseMap)
			if errRestore != nil {
				emitResponseError(fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore))
				return
			}
			if desktopTelemetrySpan != nil {
				desktopTelemetrySpan.ObserveStreamLine(restoredLine)
			}
			line = e.restoreResponseModel(restoredLine, req.Model)
			chunks := sdktranslator.TranslateStream(
				ctx,
				to,
				responseFormat,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			if responseFormat == sdktranslator.FormatOpenAIResponse {
				for i, chunk := range chunks {
					chunks[i] = helps.EnsureResponsesUsageDetails(chunk)
				}
			}
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					emitCancellation(ctx.Err())
					return
				}
			}
		}
		if emitCancellation(scanner.Err()) {
			return
		}
		if errScan := scanner.Err(); errScan != nil {
			emitResponseError(errScan)
			return
		}
		if upstreamCompleted {
			if errLineage := e.commitClaudeDesktopRequestLineage(lineageState, upstreamRequestID); errLineage != nil {
				helps.LogWithRequestID(ctx).WithError(errLineage).Warn("claude desktop: failed to persist request lineage")
			}
			commitClaudeDiagnostics(diagnosticsState, upstreamMessageID)
			if desktopTelemetrySpan != nil {
				desktopTelemetrySpan.ObserveResponseContentMetrics(upstreamRequestID, responseMetrics.stopReason, responseMetrics.textContentLength, responseMetrics.thinkingLength(), responseMetrics.toolUseContentLengths())
			}
			telemetryCompleted = true
		} else if e.desktopOnly {
			emitResponseError(errClaudeDesktopStreamIncomplete)
		}
	}()
	result := &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}
	if replayScope.valid() {
		result = wrapClaudeThinkingReplayStream(ctx, result, replayScope)
	}
	desktopTelemetryHandedOff = true
	queryHandedOff = true
	return result, nil
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !gjson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}
