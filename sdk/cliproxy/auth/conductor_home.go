package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	homeAuthCountMetadataKey  = "__cliproxy_home_auth_count"
	homeRetryRoundMetadataKey = "request_retry_round"
	// ExcludedAuthIDsMetadataKey stores credential IDs already attempted in the
	// current request retry round.
	ExcludedAuthIDsMetadataKey = "excluded_auth_ids"
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// HomeDispatchBundle is the immutable client and registry pair for one Home lifetime.
type HomeDispatchBundle struct {
	client     homeAuthDispatcher
	registry   *executionregistry.Registry
	generation uint64
}

// PublishHomeDispatch publishes the selectable Home lifetime as one atomic bundle.
func (m *Manager) PublishHomeDispatch(client homeAuthDispatcher, registry *executionregistry.Registry, generation uint64) *HomeDispatchBundle {
	if m == nil || client == nil || registry == nil {
		return nil
	}
	bundle := &HomeDispatchBundle{client: client, registry: registry, generation: generation}
	m.homeDispatchBundle.Store(bundle)
	return bundle
}

// ClearHomeDispatchBundle removes bundle only when it still belongs to the active lifetime.
func (m *Manager) ClearHomeDispatchBundle(bundle *HomeDispatchBundle) bool {
	if m == nil || bundle == nil {
		return false
	}
	return m.homeDispatchBundle.CompareAndSwap(bundle, nil)
}

// HomeDispatchBundle returns the active Home lifetime bundle.
func (m *Manager) HomeDispatchBundle() *HomeDispatchBundle {
	if m == nil {
		return nil
	}
	return m.homeDispatchBundle.Load()
}

// SetHomeExecutionRegistry preserves the legacy registry API for callers that also install the current dispatcher.
func (m *Manager) SetHomeExecutionRegistry(registry *executionregistry.Registry) {
	if m == nil {
		return
	}
	m.PublishHomeDispatch(currentHomeDispatcher(), registry, 0)
}

// ClearHomeExecutionRegistry removes a matching legacy registry bundle.
func (m *Manager) ClearHomeExecutionRegistry(registry *executionregistry.Registry) bool {
	bundle := m.HomeDispatchBundle()
	if bundle == nil || bundle.registry != registry {
		return false
	}
	return m.ClearHomeDispatchBundle(bundle)
}

// HomeExecutionRegistry returns the registry from the active Home lifetime bundle.
func (m *Manager) HomeExecutionRegistry() *executionregistry.Registry {
	bundle := m.HomeDispatchBundle()
	if bundle == nil {
		return nil
	}
	return bundle.registry
}

// HomeEnabled reports whether the home control plane integration is enabled in the runtime config.
func (m *Manager) HomeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Home.Enabled
}

func (m *Manager) localExecutionAllowed() bool {
	return m != nil && !m.HomeEnabled()
}

