package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

var _ cliproxyexecutor.ClaudeDesktopRemoteController = (*ClaudeAccountExecutor)(nil)
var _ cliproxyexecutor.ClaudeDesktopResumeController = (*ClaudeAccountExecutor)(nil)

func (e *ClaudeAccountExecutor) ResumeDesktopSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionResume) (cliproxyexecutor.ClaudeDesktopSession, error) {
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	started := time.Now()
	var history *claudeprompt.SDKRemoteWireResume
	var resolveDuration, scanDuration time.Duration
	grant, value, err := inner.desktopATIS.desktopRecords.ResumeSession(ctx, runtime.recordOwner, operation.SessionID, operation.ExpectedGeneration,
		func(saved claudesessions.ResumeRecord) error {
			resolveStarted := time.Now()
			_, err := inner.resolveDesktopRemoteModel(saved.Model)
			resolveDuration = time.Since(resolveStarted)
			if err != nil {
				return err
			}
			scanStarted := time.Now()
			history, err = inner.desktopPrompts.PrepareRemoteWireResume(helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID), saved.SDKSessionID)
			scanDuration = time.Since(scanStarted)
			return err
		}, func(scope, session string) (*claudefeatures.Host, error) {
			host, err := inner.desktopATIS.featureHosts.Main(scope, session)
			if err == nil {
				err = inner.desktopATIS.startSDKFeatureHost(host)
			}
			return host, err
		})
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession(value), err
	}
	if grant == nil {
		inner.recordDesktopResumeLifecycle(ctx, auth, value, started, resolveDuration, scanDuration)
		return cliproxyexecutor.ClaudeDesktopSession(value), nil
	}
	resumed, err := e.attachDesktopRemote(ctx, runtime, auth, grant, history)
	if err == nil {
		inner.recordDesktopResumeLifecycle(ctx, auth, value, started, resolveDuration, scanDuration)
	}
	return resumed, err
}

func (e *ClaudeAccountExecutor) desktopRemoteAuth(authID string) (*cliproxyauth.Auth, error) {
	if e == nil || e.credentialManager == nil {
		return nil, claudesessions.ErrUnavailable
	}
	auth, ok := e.credentialManager.GetByID(authID)
	if !ok {
		return nil, claudesessions.ErrNotFound
	}
	if err := e.CanScheduleAuth(auth); err != nil {
		return nil, err
	}
	if _, err := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata); err != nil {
		return nil, err
	}
	return auth, nil
}

func (e *ClaudeAccountExecutor) StartDesktopRemoteSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopRemoteStart) (cliproxyexecutor.ClaudeDesktopSession, error) {
	if !filepath.IsAbs(operation.Folder) {
		return cliproxyexecutor.ClaudeDesktopSession{}, claudesessions.ErrInvalid
	}
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	model, err := inner.resolveDesktopRemoteModel(operation.Model)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	grant, value, err := inner.desktopATIS.desktopRecords.CreateRemote(ctx, runtime.recordOwner, operation.RemoteSessionID, operation.Folder, model,
		func(scope string) (*claudefeatures.Host, error) {
			host, errStart := inner.desktopATIS.featureHosts.Main(scope, "")
			if errStart == nil {
				errStart = inner.desktopATIS.startSDKFeatureHost(host)
			}
			return host, errStart
		})
	if err != nil {
		// A live grant with failed persistence must not start upstream work.
		if grant != nil {
			_, host, _, _, _, _ := grant.Read()
			if host != nil {
				inner.desktopATIS.featureHosts.Retire(host)
			}
		}
		value.Running = false
		return cliproxyexecutor.ClaudeDesktopSession(value), err
	}
	return e.attachDesktopRemote(ctx, runtime, auth, grant)
}

func (e *ClaudeAccountExecutor) AttachDesktopRemoteSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionStop) (cliproxyexecutor.ClaudeDesktopSession, error) {
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	defer runtime.release()
	grant, err := runtime.executor.desktopATIS.desktopRecords.Remote(runtime.recordOwner, operation.SessionID, operation.ExpectedQueryID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	return e.attachDesktopRemote(ctx, runtime, auth, grant)
}

func (e *ClaudeExecutor) resolveDesktopRemoteModel(model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", claudesessions.ErrInvalid
	}
	body, _ := json.Marshal(map[string]any{"model": model})
	_, err := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, model, nil)
	return model, err
}

