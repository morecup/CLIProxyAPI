package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if errEligibility := e.validateClaudeDesktopAuth(auth); errEligibility != nil {
		return resp, errEligibility
	}
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	directAnthropic := isAnthropicUpstreamBase(baseURL)
	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	desktopCapabilities := e.desktopCapabilities()
	// Real Claude OAuth always signs CCH. An opted-in API key signs only where
	// native does, so a third-party gateway keeps a cache-stable billing header.
	// Default API-key and delegated-provider requests preserve the caller body.
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
	// Use an upstream stream whenever the downstream response needs translation
	// from Claude events. Native Claude responses use the JSON response path.
	upstreamStream := responseFormat != to
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
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, upstreamStream, helps.APIKeyModelIsCompat(req))
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, upstreamStream, helps.APIKeyModelIsCompat(req))
	desktopInput := claudeprompt.Submission{}
	if e.desktopOnly {
		desktopInput = claudeprompt.ObserveSubmissionContext(ctx, body, time.Now())
	}
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.thinkingProvider())
	if err != nil {
		return resp, err
	}
	desktopRole := e.ownedClaudeDesktopRequestRole(ctx, body)
	ctx, claudeSessionID = e.bindClaudeDesktopQueryContext(ctx, auth, claudeSessionID, desktopRole, opts.Metadata, req.Metadata)
	ctx, releaseQuery := e.beginClaudeDesktopQueryLifetime(ctx)
	defer releaseQuery()
	inputLease, errInput := e.beginClaudeDesktopInput(ctx, desktopRole)
	if errInput != nil {
		return resp, &claudeDesktopCancellationError{cause: errInput}
	}
	defer inputLease.Close()
	if e.desktopOnly && ctx.Err() != nil {
		return resp, claudeDesktopRequestContextError(ctx)
	}
	if e.desktopOnly && (desktopRole == "main" || desktopRole == "compaction") {
		lineageState, previousRequestID, err = e.beginClaudeDesktopRequestLineage(auth, claudeSessionID)
		if err != nil {
			return resp, err
		}
	}
	desktopPlan := claudeDesktopRequestPlan{}
	var desktopProfileApplied bool
	// Only the Messages endpoint on Anthropic itself was captured; count_tokens
	// keeps its own shape and other gateways never see this field.
	diagnosticsState := claudeDiagnosticsRequestState{}
	contextManagementState := claudeDesktopContextManagementState{
		eligible:    e.desktopOnly && directAnthropic,
		callerOwned: gjson.GetBytes(body, "context_management").Exists(),
	}
	if contextManagementState.eligible {
		body, contextManagementState.automaticallyInjected = injectClaudeDesktopContextManagement(body)
		if desktopCapabilities.Diagnostics {
			body, diagnosticsState = e.injectClaudeDesktopDiagnosticsForRole(body, auth, claudeSessionID, desktopRole)
		}
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body, contextManagementState.payloadRuleTouched = helps.ApplyPayloadConfigWithRequestTracked(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers, "context_management")
	body = ensureModelMaxTokens(body, baseModel)

	if err = validateClaudeOpus55Request(body, directAnthropic); err != nil {
		return resp, err
	}
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
	// Compatibility cache insertion may push the total over 4 when the client
	// already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	body = normalizeCacheControlTTL(body)
	var errPlan error
	desktopPlan, errPlan = e.planClaudeDesktopRequestInContext(ctx, body, desktopRole, baseModel, incomingHeaders)
	if errPlan != nil {
		return resp, errPlan
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
			return resp, &claudeDesktopCancellationError{cause: errInput}
		}
		desktopPrompt = helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, string(desktopRole), claudeSessionID, promptID, clientRequestID, body, opts.Metadata, req.Metadata)
		if desktopPrompt != nil {
			promptID = desktopPrompt.Identity().PromptID
		}
	}
	desktopPlan.PromptID = promptID
	desktopPlan.NativePrompt = desktopPrompt
	desktopPlan.ClientRequestID = clientRequestID
	desktopFacts := e.newClaudeDesktopRuntimeFactsForPlan(auth, claudeSessionID, baseModel, promptID, clientRequestID, previousRequestID, desktopPlan, opts.Metadata, req.Metadata)
	desktopFacts.Prompt = desktopPrompt
	desktopFacts.ContextLease, desktopFacts.ContextError = desktopContext, desktopContextErr
	desktopFacts.Input = desktopInput
	desktopSourceBody := body
	desktopTelemetrySpan := e.beginClaudeDesktopTelemetry(ctx, auth, desktopRole, desktopFacts, body, opts.Metadata, req.Metadata)
	defer func() {
		if desktopTelemetrySpan == nil || !desktopTelemetrySpan.Active() {
			return
		}
		if err != nil {
			finishClaudeDesktopTelemetryFailure(ctx, desktopTelemetrySpan, err)
			err = attachClaudeDesktopRetryTelemetry(err, desktopTelemetrySpan)
			return
		}
		desktopTelemetrySpan.FinishSuccess(ctx)
	}()
	body, desktopProfileApplied, err = e.applyClaudeDesktopMessageProfile(ctx, auth, body, cchSigning, desktopPlan, desktopFacts)
	if err != nil {
		return resp, err
	}
	if e.desktopOnly {
		body = applyClaudeDesktopStreamPolicy(body, desktopPlan, upstreamStream)
		// Title/web-helper profiles can require SSE even for native Execute.
		// Response observation must follow the final wire mode, not the caller.
		upstreamStream = gjson.GetBytes(body, "stream").Bool()
	} else {
		body = helps.SetBoolIfDifferent(body, "stream", upstreamStream)
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
			return resp, err
		}
	}
	if cchSigning {
		bodyForUpstream, err = finalizeAnthropicMessagesBodyCCH(bodyForUpstream)
		if err != nil {
			return resp, fmt.Errorf("finalize Claude CCH: %w", err)
		}
	}
	if e.desktopOnly {
		bodyForUpstream, err = e.finalizeClaudeDesktopBody(bodyForUpstream, desktopPlan)
		if err != nil {
			return resp, err
		}
	}
	// Runs on the finished body: payload rules can rewrite model and messages
	// long after translation, so an earlier check would not describe the request
	// that is about to be sent.
	if e.desktopOnly {
		if errMidSystem := validateClaudeDesktopMidSystemMessageModel(bodyForUpstream); errMidSystem != nil {
			return resp, errMidSystem
		}
	}
	if err = validateClaudeOpus55Request(bodyForUpstream, directAnthropic); err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	if errHeaders := e.applyClaudeHeadersWithProfile(
		httpReq,
		auth,
		apiKey,
		upstreamStream,
		extraBetas,
		bodyForUpstream,
		desktopPlan,
		incomingHeaders,
		claudeSessionID,
	); errHeaders != nil {
		return resp, errHeaders
	}
	if desktopTelemetrySpan != nil {
		desktopTelemetrySpan.ObserveRequest(bodyForUpstream, httpReq.Header)
	}
	fastRequest := directAnthropic && claudeRequestIsFast(httpReq, bodyForUpstream)
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
		return resp, err
	}
	httpClient = reporter.TrackHTTPClient(httpClient)
	execution := claudeDesktopRequestExecution{ctx: ctx, body: bodyForUpstream, sourceBody: desktopSourceBody, plan: desktopPlan, facts: desktopFacts,
		span: desktopTelemetrySpan, lineage: lineageState, diagnostics: diagnosticsState}
	httpResp, err := e.doClaudeDesktopRecoverableRequest(httpClient, httpReq, auth, &execution, opts.Metadata, req.Metadata)
	ctx, bodyForUpstream, desktopTelemetrySpan = execution.ctx, execution.body, execution.span
	lineageState, diagnosticsState = execution.lineage, execution.diagnostics
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, wrapClaudeFastRequestError(fastRequest, 0, err)
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
				return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errClassified)
			}
			return resp, errClassified
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
			return resp, newClaudeFastDirectResponseError(httpResp, b)
		}
		return resp, classifyClaudeUpstreamError(httpResp.StatusCode, httpResp.Header, b)
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, err)
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	var data []byte
	if upstreamStream && desktopTelemetrySpan != nil && desktopTelemetrySpan.prompt != nil {
		data, err = helps.ReadClaudeSSEWithObserver(decodedBody, func(line []byte) error {
			restoredLine, errRestore := restoreClaudeDesktopToolNamesFromStreamLine(line, oauthToolNamesReverseMap)
			if errRestore != nil {
				return fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
			}
			desktopTelemetrySpan.observePromptStreamLine(restoredLine)
			return nil
		})
	} else {
		data, err = io.ReadAll(decodedBody)
	}
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, err)
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if upstreamStream {
		if errValidate := validateClaudeStreamingResponse(data); errValidate != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errValidate)
			return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errValidate)
		}
		upstreamRequestID := claudeDesktopResponseRequestID(httpResp.Header)
		if errLineage := e.commitClaudeDesktopRequestLineage(lineageState, upstreamRequestID); errLineage != nil {
			helps.LogWithRequestID(ctx).WithError(errLineage).Warn("claude desktop: failed to persist request lineage")
		}
		commitClaudeDiagnostics(diagnosticsState, claudeMessageIDFromSSE(data))
		lines := bytes.Split(data, []byte("\n"))
		responseMetrics := claudeDesktopResponseMetricState{}
		for i, line := range lines {
			responseMetrics.observeStreamLine(line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
				observeClaudeDesktopTelemetryUsage(desktopTelemetrySpan, detail)
			}
			restoredLine, errRestore := restoreClaudeDesktopToolNamesFromStreamLine(line, oauthToolNamesReverseMap)
			if errRestore != nil {
				errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
				helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
				return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errRestore)
			}
			lines[i] = restoredLine
		}
		if desktopTelemetrySpan != nil {
			desktopTelemetrySpan.ObserveResponseContentMetrics(upstreamRequestID, responseMetrics.stopReason, responseMetrics.textContentLength, responseMetrics.thinkingLength(), responseMetrics.toolUseContentLengths())
		}
		data = bytes.Join(lines, []byte("\n"))
		if desktopTelemetrySpan != nil {
			desktopTelemetrySpan.ObserveResponsePayload(data, true)
		}
		if responseFormat == to {
			data, err = helps.CollectClaudeMessageSSE(data)
			if err != nil {
				return resp, err
			}
		}
	} else {
		upstreamRequestID := claudeDesktopResponseRequestID(httpResp.Header)
		if errLineage := e.commitClaudeDesktopRequestLineage(lineageState, upstreamRequestID); errLineage != nil {
			helps.LogWithRequestID(ctx).WithError(errLineage).Warn("claude desktop: failed to persist request lineage")
		}
		commitClaudeDiagnostics(diagnosticsState, claudeMessageIDFromResponse(data))
		detail := helps.ParseClaudeUsage(data)
		reporter.Publish(ctx, detail)
		observeClaudeDesktopTelemetryUsage(desktopTelemetrySpan, detail)
		if desktopTelemetrySpan != nil {
			responseMetrics := claudeDesktopResponseContentMetrics(data)
			desktopTelemetrySpan.ObserveResponseContentMetrics(upstreamRequestID, responseMetrics.stopReason, responseMetrics.textContentLength, responseMetrics.thinkingLength(), responseMetrics.toolUseContentLengths())
		}
		var errRestore error
		data, errRestore = restoreClaudeDesktopToolNamesFromResponse(data, oauthToolNamesReverseMap)
		if errRestore != nil {
			errRestore = fmt.Errorf("restore Claude OAuth tool name from response: %w", errRestore)
			helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
			return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errRestore)
		}
		if desktopTelemetrySpan != nil {
			desktopTelemetrySpan.ObserveResponsePayload(data, false)
		}
	}
	data = e.restoreResponseModel(data, req.Model)
	cacheClaudeThinkingReplayResponse(ctx, replayScope, data)
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		to,
		responseFormat,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		data,
		&param,
	)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	e.runClaudeDesktopCountTokensCalibration(ctx, auth, desktopRole, claudeSessionID, bodyForUpstream)
	responseHeaders := httpResp.Header.Clone()
	if upstreamStream && responseFormat == to {
		responseHeaders.Set("Content-Type", "application/json")
		responseHeaders.Del("Content-Length")
		responseHeaders.Del("Content-Encoding")
		responseHeaders.Del("Transfer-Encoding")
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: responseHeaders}
	return resp, nil
}
