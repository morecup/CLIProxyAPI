package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// doClaudeDesktopRecoverableRequest is shared by all three Messages entries.
// Only a complete, structured PTL response before any downstream output may
// trigger recovery. An unsupported history/model or failed summary leaves the
// original response intact. There is no recursive recovery of the continuation.
func (e *ClaudeExecutor) doClaudeDesktopRecoverableRequest(client *http.Client, request *http.Request, auth *cliproxyauth.Auth, state *claudeDesktopRequestExecution, metadata ...map[string]any) (*http.Response, error) {
	response, err := e.doClaudeUpstreamRequest(client, request)
	if err != nil {
		return response, err
	}
	response, err = e.retryClaudeDesktopRejectedCreditBeta(client, request, auth, state, response)
	if err != nil {
		return response, err
	}
	if state.span != nil {
		state.span.ObserveFirstByte(time.Now())
		state.span.ObserveHTTPResponse(response.StatusCode, response.Header)
	}
	if !e.desktopOnly || state.plan.Variant.Key.Role != claudeprofile.RoleMain || state.span == nil || state.span.prompt == nil ||
		response.StatusCode != http.StatusBadRequest || state.ctx.Err() != nil {
		return response, nil
	}
	const inspectionLimit = 1024 * 1024
	raw, complete := helps.PeekClaudeDesktopErrorBody(response, inspectionLimit)
	if !complete {
		return response, nil
	}
	decoded, errDecode := decodeResponseBody(io.NopCloser(bytes.NewReader(raw)), claudeResponseContentEncoding(response.Header))
	if errDecode != nil {
		return response, nil
	}
	body, errRead := io.ReadAll(io.LimitReader(decoded, inspectionLimit+1))
	errClose := decoded.Close()
	if errRead != nil || errClose != nil || len(body) > inspectionLimit || !gjson.ValidBytes(body) ||
		gjson.GetBytes(body, "type").String() != "error" || gjson.GetBytes(body, "error.type").String() != "invalid_request_error" {
		return response, nil
	}
	failure := claudeprompt.ClassifySDKReactiveFailure(gjson.GetBytes(body, "error.message").String())
	if failure.Reason != "prompt_too_long" {
		return response, nil
	}
	if _, errVariant := e.desktopProfile.Resolve(claudeprofile.RequestVariantKey{Model: gjson.GetBytes(state.body, "model").String(),
		LogicalModel: gjson.GetBytes(state.body, "model").String(), Role: claudeprofile.RoleCompaction, ThinkingDisplay: "omitted"}); errVariant != nil {
		return response, nil
	}
	view, errView := state.span.prompt.CompactionView(state.body)
	if errView != nil {
		return response, nil
	}
	defer view.Discard()
	if !view.ClaimReactiveFailure() {
		return response, nil
	}
	reactive := state.span.telemetry.BeginSDKReactiveCompaction(view)
	defer reactive.Close()
	physicalError := classifyClaudeUpstreamError(response.StatusCode, response.Header, body)
	helps.RecordAPIResponseMetadata(state.ctx, e.cfg, response.StatusCode, response.Header.Clone())
	helps.AppendAPIResponseChunk(state.ctx, e.cfg, body)
	state.span.FinishPhysicalFailure(state.ctx, physicalError)
	stopRecovery := func() (*http.Response, error) {
		if errCancelled := state.ctx.Err(); errCancelled != nil {
			if errClose := response.Body.Close(); errClose != nil {
				helps.LogWithRequestID(state.ctx).WithError(errClose).Warn("claude desktop: cancelled recovery response close failed")
			}
			return nil, errCancelled
		}
		return response, nil
	}
	nativeContext, errContext := helps.ResolveClaudeDesktopRecoveryContext(state.ctx, cliproxyexecutor.ClaudeDesktopSessionBinding{
		AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: state.facts.SessionID}, view)
	if errContext != nil {
		reactive.RecordContextUnavailable()
		return stopRecovery()
	}
	customInstructions := ""
	var compactHooks claudeprompt.SDKCompactHookRunner
	if nativeContext != nil {
		compactHooks = e.claudeDesktopCompactHookTelemetryRunner(auth, state.facts.SessionID, state.facts.LogicalModel, state.facts.PromptID, nativeContext.Hooks)
		pre, errPre := claudeprompt.RunSDKPreCompactHooks(state.ctx, compactHooks, "auto", nil, false)
		if errPre != nil {
			reactive.RecordContextUnavailable()
			return stopRecovery()
		}
		if pre.BlockedBy != "" {
			return stopRecovery()
		}
		customInstructions = pre.NewCustomInstructions
	} else {
		// Ordinary text recovery still works, but cannot be reported as native
		// context recovery when no owning runtime supplied its real operations.
		reactive.RecordContextUnavailable()
	}
	result, errSummary := helps.RunClaudeDesktopReactiveSummary(state.ctx, e, helps.ClaudeDesktopReactiveSummaryParams{
		Auth: auth, Bundle: e.desktopProfile, View: view, ParentRequest: state.body, SessionID: state.facts.SessionID, InitialTokenGap: failure.TokenGap,
		CustomInstructions: customInstructions, ObserveAttempt: reactive.ObserveAttempt})
	defer result.Payload.Discard()
	if errCancelled := state.ctx.Err(); errCancelled != nil {
		if errClose := response.Body.Close(); errClose != nil {
			helps.LogWithRequestID(state.ctx).WithError(errClose).Warn("claude desktop: cancelled recovery response close failed")
		}
		return nil, errCancelled
	}
	if errSummary != nil || !result.ReadyToApply {
		return response, nil
	}
	if nativeContext != nil {
		operations, errSnapshot := nativeContext.SnapshotAndReset(state.ctx, append([]claudeprompt.SDKHistoryMessage(nil), result.Preserve...))
		if errSnapshot != nil {
			reactive.RecordContextUnavailable()
			return stopRecovery()
		}
		restored, errRestore := result.Payload.Application.RestoreContext(state.ctx, claudeprompt.SDKCompactionRestoreParams{
			Owner: state.span.prompt, Summary: result.Payload.Text, Operations: operations, PostHooks: compactHooks,
			Normalize: nativeContext.Normalize, RemoteEnabled: nativeContext.RemoteEnabled})
		if errRestore != nil || restored.RestoreError != nil || restored.FallbackError != nil {
			reactive.RecordContextUnavailable()
		}
		if errRestore != nil {
			return stopRecovery()
		}
	}
	// Restoration has completed only for a supplied, scope-checked native
	// context. Final native lifecycle telemetry still requires its full schema,
	// boundary/transcript and timing facts; do not infer success from adoption.
	next, nextRequest, errPrepare := e.prepareClaudeDesktopCompactionContinuation(auth, request, *state, result.Payload.Application)
	if errPrepare != nil {
		return response, nil
	}
	// Persistence follows actual adoption, never merely a successful helper.
	// Failure leaves the API continuation usable and exposes the missing
	// durable state; it must not revert the already-committed prompt owner.
	next.facts.ContextError = state.facts.ContextLease.SaveAdopted(state.ctx, state.sourceBody, next.body)
	next.facts.ContextSaved = next.facts.ContextError == nil
	previousKey := state.span.prompt.FinalizerKey()
	helps.RememberClaudeDesktopContinuation(state.ctx, next.ctx, auth, e.desktopProfile.ProfileID, state.facts.SessionID,
		state.facts.ClientRequestID, state.sourceBody, next.body, next.facts.Prompt, next.facts.ClientRequestID)
	*state = next
	cliproxyexecutor.ClearUpstreamFailureFinalizer(state.ctx, previousKey)
	state.span = e.beginClaudeDesktopTelemetry(state.ctx, auth, claudeprofile.RoleMain, state.facts, state.body, metadata...)
	state.span.ObserveRequest(state.body, nextRequest.Header)
	if errClose := response.Body.Close(); errClose != nil {
		helps.LogWithRequestID(state.ctx).WithError(errClose).Warn("claude desktop: recovered error response close failed")
	}
	authID, authLabel, authType, authValue := claudeAuthLogIdentity(auth)
	helps.RecordAPIRequest(state.ctx, e.cfg, helps.UpstreamRequestLog{URL: nextRequest.URL.String(), Method: nextRequest.Method,
		Headers: nextRequest.Header.Clone(), Body: state.body, Provider: e.upstreamRequestLogProvider(),
		AuthID: authID, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue})
	response, err = e.doClaudeUpstreamRequest(client, nextRequest)
	if err == nil && state.span != nil {
		state.span.ObserveFirstByte(time.Now())
		state.span.ObserveHTTPResponse(response.StatusCode, response.Header)
	}
	return response, err
}