func (e *ClaudeAccountExecutor) attachDesktopRemote(ctx context.Context, runtime *claudeAccountRuntime, auth *cliproxyauth.Auth, grant *claudesessions.RemoteGrant, resumed ...*claudeprompt.SDKRemoteWireResume) (cliproxyexecutor.ClaudeDesktopSession, error) {
	value, host, remoteID, _, _, err := grant.Read()
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopSession{}, err
	}
	inner := runtime.executor
	runtime.mu.Lock()
	if runtime.retiring || runtime.closed {
		runtime.mu.Unlock()
		return cliproxyexecutor.ClaudeDesktopSession(value), claudesessions.ErrUnavailable
	}
	input := runtime.remoteInputs[host.ID()]
	if input == nil {
		model, defaultModel, system, errConfiguration := grant.Configuration()
		if errConfiguration != nil {
			runtime.mu.Unlock()
			return cliproxyexecutor.ClaudeDesktopSession(value), errConfiguration
		}
		// Keep injected transport ownership for offline tests, but never carry
		// the management Gin request (and its headers) into model requests.
		base := context.Background()
		if transport := ctx.Value("cliproxy.roundtripper"); transport != nil {
			base = context.WithValue(base, "cliproxy.roundtripper", transport)
		}
		base, release := helps.BindClaudeDesktopQueryLifetime(base, host.Context())
		var history *claudeprompt.SDKRemoteWireResume
		if len(resumed) == 1 {
			history = resumed[0]
		}
		required, errRequired := grant.RequiresSavedHistory()
		if errRequired == nil && required && history == nil {
			history, errRequired = inner.desktopPrompts.PrepareRemoteWireResume(helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID), value.SDKSessionID)
		}
		if errRequired != nil {
			release()
			runtime.mu.Unlock()
			inner.desktopATIS.featureHosts.Retire(host)
			return cliproxyexecutor.ClaudeDesktopSession(inner.desktopATIS.desktopRecords.ByHost(host)), errRequired
		}
		if inner.desktopTelemetry != nil {
			if errObserver := host.SetShellTelemetryObserver(inner.claudeDesktopShellTelemetryObserver(auth, host)); errObserver != nil {
				helps.LogWithRequestID(ctx).WithError(errObserver).Warn("claude desktop: shell telemetry observer was not installed")
			}
		}
		taskScope := sha256.Sum256([]byte(helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID) + "\x00" + value.SDKSessionID))
		// Child announcements live as long as this query's input actor, like
		// the task runtime whose generations they keep consistent.
		announcements := &helps.ClaudeDesktopDeferredAnnouncements{}
		agentOptions := &claudetasks.Options{
			Scope:         hex.EncodeToString(taskScope[:]),
			DeliveryScope: value.QueryID,
			// The native main agent identity is the SDK session ID; it feeds
			// the "main" candidate ref in SendMessage listings.
			MainAgentID: value.SDKSessionID,
			Store:       helps.NewClaudeDesktopAgentTaskStore(inner.desktopDurableStatePath, auth.ProxyURL),
			OpenTranscript: func(agentID, leaf string) (claudetasks.Transcript, error) {
				if host.Context().Err() != nil {
					return nil, claudetasks.ErrUnavailable
				}
				return inner.desktopPrompts.OpenSidechain(helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID), value.SDKSessionID, agentID, leaf)
			},
			ResolveModel: inner.resolveDesktopAgentModel,
			HandbackProvenance: func() bool {
				value, _ := inner.desktopATIS.featureValueOnHost(auth, host, "tengu_melodic_wolf", json.RawMessage(`false`))
				return claudefeatures.Truthy(value.Raw)
			},
			MaxDepth: func() int {
				v, err := inner.desktopATIS.featureValueOnHost(auth, host, "tengu_hazel_trellis", json.RawMessage(`3`))
				var depth int
				if err == nil && json.Unmarshal(v.Raw, &depth) == nil && depth >= 1 {
					return depth
				}
				return 3
			},
			Definitions: func(model string) claudetasks.DefinitionContext {
				return inner.desktopAgentDefinitionContext(auth, host, model)
			},
			// Native tengu_tool_search_outcome for main turns and child loops;
			// the ToolSearch tool runs between two owned requests, so it is
			// recorded span-less against this query's SDK session.
			ToolSearchOutcome: inner.claudeDesktopToolSearchOutcomeObserver(auth, value.SDKSessionID),
			// Wrapper events plus the BUr fallthrough tengu_feature_bad; the
			// TaskOutput waiting_for_task progress item; the DMt subagent_end
			// cache hint (claude_desktop_tool_lifecycle_telemetry.go).
			ToolExecuted: inner.claudeDesktopToolLifecycleExecutedObserver(auth, value.SDKSessionID),
			ToolProgress: inner.claudeDesktopToolProgressObserver(auth, value.SDKSessionID),
			SubagentEnd:  inner.claudeDesktopSubagentEndObserver(auth, value.SDKSessionID),
			Execute: func(turn context.Context, invocation claudetasks.Invocation) ([]byte, error) {
				rows := make([]json.RawMessage, 0, len(invocation.Messages))
				for _, message := range invocation.Messages {
					row, err := json.Marshal(message)
					if err != nil {
						return nil, err
					}
					rows = append(rows, row)
				}
				// Same request-build order as a main turn, against the child's
				// own rows and model: reference filter, deferred-tools reminder
				// merged into the trailing user row, tools from the merged rows.
				turn, timing := helps.WithClaudeDesktopAttachmentTiming(turn)
				prepared, err := announcements.Prepare(invocation.AgentID,
					helps.ClaudeDesktopOwnedRequestToolsForContext(inner.desktopAgentDefinitionContext(auth, host, invocation.Model)).Timed(timing), rows)
				if err != nil {
					return nil, err
				}
				request := map[string]any{"model": invocation.Model, "stream": true, "messages": prepared.Messages, "tools": prepared.Tools}
				body, err := json.Marshal(request)
				if err != nil {
					return nil, err
				}
				turn = claudetasks.WithInvocation(turn, invocation)
				turn = claudetasks.WithCaller(turn, claudetasks.Caller{AgentID: invocation.AgentID, PromptID: invocation.PromptID, Model: invocation.Model, Depth: invocation.Depth})
				turn = cliproxyexecutor.WithClaudeDesktopParentPromptID(turn, invocation.ParentPromptID)
				return e.executeDesktopRemoteInput(turn, runtime, grant, invocation.Model, body)
			},
			Observe: func(observeCtx context.Context, event claudetasks.Event) error {
				return inner.desktopControlPlane.RecordAgentTask(observeCtx, value.ID, value.QueryID, event)
			},
		}
		input = helps.NewClaudeDesktopRemoteInput(base, helps.ClaudeDesktopRemoteInputConfiguration{
			Model: model, DefaultModel: defaultModel, System: system, Persist: grant.SetConfiguration, Resume: history, Agents: agentOptions},
			func(turn context.Context, model string, body []byte) ([]byte, error) {
				return e.executeDesktopRemoteInput(turn, runtime, grant, model, body)
			},
			func(admission context.Context) error {
				if _, _, _, _, _, err := grant.Read(); err != nil {
					return err
				}
				lease, err := host.BeginInput(admission, func() { inner.desktopATIS.featureHosts.Retire(host) })
				if err != nil {
					return err
				}
				defer lease.Close()
				return lease.Accept()
			}, inner.resolveDesktopRemoteModel,
			func(err error) { inner.desktopControlPlane.RecordInboundOutcome(value.ID, value.QueryID, err) })
		if errBind := grant.BindInputDone(input.Done()); errBind != nil {
			input.Stop()
			release()
			runtime.mu.Unlock()
			return cliproxyexecutor.ClaudeDesktopSession(value), errBind
		}
		if runtime.remoteInputs == nil {
			runtime.remoteInputs = make(map[string]*helps.ClaudeDesktopRemoteInput)
		}
		runtime.remoteInputs[host.ID()] = input
		owned := input
		go func() {
			<-owned.Done()
			release()
			runtime.mu.Lock()
			if runtime.remoteInputs[host.ID()] == owned {
				delete(runtime.remoteInputs, host.ID())
			}
			runtime.mu.Unlock()
		}()
	}
	runtime.mu.Unlock()
	consumer := &claudecontrol.InboundConsumer{User: input.Enqueue, Control: input.Control,
		RestoreWorker: func(restoreCtx context.Context, state *claudecontrol.WorkerRestoration) error {
			if _, _, _, _, _, err := grant.Read(); err != nil {
				return err
			}
			policy, errPolicy := inner.desktopRemoteHydrationPolicy(auth, host)
			if errPolicy != nil {
				state.RecordHydrationFailure(true)
				return errPolicy
			}
			if err := input.RestoreWorker(restoreCtx, state.ExternalMetadata(), state.WorkerEpoch()); err != nil {
				return err
			}
			// Bind after model restoration and before any input is admitted.
			// Telemetry failure remains visible but cannot veto usable history.
			model, _, _, errConfiguration := grant.Configuration()
			if errConfiguration != nil {
				return errConfiguration
			}
			observeChain, _ := inner.desktopTelemetry.SDKTranscriptChainObserver(restoreCtx, auth, value.SDKSessionID, value.QueryID, model,
				func(name string) (json.RawMessage, error) {
					feature, err := inner.desktopATIS.featureValueOnHost(auth, host, name, json.RawMessage(`{}`))
					if err != nil && (errors.Is(err, claudefeatures.ErrStale) || feature.Source == "" || feature.Source == "stale" || host.Context().Err() != nil) {
						return nil, err
					}
					return feature.Raw, nil
				})
			// The adoption success path emits tengu_session_resumed for this
			// session (sdk_resume_bridge.go) through the helps observer.
			restoreCtx = withClaudeDesktopResumeTelemetry(restoreCtx, inner.desktopTelemetry, auth, model)
			status, errHydrate := helps.HydrateClaudeDesktopRemote(restoreCtx, &inner.desktopPrompts,
				helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID), value.SDKSessionID,
				remoteID, state, input, policy, observeChain)
			state.RecordHydrationFailure(status.ReadFailed || status.SubagentReadFailed || errHydrate != nil)
			if errHydrate != nil {
				return errHydrate
			}
			if err := input.Restore(); err != nil {
				inner.desktopATIS.featureHosts.Retire(host)
				return err
			}
			return nil
		},
		Policy: func() (claudecontrol.AttestationPolicy, error) { return inner.desktopRemotePolicy(auth, host) }}
	err = inner.desktopControlPlane.AttachRemoteQuery(ctx, auth, grant, consumer,
		func(id string) error { return inner.desktopATIS.desktopRecords.BindBridge(host, id) },
		inner.desktopBridgeTranscript(auth, value.SDKSessionID))
	var workerConflict *claudecontrol.WorkerEpochConflict
	if err == nil {
		if _, _, _, _, _, err = grant.Read(); err == nil {
			err = input.Start()
		}
	} else if errors.As(err, &workerConflict) || errors.Is(err, claudesessions.ErrRemoteMismatch) || errors.Is(err, claudesessions.ErrRemoteBound) {
		input.Stop()
		inner.desktopATIS.featureHosts.Retire(host)
		err = errors.Join(err, inner.desktopControlPlane.RetireQuery(ctx, value.ID, value.QueryID))
	}
	return cliproxyexecutor.ClaudeDesktopSession(inner.desktopATIS.desktopRecords.ByHost(host)), err
}

