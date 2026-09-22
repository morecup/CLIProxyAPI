package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeExecutor owns account-scoped Desktop protocol and lifecycle state.
// Anthropic-compatible upstreams use a separate provider constructor.
type ClaudeExecutor struct {
	cfg                      *config.Config
	providerID               string
	desktopOnly              bool
	desktopProfile           *claudeprofile.Bundle
	desktopProfileErr        error
	desktopTransports        *helps.ClaudeDesktopTransportRegistry
	desktopATIS              *claudeDesktopATISManager
	desktopExecutionSessions *helps.ClaudeDesktopExecutionSessions
	desktopControlPlane      *claudecontrol.Manager
	desktopStartup           *claudestartup.Manager
	desktopTelemetry         *claudetelemetry.Manager
	desktopLineage           claudeDesktopLineageStore
	desktopPrompts           claudeprompt.Tracker
	desktopContexts          *helps.ClaudeDesktopContextStore
	desktopDurableStatePath  string
	desktopCalibrationMu     sync.Mutex
	desktopCalibrated        map[string]struct{}
	requestLogProvider       string
	upstreamModelNormalizer  func(string) string
}

type claudeDesktopCancellationError struct {
	cause error
}

func (e *claudeDesktopCancellationError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeDesktopCancellationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *claudeDesktopCancellationError) IsRequestScoped() bool {
	return e != nil
}

func newClaudeDesktopCancellationError(ctx context.Context, enabled bool, err error) error {
	if !enabled {
		return nil
	}
	cause := err
	if ctx != nil && ctx.Err() != nil {
		cause = ctx.Err()
	}
	if !errors.Is(cause, context.Canceled) {
		return nil
	}
	return &claudeDesktopCancellationError{cause: cause}
}

func claudeDesktopRequestContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cancelled := newClaudeDesktopCancellationError(ctx, true, ctx.Err()); cancelled != nil {
		return cancelled
	}
	return ctx.Err()
}

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return sigcompat.SignatureProviderFromModelName(baseModel) == sigcompat.SignatureProviderClaude
}

func sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx context.Context, body []byte, baseModel string, preserveEmptyThinkingBlocks ...bool) []byte {
	sanitized := body
	preserveEmpty := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || preserveEmpty {
		var report sigcompat.SignatureSanitizeReport
		sanitized, report = sigcompat.SanitizeClaudeMessagesForClaudeUpstream(body, baseModel, preserveEmptyThinkingBlocks...)
		logClaudeSignatureSanitizeReport(ctx, baseModel, report)
	}
	return sanitizeClaudeWebSearchDomains(sanitized)
}

// sanitizeClaudeWebSearchDomains removes empty allowed_domains/blocked_domains
// arrays from built-in web_search tools. Some clients (e.g. litellm) emit an
// empty array instead of omitting the field, and Anthropic rejects it with
// "Empty list of domains is ambiguous. Provide at least one domain or null.".
// Deleting the key is equivalent to leaving it unset.
func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if updated, errDelete := sjson.DeleteBytes(body, path); errDelete == nil {
					body = updated
				}
			}
		}
		return true
	})
	return body
}

func logClaudeSignatureSanitizeReport(ctx context.Context, baseModel string, report sigcompat.SignatureSanitizeReport) {
	if report.DroppedBlocks == 0 && report.DroppedSignatures == 0 && report.ReplacedSignatures == 0 {
		return
	}

	fields := log.Fields{
		"component":           "signature_sanitizer",
		"executor":            "claude",
		"action":              "sanitize_claude_messages",
		"target_provider":     string(report.TargetProvider),
		"target_model":        baseModel,
		"preserved":           report.Preserved,
		"dropped_blocks":      report.DroppedBlocks,
		"dropped_signatures":  report.DroppedSignatures,
		"replaced_signatures": report.ReplacedSignatures,
	}
	if len(report.Decisions) > 0 {
		decision := report.Decisions[0]
		fields["first_block_kind"] = string(decision.BlockKind)
		fields["first_detected_provider"] = string(decision.DetectedProvider)
		fields["first_reason"] = decision.Reason
	}

	helps.LogWithRequestID(ctx).WithFields(fields).Debug("claude executor: sanitized signature history before upstream")
}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor {
	return newClaudeExecutorWithRuntime(cfg, claudeDesktopRuntimeOptions{})
}