// The default main query uses the SDK instance's sticky beta state. Child and
// detached/explicit fallback contexts are not silently mapped onto that owner.
func (e *ClaudeExecutor) claudeDesktopCreditBetaHost(ctx context.Context, auth *cliproxyauth.Auth, session string) *claudefeatures.Host {
	if e == nil || !e.desktopOnly || e.desktopATIS == nil || ctx == nil || ctx.Err() != nil || auth == nil {
		return nil
	}
	owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	if owner.agentID != "" {
		return nil
	}
	if session == "" {
		session = owner.session
	}
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil || session == "" {
		return nil
	}
	return e.desktopATIS.queryHostFromContext(ctx, auth, claudeDesktopATISIdentityHash(enrollment), session)
}

// retryClaudeDesktopRejectedCreditBeta implements only mo/xjn's attributed
// default credit-beta branch. It neither mints nor retries a fallback credit,
// changes model/content, nor turns an arbitrary API 400 into a replay.
func (e *ClaudeExecutor) retryClaudeDesktopRejectedCreditBeta(client *http.Client, request *http.Request, auth *cliproxyauth.Auth, state *claudeDesktopRequestExecution, response *http.Response) (*http.Response, error) {
	if state == nil || request == nil || response == nil || response.StatusCode != http.StatusBadRequest ||
		state.plan.Variant.Key.Role != claudeprofile.RoleMain || request.GetBody == nil ||
		!helps.CanStripDefaultClaudeDesktopCreditBeta(request.Header, state.body) {
		return response, nil
	}
	host := e.claudeDesktopCreditBetaHost(state.ctx, auth, state.facts.SessionID)
	if host == nil {
		return response, nil
	}
	const inspectionLimit = 1024 * 1024
	raw, complete := helps.PeekClaudeDesktopErrorBody(response, inspectionLimit)
	if !complete {
		return response, nil
	}
	decoded, errDecode := decodeResponseBody(io.NopCloser(bytes.NewReader(raw)), claudeResponseContentEncoding(response.Header))
	if errDecode != nil {
		return response, nil
	}
	body, errRead := io.ReadAll(io.LimitReader(decoded, inspectionLimit+1))
	errClose := decoded.Close()
	if errRead != nil || errClose != nil || len(body) > inspectionLimit || !helps.ClaudeDesktopCreditBetaRejected(body) {
		return response, nil
	}
	nextBody, errBody := request.GetBody()
	if errBody != nil || nextBody == nil {
		return response, nil
	}
	if state.ctx.Err() != nil || !host.RejectBetaForSession(state.facts.SessionID, helps.ClaudeDesktopFallbackCreditBeta) {
		_ = nextBody.Close()
		return response, nil
	}
	next := request.Clone(request.Context())
	next.Body = nextBody
	helps.StripClaudeDesktopFallbackCreditBeta(next.Header)
	if state.span != nil {
		state.span.telemetry.ObserveDefaultCreditBetaStrip(!gjson.GetBytes(state.body, "stream").Bool())
	}
	if errClose := response.Body.Close(); errClose != nil {
		helps.LogWithRequestID(state.ctx).WithError(errClose).Warn("claude desktop: rejected beta response close failed")
	}
	// This is a protocol negotiation retry within the same query, with the
	// same immutable body, credential snapshot and client request identity.
	// No recursion: even an identically rejected retry is returned unchanged.
	return e.doClaudeUpstreamRequest(client, next)
}