func (e *ClaudeExecutor) desktopRemoteHydrationPolicy(auth *cliproxyauth.Auth, host *claudefeatures.Host) (helps.ClaudeDesktopRemoteHydrationPolicy, error) {
	policy := helps.ClaudeDesktopRemoteHydrationPolicy{}
	// Native il()/al() read in this order at the hydration call site. Use the
	// current host's evaluated features and exposure dedup, never the prewarm
	// host, another session, downstream metadata or a process-global override.
	for _, gate := range []struct {
		name  string
		value *bool
	}{
		{"tengu_ccr_delta_rehydrate", &policy.DeltaEnabled},
		{"tengu_ccr_subagent_skip_on_delta", &policy.SkipSubagentsOnDelta},
	} {
		value, err := e.desktopATIS.featureValueOnHost(auth, host, gate.name, json.RawMessage(`false`))
		if err != nil && (errors.Is(err, claudefeatures.ErrStale) || value.Source == "" || value.Source == "stale" || host.Context().Err() != nil) {
			return helps.ClaudeDesktopRemoteHydrationPolicy{}, err
		}
		// Native cached feature reads tolerate unavailable disk state. The
		// feature service already records cache/exposure failures in health;
		// use its owned value or false fallback without vetoing restoration.
		*gate.value = claudefeatures.Truthy(value.Raw)
	}
	return policy, nil
}