type claudeDesktopRuntimeOptions struct {
	executionSessions            *helps.ClaudeDesktopExecutionSessions
	statePath                    string
	durableStatePath             string
	appSessionID                 string
	machineProfileID             string
	hostSnapshot                 claudetelemetry.HostSnapshotProvider
	sdkProcessSnapshot           claudetelemetry.SDKProcessSnapshotProvider
	enableATIS                   bool
	enableControlPlane           bool
	enableStartup                bool
	startupDoerFactory           claudestartup.DoerFactory
	telemetryEndpointDoerFactory claudetelemetry.EndpointDoerFactory
	controlCredentials           claudecontrol.CredentialSource
}

func newClaudeExecutorWithRuntime(cfg *config.Config, runtimeOptions claudeDesktopRuntimeOptions) *ClaudeExecutor {
	if runtimeOptions.executionSessions == nil {
		runtimeOptions.executionSessions = &helps.ClaudeDesktopExecutionSessions{}
	}
	bundlePath := ""
	statePath := ""
	globalProxyURL := ""
	if cfg != nil {
		bundlePath = cfg.ClaudeDesktop.BundlePath
		statePath = claudetelemetry.StatePath(cfg.ClaudeDesktop.StatePath, cfg.AuthDir)
		globalProxyURL = cfg.ProxyURL
	}
	if strings.TrimSpace(runtimeOptions.statePath) != "" {
		statePath = strings.TrimSpace(runtimeOptions.statePath)
	}
	durableStatePath := statePath
	if strings.TrimSpace(runtimeOptions.durableStatePath) != "" {
		durableStatePath = strings.TrimSpace(runtimeOptions.durableStatePath)
	}
	bundle, errProfile := claudeprofile.Load(bundlePath)
	desktopTransports := helps.NewClaudeDesktopTransportRegistry()
	codeVersion := ""
	if bundle != nil {
		codeVersion = bundle.CodeVersion
	}
	var desktopATIS *claudeDesktopATISManager
	if runtimeOptions.enableATIS {
		desktopATIS = newClaudeDesktopATISManager(statePath, codeVersion, func(ctx context.Context, auth *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
			return desktopTransports.EndpointClient(ctx, cfg, auth, bundle, claudeDesktopATISEndpointRole)
		}, helps.NewClaudeDesktopFeatureStore(statePath, globalProxyURL), helps.NewClaudeDesktopSessionAliasStore(durableStatePath, globalProxyURL), helps.NewClaudeDesktopSessionRecordStore(durableStatePath, globalProxyURL))
		if bundle != nil {
			desktopATIS.client = claudeDesktopATISClientFromProfile(bundle)
			desktopATIS.profileID = bundle.ProfileID
		}
	}
	var desktopControlPlane *claudecontrol.Manager
	if runtimeOptions.enableControlPlane {
		desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
			StatePath:   statePath,
			Bundle:      bundle,
			Credentials: runtimeOptions.controlCredentials,
			DoerFactory: func(ctx context.Context, endpointRole string, auth *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
				return desktopTransports.EndpointClient(ctx, cfg, auth, bundle, endpointRole)
			},
		})
	}
	telemetryEndpointDoerFactory := runtimeOptions.telemetryEndpointDoerFactory
	if telemetryEndpointDoerFactory == nil {
		telemetryEndpointDoerFactory = func(_ string, endpointRole string, auth *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
			if auth == nil || bundle == nil || !bundle.IsTelemetryEndpointRole(endpointRole) {
				return nil
			}
			client, errClient := desktopTransports.EndpointClient(context.Background(), cfg, auth, bundle, endpointRole)
			if errClient != nil {
				return claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
					return nil, errClient
				})
			}
			return client
		}
	}
	telemetryManager := claudetelemetry.NewManager(claudetelemetry.Options{
		StatePath:            statePath,
		Bundle:               bundle,
		ApplicationSessionID: runtimeOptions.appSessionID,
		GlobalProxyURL:       globalProxyURL,
		DoerFactory: func(effectiveProxyURL string) claudetelemetry.HTTPDoer {
			return helps.NewUtlsHTTPClientWithProxyURL(context.Background(), effectiveProxyURL, 0)
		},
		EndpointDoerFactory:        telemetryEndpointDoerFactory,
		HostSnapshotProvider:       runtimeOptions.hostSnapshot,
		SDKProcessSnapshotProvider: runtimeOptions.sdkProcessSnapshot,
		MachineProfileID:           runtimeOptions.machineProfileID,
	})
	if desktopATIS != nil {
		desktopATIS.featureSink = telemetryManager.ObserveSDKFeatureExposure
		desktopATIS.featureStateObserver = telemetryManager.ObserveSDKFeatureState
		desktopATIS.featureHostObserverFactory = telemetryManager.SDKFeatureHostObserver
	}
	var desktopStartup *claudestartup.Manager
	if runtimeOptions.enableStartup {
		startupDoerFactory := runtimeOptions.startupDoerFactory
		if startupDoerFactory == nil {
			startupDoerFactory = func(ctx context.Context, endpointRole string, auth *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
				return desktopTransports.EndpointClient(ctx, cfg, auth, bundle, endpointRole)
			}
		}
		desktopStartup = claudestartup.NewManager(claudestartup.Options{
			StatePath:                  statePath,
			Bundle:                     bundle,
			ApplicationSessionID:       runtimeOptions.appSessionID,
			HostSnapshotProvider:       runtimeOptions.hostSnapshot,
			DoerFactory:                startupDoerFactory,
			UpdateCheckObserver:        telemetryManager.ObserveUpdateCheck,
			SessionsWatchRetryObserver: telemetryManager.ObserveSessionsWatchRetry,
			SDKFeaturesObserverFactory: desktopATIS.PrepareSDKFeatures,
			SDKFeatureRefreshCadence:   desktopATIS.FeatureRefreshCadence,
			SDKFeatureAuthedEvaluation: desktopATIS.FeatureAuthedEvaluation,
			SDKFeatureContext:          desktopATIS.FeatureContext(),
			SDKFeatureFailureObserver: func(auth *cliproxyauth.Auth) error {
				return desktopATIS.observeFeatureHostHealth(auth, desktopATIS.featureHosts.Warm(), false)
			},
		})
	}
	if desktopATIS != nil && desktopStartup != nil {
		desktopATIS.startFeatureHost = func(host *claudefeatures.Host) error {
			return desktopStartup.AddSDKFeatureHost(&claudestartup.SDKFeatureHost{
				Context: host.Context(),
				Prepare: func(auth *cliproxyauth.Auth) (string, func([]byte) error, error) {
					return desktopATIS.prepareSDKFeatureHost(auth, host)
				},
				Cadence: func(auth *cliproxyauth.Auth) (time.Duration, bool) {
					return desktopATIS.featureRefreshCadenceForHost(auth, host)
				},
				Authed: func(auth *cliproxyauth.Auth) bool {
					return desktopATIS.featureAuthedEvaluationForHost(auth, host)
				},
				Failure: func(auth *cliproxyauth.Auth) error {
					return desktopATIS.observeFeatureHostHealth(auth, host, false)
				},
			})
		}
	}
	nativeContentOptions := claudeprompt.SDKNativeContentOptions{
		Store:                 helps.NewClaudeDesktopNativeContentStore(durableStatePath, globalProxyURL),
		TranscriptStore:       helps.NewClaudeDesktopTranscriptStore(durableStatePath, globalProxyURL),
		RemoteTranscriptStore: helps.NewClaudeDesktopRemoteTranscriptStore(durableStatePath, globalProxyURL),
	}
	if bundle != nil {
		nativeContentOptions.Version, nativeContentOptions.Entrypoint, nativeContentOptions.Cwd = bundle.CodeVersion, "claude-desktop", bundle.Environment.DefaultWorkingDir
	}
	return &ClaudeExecutor{
		cfg:                      cfg,
		providerID:               "claude",
		desktopOnly:              true,
		desktopProfile:           bundle,
		desktopProfileErr:        errProfile,
		desktopTransports:        desktopTransports,
		desktopATIS:              desktopATIS,
		desktopExecutionSessions: runtimeOptions.executionSessions,
		desktopControlPlane:      desktopControlPlane,
		desktopStartup:           desktopStartup,
		desktopTelemetry:         telemetryManager,
		desktopContexts:          helps.NewClaudeDesktopContextStore(durableStatePath, globalProxyURL),
		desktopDurableStatePath:  durableStatePath,
		desktopPrompts:           claudeprompt.NewTracker(helps.NewClaudeDesktopSDKSessionStore(durableStatePath, globalProxyURL), nativeContentOptions),
	}
}