func (m *Manager) localFallbackAuth(authID string) *Auth {
	if !m.localExecutionAllowed() {
		return nil
	}
	m.mu.RLock()
	auth := m.auths[strings.TrimSpace(authID)]
	m.mu.RUnlock()
	if auth == nil {
		return nil
	}
	return auth.Clone()
}

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Code         string `json:"code,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	RequestRetry *int   `json:"request_retry,omitempty"`
}

type homeDispatchRetryAfterError struct {
	cause           *Error
	retryAfter      time.Duration
	requestRetry    int
	hasRequestRetry bool
}

// homeRetryRoundExhaustedError marks a terminal error produced after the
// current Home credential round has been exhausted. The wrapped error retains
// its status and retry-after metadata for the outer request retry policy.
type homeRetryRoundExhaustedError struct {
	cause         error
	retryAfter    time.Duration
	hasRetryAfter bool
	retryNow      bool
}

func (e *homeRetryRoundExhaustedError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *homeRetryRoundExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *homeRetryRoundExhaustedError) RetryAfter() *time.Duration {
	if e == nil || !e.hasRetryAfter {
		return nil
	}
	value := e.retryAfter
	return &value
}

func markHomeRetryRoundExhausted(err error, retryAfter *time.Duration, retryNow bool) error {
	if err == nil {
		return nil
	}
	marked := &homeRetryRoundExhaustedError{cause: err, retryNow: retryNow}
	if retryAfter != nil {
		marked.retryAfter = *retryAfter
		marked.hasRetryAfter = true
	}
	return marked
}

func isHomeRetryRoundExhausted(err error) bool {
	if err == nil {
		return false
	}
	var marker *homeRetryRoundExhaustedError
	return errors.As(err, &marker) && marker != nil
}

type homeRetryRoundTiming struct {
	retryAfter time.Duration
	immediate  bool
	invalid    bool
}

func (t *homeRetryRoundTiming) Observe(err error) {
	if t == nil || err == nil || t.immediate || t.invalid {
		return
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil {
		return
	}
	if *retryAfter == 0 {
		t.retryAfter = 0
		t.immediate = true
		return
	}
	if *retryAfter < 0 {
		t.retryAfter = *retryAfter
		t.invalid = true
		return
	}
	if t.retryAfter <= 0 || *retryAfter < t.retryAfter {
		t.retryAfter = *retryAfter
	}
}

func (t *homeRetryRoundTiming) RetryAfter() *time.Duration {
	if t == nil || t.immediate || (!t.invalid && t.retryAfter <= 0) {
		return nil
	}
	value := t.retryAfter
	return &value
}

func (e *homeDispatchRetryAfterError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *homeDispatchRetryAfterError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *homeDispatchRetryAfterError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return e.cause.HTTPStatus
}

func (e *homeDispatchRetryAfterError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	value := e.retryAfter
	return &value
}

func (e *homeDispatchRetryAfterError) RequestRetryLimit() (int, bool) {
	if e == nil || !e.hasRequestRetry {
		return 0, false
	}
	return e.requestRetry, true
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeForceMappingAttributeKey      = "home_force_mapping"
	homeOriginalAliasAttributeKey     = "home_original_alias"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if !homeMode {
		return true
	}
	if isHomeRequestRetryExceededError(errPick) {
		return true
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
	case "auth_not_found", "auth_unavailable":
		return true
	default:
		return false
	}
}

func isHomeNextRoundImmediatelyAvailable(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), "auth_unavailable")
}

func pendingHomeRetryRoundDelay(err error, maxWait time.Duration, retryLimit *int, acceptRemoteRetryLimit bool) (time.Duration, bool) {
	if err == nil || isHomeRetryRoundExhausted(err) {
		return 0, false
	}
	var homeCooldown *homeDispatchRetryAfterError
	if !errors.As(err, &homeCooldown) || homeCooldown == nil {
		return 0, false
	}
	observeHomeCooldownRetryLimit(homeCooldown, retryLimit, acceptRemoteRetryLimit)
	retryAfter := homeCooldown.RetryAfter()
	if retryAfter == nil || *retryAfter <= 0 || maxWait <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func homeAuthAlreadyTried(tried map[string]struct{}, authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(tried) == 0 {
		return false
	}
	_, ok := tried[authID]
	return ok
}

func repeatedHomeAuthError() *Error {
	return &Error{
		Code:       homeRequestRetryExceededErrorCode,
		Message:    "home returned a previously tried auth",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

type homeAuthDispatchResponse struct {
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	AuthIndex     string `json:"auth_index"`
	UserAPIKey    string `json:"user_api_key"`
	RequestRetry  *int   `json:"request_retry,omitempty"`
	ForceMapping  bool   `json:"force_mapping"`
	OriginalAlias string `json:"original_alias"`
	Auth          Auth   `json:"auth"`
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) ([]byte, error)
	AbortAmbiguousDispatch()
}

type homeDispatchConstraintsDispatcher interface {
	RPopAuthWithConstraints(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int, excludedAuthIDs []string, pinnedAuthID string) ([]byte, error)
}

type homeDispatchRetryRoundConstraintsDispatcher interface {
	RPopAuthWithRetryRoundConstraints(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int, retryRound int, excludedAuthIDs []string, pinnedAuthID string) ([]byte, error)
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("userApiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := ctx.Value("gin").(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("userApiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func (m *Manager) endHomeSelectionBeforeRedispatch(ctx context.Context, selection *HomeDispatchSelection, reason string) error {
	if selection == nil {
		return nil
	}
	ticket := selection.EndWithRelease(reason)
	if ticket == nil {
		return nil
	}

	bound := internalconfig.CredentialConcurrencyConfig{}.WithDefaults().CPACancelBound
	if m != nil {
		if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil {
			bound = cfg.CredentialConcurrency.WithDefaults().CPACancelBound
		}
	}
	waitCtx := ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	waitCtx, cancelWait := context.WithTimeout(waitCtx, bound)
	defer cancelWait()
	if errWait := ticket.Wait(waitCtx); errWait != nil {
		return &Error{Code: "home_unavailable", Message: "Home did not acknowledge credential release: " + errWait.Error(), Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}
	return nil
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.homeSessionAliases.clear()
}

func (m *Manager) replaceHomeSelectionAuth(selection *HomeDispatchSelection, auth *Auth) {
	if m == nil || selection == nil || auth == nil {
		return
	}
	m.mu.Lock()
	selection.ReplaceAuth(auth)
	m.mu.Unlock()
}

func (m *Manager) pickNextViaHome(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	selection, errSelection := m.pickHomeDispatchSelection(ctx, model, withHomeExcludedAuthIDs(opts, tried))
	if errSelection != nil {
		return nil, nil, "", errSelection
	}
	selectionAuth := selection.CloneAuth()
	if selectionAuth == nil || homeAuthAlreadyTried(tried, selectionAuth.ID) {
		selection.End("repeated_auth")
		return nil, nil, "", repeatedHomeAuthError()
	}
	auth := selection.CloneAuthForRoute(model)
	executor := selection.Executor
	provider := selection.Provider
	selection.End("legacy_selection_unbound")
	return auth, executor, provider, nil
}

func (m *Manager) pickHomeDispatchSelection(ctx context.Context, model string, opts cliproxyexecutor.Options) (*HomeDispatchSelection, error) {
	if m == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	requestedModel := strings.TrimSpace(model)
	if requestedModel == "" {
		requestedModel = requestedModelFromMetadata(opts.Metadata, model)
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	retryRound := homeRetryRoundFromMetadata(opts.Metadata)
	excludedAuthIDList := homeExcludedAuthIDsFromMetadata(opts.Metadata)
	excludedAuthIDs := make(map[string]struct{}, len(excludedAuthIDList))
	for _, authID := range excludedAuthIDList {
		excludedAuthIDs[authID] = struct{}{}
	}
	bundle := m.HomeDispatchBundle()
	if bundle == nil || bundle.client == nil || bundle.registry == nil {
		return nil, &Error{Code: "home_unavailable", Message: "home dispatch bundle unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}
	client := bundle.client
	registry := bundle.registry
	if !client.HeartbeatOK() {
		return nil, &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}
	if pinnedAuthID != "" {
		if _, excluded := excludedAuthIDs[pinnedAuthID]; excluded {
			return nil, &Error{Code: "auth_not_found", Message: "pinned auth is unavailable in the current retry round", HTTPStatus: http.StatusServiceUnavailable}
		}
	}
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		return nil, &Error{Code: "home_unavailable", Message: "home execution registry unavailable", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}

	sessionID := m.homeDispatchSessionID(opts)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)
	var raw []byte
	var errRPop error
	if retryRoundClient, okRetryRound := client.(homeDispatchRetryRoundConstraintsDispatcher); okRetryRound {
		raw, errRPop = retryRoundClient.RPopAuthWithRetryRoundConstraints(ctx, requestedModel, sessionID, dispatchHeaders, homeAuthCountFromMetadata(opts.Metadata), retryRound, excludedAuthIDList, pinnedAuthID)
	} else if constrainedClient, okConstraints := client.(homeDispatchConstraintsDispatcher); okConstraints {
		raw, errRPop = constrainedClient.RPopAuthWithConstraints(ctx, requestedModel, sessionID, dispatchHeaders, homeAuthCountFromMetadata(opts.Metadata), excludedAuthIDList, pinnedAuthID)
	} else {
		raw, errRPop = client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, homeAuthCountFromMetadata(opts.Metadata))
	}
	if errRPop != nil {
		if home.IsAmbiguousDispatchError(errRPop) {
			client.AbortAmbiguousDispatch()
		}
		pending.End()
		if errors.Is(errRPop, home.ErrAuthNotFound) {
			return nil, &Error{Code: "auth_not_found", Message: errRPop.Error(), HTTPStatus: http.StatusServiceUnavailable}
		}
		return nil, &Error{Code: "home_unavailable", Message: errRPop.Error(), Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}

	envelope, errEnvelope := decodeHomeDispatchConcurrencyEnvelope(raw)
	if errEnvelope != nil {
		if envelope.Present {
			client.AbortAmbiguousDispatch()
		}
		pending.End()
		if envelope.Present {
			return nil, invalidHomeConcurrencyResponse("Home returned malformed concurrency tuple")
		}
		return nil, &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}

	kind := "http"
	if opts.Stream {
		kind = "stream"
	}
	baseScope := executionregistry.ScopeSpec{
		RequestID: logging.GetRequestID(ctx),
		Model:     requestedModel,
		Kind:      kind,
		StartedAt: time.Now(),
	}
	var scope *executionregistry.Scope
	if envelope.Present {
		var errInstall error
		scope, errInstall = installHomeConcurrencyScope(registry, pending, envelope.Tuple, baseScope)
		if errInstall != nil {
			client.AbortAmbiguousDispatch()
			pending.End()
			return nil, homeConcurrencyInstallError(errInstall)
		}
	}
	endScope := func() {
		if scope != nil {
			scope.End("local_validation_failed")
			return
		}
		pending.End()
	}
	if errHome := decodeHomeDispatchError(raw); errHome != nil {
		if envelope.Present {
			client.AbortAmbiguousDispatch()
			endScope()
			return nil, invalidHomeConcurrencyResponse("Home returned both accounted concurrency and an error")
		}
		pending.End()
		return nil, errHome
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		endScope()
		return nil, &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		// Backward compatibility: older Home instances returned the auth directly.
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			endScope()
			return nil, &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
		}
	}
	observedModel := canonicalHomeDispatchModel(dispatch.Model, requestedModel)
	if envelope.Present {
		observedConcurrencyModel, validModel := validCanonicalHomeConcurrencyModelKey(observedModel)
		if !validModel || envelope.Tuple.Model != observedConcurrencyModel {
			client.AbortAmbiguousDispatch()
			endScope()
			return nil, invalidHomeConcurrencyResponse("Home concurrency model does not match dispatched model")
		}
	}
	if !envelope.Present {
		baseScope.Model = observedModel
	}

	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 3)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if originalAlias := strings.TrimSpace(dispatch.OriginalAlias); dispatch.ForceMapping && originalAlias != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 2)
		}
		auth.Attributes[homeForceMappingAttributeKey] = "true"
		auth.Attributes[homeOriginalAliasAttributeKey] = originalAlias
	}
	if strings.TrimSpace(auth.ID) == "" {
		endScope()
		return nil, &Error{Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway}
	}
	if pinnedAuthID != "" && strings.TrimSpace(auth.ID) != pinnedAuthID {
		endScope()
		return nil, &Error{Code: "auth_not_found", Message: "home returned an auth that does not match the pinned credential", HTTPStatus: http.StatusServiceUnavailable}
	}
	if errIdentity := verifyAccountedHomeConcurrencyIdentity(envelope.Tuple, &auth, dispatch.AuthIndex); errIdentity != nil {
		endScope()
		return nil, errIdentity
	}
	logicalProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
	executorKey := executorKeyFromAuth(&auth)
	if logicalProvider == "" || executorKey == "" {
		endScope()
		return nil, &Error{Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway}
	}

	homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex)
	if homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}

	executor, okExecutor := m.Executor(executorKey)
	if !okExecutor {
		endScope()
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway}
	}
	if scope == nil {
		var errInstall error
		scope, errInstall = installHomeConcurrencyScope(registry, pending, homeConcurrencyTuple{}, executionregistry.ScopeSpec{
			RequestID:    baseScope.RequestID,
			CredentialID: strings.TrimSpace(auth.ID),
			Model:        baseScope.Model,
			Kind:         baseScope.Kind,
			StartedAt:    baseScope.StartedAt,
		})
		if errInstall != nil {
			client.AbortAmbiguousDispatch()
			pending.End()
			return nil, homeConcurrencyInstallError(errInstall)
		}
	}

	selection, errSelection := newHomeDispatchSelection(auth.Clone(), executor, logicalProvider, scope)
	if errSelection != nil {
		endScope()
		return nil, &Error{Code: "home_unavailable", Message: "home execution registry unavailable", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}
	if pinnedAuthID == "" && dispatch.RequestRetry != nil && *dispatch.RequestRetry >= 0 {
		selection.requestRetry = *dispatch.RequestRetry
		selection.hasRequestRetry = true
	}
	if envelope.Present {
		selection.accountedModel = envelope.Tuple.Model
	}
	return selection, nil
}

func homeRetryRoundFromMetadata(metadata map[string]any) int {
	if metadata == nil {
		return 0
	}
	switch value := metadata[homeRetryRoundMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 && value == float64(int(value)) {
			return int(value)
		}
	}
	return 0
}

func requestedModelFromMetadata(metadata map[string]any, fallback string) string {
	if metadata != nil {
		if v, ok := metadata[cliproxyexecutor.RequestedModelMetadataKey]; ok {
			switch typed := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			case []byte:
				if trimmed := strings.TrimSpace(string(typed)); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "unknown"
	}
	return fallback
}