func (e *ClaudeExecutor) desktopRemotePolicy(auth *cliproxyauth.Auth, host *claudefeatures.Host) (claudecontrol.AttestationPolicy, error) {
	policy := claudecontrol.AttestationPolicy{AcceptLevel: "VERIFIED"}
	for _, gate := range []string{"tengu_bridge_attestation_enforce", "tengu_sessions_elevated_auth_enforcement"} {
		value, err := e.desktopATIS.featureValueOnHost(auth, host, gate, json.RawMessage(`false`))
		if err != nil {
			return policy, err
		}
		if !claudefeatures.Truthy(value.Raw) {
			return policy, nil
		}
	}
	// The actual organization policy-limits consumer is not implemented yet.
	// Do not fabricate require_trusted_devices from unrelated auth metadata.
	return policy, errors.New("Claude Desktop trusted-device policy limits are unavailable")
}

func (e *ClaudeAccountExecutor) executeDesktopRemoteInput(ctx context.Context, runtime *claudeAccountRuntime, grant *claudesessions.RemoteGrant, model string, body []byte) ([]byte, error) {
	value, host, _, folder, _, err := grant.Read()
	if err != nil {
		return nil, err
	}
	auth, err := e.desktopRemoteAuth(runtime.authID)
	if err != nil {
		return nil, err
	}
	inner := runtime.executor
	if inner.claudeDesktopRecordOwner(auth) != runtime.recordOwner {
		return nil, claudesessions.ErrUnavailable
	}
	enrollment, err := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		return nil, err
	}
	owner := claudeDesktopQueryContext{manager: inner.desktopATIS, host: host, accountID: auth.ID, profileID: inner.desktopProfile.ProfileID,
		egress: auth.ProxyURL, identity: claudeDesktopATISIdentityHash(enrollment), session: host.SessionID(), lifetime: host.Context(), desktopSessionID: value.ID, remoteInput: true}
	if invocation, ok := claudetasks.InvocationFromContext(ctx); ok {
		owner.agentID = invocation.AgentID
	}
	ctx = context.WithValue(ctx, claudeDesktopQueryContextKey{}, owner)
	ctx = cliproxyexecutor.WithClaudeDesktopSessionBinding(ctx, cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: auth.ID, ProfileID: owner.profileID, Egress: auth.ProxyURL, SessionID: host.SessionID()})
	metadata := map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID, "working_dir": folder}
	if caller := claudetasks.CallerFromContext(ctx); caller.PromptID != "" {
		metadata[claudeDesktopPromptIDMetadataKey] = caller.PromptID
	}
	result, err := e.credentialManager.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: body, Format: sdktranslator.FormatClaude},
		cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
			Metadata: metadata})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Chunks == nil {
		return nil, errors.New("Claude Desktop remote input returned no model stream")
	}
	var buffer bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok {
				if _, ownedChild := claudetasks.InvocationFromContext(ctx); ownedChild {
					var response claudeprompt.Response
					response.EnableNativeContent()
					response.ObservePayload(buffer.Bytes(), true)
					return response.NativeCompletedMessage()
				}
				return helps.CollectClaudeMessageSSE(buffer.Bytes())
			}
			if chunk.Err != nil {
				return nil, chunk.Err
			}
			buffer.Write(chunk.Payload)
		}
	}
}