// Activate starts the account-local application lifetime without fabricating
// a user session. Pending durable telemetry is rebound to the current token
// and may resume immediately after enable or crash recovery.
func (e *ClaudeExecutor) Activate(auth *cliproxyauth.Auth) error {
	if e == nil || e.desktopTelemetry == nil {
		return nil
	}
	if errTelemetry := e.desktopTelemetry.Activate(auth); errTelemetry != nil {
		return errTelemetry
	}
	if e.desktopStartup != nil {
		e.desktopStartup.Activate(auth)
	}
	e.recordDesktopTranscriptLeasePass(auth)
	return nil
}

func (e *ClaudeExecutor) recordDesktopTranscriptLeasePass(auth *cliproxyauth.Auth) {
	if e == nil || auth == nil || e.desktopTelemetry == nil || e.desktopATIS == nil || e.desktopProfile == nil {
		return
	}
	accountScope := helps.ClaudeDesktopPromptAccountScope(auth, e.desktopProfile.ProfileID)
	pass := e.desktopPrompts.TranscriptLeasePass(accountScope, nil)
	values, err := e.desktopATIS.desktopRecords.List(e.claudeDesktopRecordOwner(auth))
	if err == nil {
		sessionIDs := make([]string, 0, len(values))
		for _, value := range values {
			sessionIDs = append(sessionIDs, value.SDKSessionID)
		}
		pass = e.desktopPrompts.TranscriptLeasePass(accountScope, sessionIDs)
	} else if pass.Skipped == "" {
		pass.Errors++
	}
	if errTelemetry := e.desktopTelemetry.RecordDesktopTranscriptLeasePass(auth, claudetelemetry.DesktopTranscriptLeasePass{
		Candidates: pass.Candidates, Renewed: pass.Renewed, Fresh: pass.Fresh,
		Missing: pass.Missing, Errors: pass.Errors, Skipped: pass.Skipped,
		RetentionDays: pass.RetentionDays, RetentionSource: pass.RetentionSource,
	}); errTelemetry != nil {
		log.WithError(errTelemetry).Warn("claude desktop: transcript lease telemetry could not be persisted")
	}
}