func (e *ClaudeExecutor) prepareClaudeDesktopCompactionContinuation(auth *cliproxyauth.Auth, original *http.Request, state claudeDesktopRequestExecution, application *claudeprompt.SDKCompactionApplication) (claudeDesktopRequestExecution, *http.Request, error) {
	if application == nil {
		return state, nil, claudeprompt.ErrSDKCompactionContentUnknown
	}
	rows, err := json.Marshal(application.Messages())
	if err != nil {
		return state, nil, err
	}
	state.body, err = sjson.SetRawBytes(state.body, "messages", rows)
	if err != nil {
		return state, nil, err
	}
	state.lineage, state.facts.PreviousRequestID, err = e.beginClaudeDesktopRequestLineage(auth, state.facts.SessionID)
	if err != nil {
		return state, nil, err
	}
	state.ctx = cliproxyexecutor.WithFreshUpstreamQuery(state.ctx, time.Now())
	state.facts.ClientRequestID = uuid.NewString()
	state.facts.Input = claudeprompt.Submission{}
	state.plan.ClientRequestID = state.facts.ClientRequestID
	state.body, state.diagnostics = injectClaudeDiagnosticsForRole(state.body, auth, state.facts.SessionID, claudeprofile.RoleMain)
	cchSigning := claudeDesktopCCHSigningEnabled(claudeCredsToken(auth), original.URL.String())
	state.body, _, err = e.applyClaudeDesktopMessageProfile(state.ctx, auth, state.body, cchSigning, state.plan, state.facts)
	if err != nil {
		return state, nil, err
	}
	state.body, err = e.applyClaudeDesktopIdentity(state.body, auth, state.facts.SessionID)
	if err == nil && cchSigning {
		state.body, err = finalizeAnthropicMessagesBodyCCH(state.body)
	}
	if err == nil {
		state.body, err = e.finalizeClaudeDesktopBody(state.body, state.plan)
	}
	if err != nil {
		return state, nil, err
	}
	request := original.Clone(state.ctx)
	request.Body = io.NopCloser(bytes.NewReader(state.body))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(state.body)), nil }
	request.ContentLength = int64(len(state.body))
	if err = e.applyClaudeHeadersWithProfile(request, auth, claudeCredsToken(auth), gjson.GetBytes(state.body, "stream").Bool(), nil,
		state.body, state.plan, original.Header, state.facts.SessionID); err != nil {
		return state, nil, err
	}
	scope, _ := json.Marshal([]string{auth.ID, e.desktopProfile.ProfileID, auth.ProxyURL})
	attempt, _ := cliproxyexecutor.UpstreamAttemptFromContext(state.ctx)
	state.facts.Prompt, err = application.Commit(state.ctx, claudeprompt.Input{AccountID: string(scope), SessionID: state.facts.SessionID,
		PromptID: state.facts.PromptID, ClientRequestID: state.facts.ClientRequestID, Role: "main", Body: state.body, Attempt: 1, StartedAt: attempt.StartedAt})
	return state, request, err
}