func (e *ClaudeExecutor) resolveDesktopAgentModel(parent, selected, agentType string) (string, error) {
	model := parent
	switch selected {
	case "":
	case "sonnet":
		model = "claude-sonnet-5"
	case "opus":
		model = "claude-opus-5"
	case "haiku":
		model = "claude-haiku-4-5-20251001"
	case "fable":
		model = "fable"
	default:
		return "", errors.New("Agent model must be sonnet, opus, haiku or fable")
	}
	if agentType != "general-purpose" {
		return "", errors.New("Agent definition is unavailable")
	}
	// This is a variant-availability probe, not an upstream payload. The
	// executor injects actual role-owned diagnostics before its final plan.
	body, _ := json.Marshal(map[string]any{"model": model, "diagnostics": map[string]any{}})
	_, err := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleSubagent, model, nil)
	return model, err
}

// desktopAgentDefinitionContext resolves the tool-catalog gates the native
// worker reads for one request model, in the order the serializer first
// reaches them (Agent is serialized first): the lean-prompt override
// (tengu_velvet_tide, read only for models outside the lean lane), the
// subagent steer (tengu_thistle_grebe), fine-grained tool streaming
// (tengu_fgts), then SendMessage's cross-session lane (tengu_harbor_kite_win
// on Windows, then tengu_harbor_kite) and the account subscription. Agent
// teams need the worker's environment flag, fork needs an interactive session
// and background tasks stay enabled, so those gates keep their zero values.
// The tool-search decision then reads the unsupported-model list
// (tengu_tool_search_unsupported_models), the placeholder gate
// (tengu_deferred_stub_tool) and the non-deferrable builtins
// (tengu_non_deferrable_builtins) from the same host.
func (e *ClaudeExecutor) desktopAgentDefinitionContext(auth *cliproxyauth.Auth, host *claudefeatures.Host, model string) claudetasks.DefinitionContext {
	definition := claudetasks.DefinitionContext{Model: model, SubscriptionType: claudeDesktopSubscriptionType(auth)}
	feature := func(name string, fallback json.RawMessage) json.RawMessage {
		if e == nil || e.desktopATIS == nil {
			return fallback
		}
		value, err := e.desktopATIS.featureValueOnHost(auth, host, name, fallback)
		if err != nil {
			return fallback
		}
		return value.Raw
	}
	if !claudetasks.LeanPromptModel(model) {
		definition.LeanPromptForced = claudefeatures.Truthy(feature("tengu_velvet_tide", json.RawMessage(`false`)))
	}
	var steer string
	if json.Unmarshal(feature("tengu_thistle_grebe", json.RawMessage(`null`)), &steer) == nil {
		definition.SubagentSteer = steer
	}
	definition.EagerInputStreaming = claudefeatures.Truthy(feature("tengu_fgts", json.RawMessage(`false`)))
	crossSession := true
	if e != nil && e.desktopProfile != nil && e.desktopProfile.Telemetry.Runtime.OSPlatform == "win32" {
		crossSession = claudefeatures.Truthy(feature("tengu_harbor_kite_win", json.RawMessage(`false`)))
	}
	definition.CrossSessionEnabled = crossSession && claudefeatures.Truthy(feature("tengu_harbor_kite", json.RawMessage(`false`)))
	// ToolSearchDisabled and ToolSearchFetchRule keep their zero values: the
	// Desktop worker runs with ENABLE_TOOL_SEARCH unset on the first-party
	// host with experimental betas enabled (mode "tst"), and the fetch rule is
	// client data (juniper_shoal.gorse_hollow, native default false) that
	// neither the captured profile nor this runtime carries. An absent
	// unsupported-model list stays nil so the SDK default applies; a present
	// list (even empty) replaces it.
	var unsupported []string
	if json.Unmarshal(feature("tengu_tool_search_unsupported_models", json.RawMessage(`null`)), &unsupported) == nil {
		definition.ToolSearchUnsupportedModels = unsupported
	}
	definition.DeferredStubDisabled = !claudefeatures.Truthy(feature("tengu_deferred_stub_tool", json.RawMessage(`true`)))
	definition.NonDeferrableBuiltins = claudeDesktopNonDeferrableBuiltins(feature("tengu_non_deferrable_builtins", json.RawMessage(`null`)), model)
	return definition
}