// CloseExecutionSession also supports SDK users registering the account-local
// executor directly. The service normally registers ClaudeAccountExecutor.
func (e *ClaudeExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || !e.desktopOnly {
		return
	}
	if strings.TrimSpace(sessionID) == cliproxyauth.CloseAllExecutionSessionsID {
		e.Close()
		return
	}
	e.desktopExecutionSessions.Close(sessionID)
}

// Close releases telemetry workers and all account-scoped transport pools.
func (e *ClaudeExecutor) Close() {
	if e == nil {
		return
	}
	e.desktopControlPlane.PrepareClose()
	if e.desktopStartup != nil {
		e.desktopStartup.Close()
	}
	e.desktopATIS.Close()
	if e.desktopControlPlane != nil {
		e.desktopControlPlane.Close()
	}
	// Final bridge metadata is produced during control cleanup. Keep the
	// transcript queue open until those exact query producers have retired.
	if err := e.desktopPrompts.Close(); err != nil {
		log.Warn("claude desktop: transcript writer closed with unavailable SDK state")
	}
	// Worker teardown produces SDK events after query cancellation. Join it
	// before stopping the account's durable telemetry delivery workers.
	if e.desktopTelemetry != nil {
		e.desktopTelemetry.Close()
	}
	if e.desktopTransports != nil {
		e.desktopTransports.CloseAll()
	}
}