// claudeDesktopNonDeferrableBuiltins reads tengu_non_deferrable_builtins the
// way the native lookup does: a plain array applies to every model; an object
// is keyed by model substring and the first key contained in the lowercased
// model wins, otherwise the "*" entry; anything else means no builtins.
func claudeDesktopNonDeferrableBuiltins(raw json.RawMessage, model string) []string {
	names := func(value gjson.Result) []string {
		if !value.IsArray() {
			return nil
		}
		var result []string
		for _, item := range value.Array() {
			if item.Type == gjson.String {
				result = append(result, item.String())
			}
		}
		return result
	}
	value := gjson.ParseBytes(raw)
	if !value.IsObject() {
		return names(value)
	}
	lowered := strings.ToLower(model)
	var fallback gjson.Result
	var selected *gjson.Result
	value.ForEach(func(key, item gjson.Result) bool {
		if key.String() == "*" {
			fallback = item
			return true
		}
		if strings.Contains(lowered, strings.ToLower(key.String())) {
			selected = &item
			return false
		}
		return true
	})
	if selected == nil {
		selected = &fallback
	}
	return names(*selected)
}

// claudeDesktopSubscriptionType reads the account subscription the same way
// the SDK telemetry layer reports it, so the catalog and the events agree.
func claudeDesktopSubscriptionType(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		for _, key := range []string{"subscription_type", "subscriptionType", "billing_type"} {
			if value, _ := auth.Metadata[key].(string); strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return "pro"
}