// Quarantine freezes durable telemetry and tears down network resources
// without emitting the normal Desktop application shutdown sequence.
func (e *ClaudeExecutor) Quarantine() {
	if e == nil {
		return
	}
	e.prepareQuarantine()
	if e.desktopTelemetry != nil {
		e.desktopTelemetry.Quarantine()
	}
	e.desktopControlPlane.Quarantine()
	if e.desktopStartup != nil {
		e.desktopStartup.Close()
	}
	e.desktopATIS.Close()
	if err := e.desktopPrompts.Close(); err != nil {
		log.Warn("claude desktop: transcript writer quarantined with unavailable SDK state")
	}
	if e.desktopTransports != nil {
		e.desktopTransports.CloseAll()
	}
}

func (e *ClaudeExecutor) prepareQuarantine() {
	if e == nil {
		return
	}
	// These signals do not join workers. A blocked telemetry delivery must not
	// delay canceling an archive, and a blocked archive must not delay freezing
	// the account's durable telemetry queue.
	e.desktopTelemetry.PrepareQuarantine()
	e.desktopControlPlane.PrepareQuarantine()
}

func (e *ClaudeExecutor) StartupStatus() claudestartup.Status {
	if e == nil || e.desktopStartup == nil {
		return claudestartup.Status{State: "stopped"}
	}
	return e.desktopStartup.Status()
}

func (e *ClaudeExecutor) Identifier() string {
	if e != nil && strings.TrimSpace(e.providerID) != "" {
		return e.providerID
	}
	return "claude"
}

func (e *ClaudeExecutor) thinkingProvider() string { return "claude" }

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

func (e *ClaudeExecutor) upstreamModel(baseModel string) string {
	if e.upstreamModelNormalizer != nil {
		return e.upstreamModelNormalizer(baseModel)
	}
	return baseModel
}

func (e *ClaudeExecutor) restoreResponseModel(payload []byte, model string) []byte {
	if e.upstreamModelNormalizer == nil || strings.TrimSpace(model) == "" {
		return payload
	}
	return restoreClaudeResponseModel(payload, model)
}

func restoreClaudeResponseModel(payload []byte, model string) []byte {
	if updated, changed := setClaudeResponseModel(payload, model); changed {
		return updated
	}

	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return payload
	}
	dataIndex := bytes.Index(payload, []byte("data:"))
	if dataIndex < 0 {
		return payload
	}
	rawJSON := bytes.TrimSpace(payload[dataIndex+len("data:"):])
	updated, changed := setClaudeResponseModel(rawJSON, model)
	if !changed {
		return payload
	}
	rebuilt := make([]byte, 0, dataIndex+len("data: ")+len(updated))
	rebuilt = append(rebuilt, payload[:dataIndex]...)
	rebuilt = append(rebuilt, []byte("data: ")...)
	rebuilt = append(rebuilt, updated...)
	return rebuilt
}

func setClaudeResponseModel(payload []byte, model string) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	changed := false
	for _, path := range []string{"model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, errSet := sjson.SetBytes(updated, path, model)
		if errSet != nil {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if errEligibility := e.validateClaudeDesktopAuth(auth); errEligibility != nil {
		return errEligibility
	}
	apiKey, _ := claudeCreds(auth)
	useAPIKey := auth != nil && (auth.AuthKind() == cliproxyauth.AuthKindAPIKey || (auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""))
	isAnthropicBase := isAnthropicUpstreamURL(req.URL)
	if strings.TrimSpace(apiKey) != "" {
		if isAnthropicBase && useAPIKey {
			req.Header.Del("Authorization")
			req.Header.Set("x-api-key", apiKey)
		} else {
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if errEligibility := e.validateClaudeDesktopAuth(auth); errEligibility != nil {
		return nil, errEligibility
	}
	if e.desktopOnly {
		return e.httpRequestClaudeDesktop(ctx, auth, req)
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
