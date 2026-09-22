package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	claudedesktop "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

var _ cliproxyexecutor.ClaudeDesktopLocalController = (*ClaudeAccountExecutor)(nil)

const desktopPendingTurnStuckIdleAfter = 30 * time.Second

const desktopSessionWatchSuppressionLimit = 1024
const desktopSessionIdleTimeout = 15 * time.Minute

type desktopSessionIdleTimer struct {
	suppressedAt        time.Time
	timer               *time.Timer
	consecutiveDeclines int
}

func isDesktopSessionWatchObservation(kind string) bool {
	return kind == "sessions_watch_demand_suppressed" || kind == "sessions_watch_demand_restored"
}

// observeDesktopSessionWatchDemand serializes the actual visible/hidden state
// for one account runtime. A visibility report carries neither watcher tags nor
// a client duration: tags are fixed by the renderer contract and duration is
// calculated against this runtime's clock.
func (r *claudeAccountRuntime) observeDesktopSessionWatchDemand(scope, kind string, observedAt time.Time, emit func(time.Duration) error) (bool, time.Time, error) {
	if r == nil || scope == "" || emit == nil {
		return false, time.Time{}, claudesessions.ErrInvalid
	}
	r.watchDemandMu.Lock()
	defer r.watchDemandMu.Unlock()
	if r.watchSuppressions == nil {
		r.watchSuppressions = make(map[string]time.Time)
	}
	switch kind {
	case "sessions_watch_demand_suppressed":
		if _, alreadySuppressed := r.watchSuppressions[scope]; alreadySuppressed {
			return false, time.Time{}, nil
		}
		if err := emit(0); err != nil {
			return false, time.Time{}, err
		}
		if len(r.watchSuppressions) >= desktopSessionWatchSuppressionLimit {
			var oldestScope string
			var oldestAt time.Time
			for candidateScope, candidateAt := range r.watchSuppressions {
				if oldestScope == "" || candidateAt.Before(oldestAt) {
					oldestScope, oldestAt = candidateScope, candidateAt
				}
			}
			delete(r.watchSuppressions, oldestScope)
			if timer := r.watchIdleTimers[oldestScope]; timer != nil && timer.timer != nil {
				timer.timer.Stop()
			}
			delete(r.watchIdleTimers, oldestScope)
		}
		r.watchSuppressions[scope] = observedAt
		return true, observedAt, nil
	case "sessions_watch_demand_restored":
		suppressedAt, found := r.watchSuppressions[scope]
		if !found || observedAt.Before(suppressedAt) {
			return false, time.Time{}, claudesessions.ErrInvalid
		}
		if err := emit(observedAt.Sub(suppressedAt)); err != nil {
			return false, time.Time{}, err
		}
		delete(r.watchSuppressions, scope)
		if timer := r.watchIdleTimers[scope]; timer != nil && timer.timer != nil {
			timer.timer.Stop()
		}
		delete(r.watchIdleTimers, scope)
		return true, suppressedAt, nil
	default:
		return false, time.Time{}, claudesessions.ErrInvalid
	}
}

func (r *claudeAccountRuntime) scheduleDesktopSessionIdleTimeout(e *ClaudeAccountExecutor, scope, sessionID, generation string, suppressedAt time.Time) {
	if r == nil || e == nil || scope == "" || suppressedAt.IsZero() {
		return
	}
	r.mu.Lock()
	unavailable := r.retiring || r.closed
	r.mu.Unlock()
	if unavailable {
		return
	}
	r.watchDemandMu.Lock()
	defer r.watchDemandMu.Unlock()
	if current, found := r.watchSuppressions[scope]; !found || !current.Equal(suppressedAt) {
		return
	}
	if r.watchIdleTimers == nil {
		r.watchIdleTimers = make(map[string]*desktopSessionIdleTimer)
	}
	if _, exists := r.watchIdleTimers[scope]; exists {
		return
	}
	entry := &desktopSessionIdleTimer{suppressedAt: suppressedAt}
	after := r.watchAfter
	if after == nil {
		after = time.AfterFunc
	}
	entry.timer = after(desktopSessionIdleTimeout, func() {
		r.fireDesktopSessionIdleTimeout(e, scope, sessionID, generation, entry)
	})
	r.watchIdleTimers[scope] = entry
}

func (r *claudeAccountRuntime) fireDesktopSessionIdleTimeout(e *ClaudeAccountExecutor, scope, sessionID, generation string, entry *desktopSessionIdleTimer) {
	if r == nil || e == nil || entry == nil || !r.acquire() {
		return
	}
	defer r.release()
	r.watchDemandMu.Lock()
	current, found := r.watchSuppressions[scope]
	if !found || !current.Equal(entry.suppressedAt) || r.watchIdleTimers[scope] != entry {
		r.watchDemandMu.Unlock()
		return
	}
	delete(r.watchIdleTimers, scope)
	r.watchDemandMu.Unlock()
	auth, err := e.desktopRemoteAuth(r.authID)
	if err != nil {
		return
	}
	value, _, _, err := r.executor.desktopATIS.desktopRecords.LocalConversation(r.recordOwner, sessionID)
	if err != nil || value.Generation != generation || !value.Running {
		return
	}
	activity, err := r.executor.desktopTelemetry.DesktopActivity(auth, value.ID, value.SDKSessionID)
	if err != nil || !activity.Found || activity.LastActivityAt.IsZero() {
		return
	}
	seconds := desktopIdleSecondsSince(r.executor.desktopATIS.now(), activity.LastActivityAt)
	if seconds == nil {
		return
	}
	facts := claudetelemetry.DesktopCodeLifecycleFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, SecondsSinceLastActivity: seconds}
	if r.hasDesktopRemoteControl() {
		if err := r.executor.desktopTelemetry.RecordDesktopSessionPauseBlockedByRemoteControl(auth, facts); err != nil {
			helps.LogWithRequestID(context.Background()).WithError(err).Warn("claude desktop: idle remote-control block telemetry could not be persisted")
		}
	}
	if activity.PendingRequests > 0 {
		entry.consecutiveDeclines++
		facts.ConsecutiveDeclines = entry.consecutiveDeclines
		if err := r.executor.desktopTelemetry.RecordDesktopSessionIdlePauseDeclined(auth, facts); err != nil {
			helps.LogWithRequestID(context.Background()).WithError(err).Warn("claude desktop: idle pause-declined telemetry could not be persisted")
		}
	}
}

func (r *claudeAccountRuntime) hasDesktopRemoteControl() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.remoteInputs) != 0
}

func (r *claudeAccountRuntime) cancelDesktopSessionIdleTimers() {
	if r == nil {
		return
	}
	r.watchDemandMu.Lock()
	for _, timer := range r.watchIdleTimers {
		if timer != nil && timer.timer != nil {
			timer.timer.Stop()
		}
	}
	r.watchIdleTimers = make(map[string]*desktopSessionIdleTimer)
	r.watchSuppressions = make(map[string]time.Time)
	r.watchDemandMu.Unlock()
}

func desktopIdleSecondsSince(now, lastActivityAt time.Time) *int64 {
	if lastActivityAt.IsZero() {
		return nil
	}
	idleFor := now.Sub(lastActivityAt)
	if idleFor < 0 {
		idleFor = 0
	}
	seconds := int64(idleFor.Round(time.Second) / time.Second)
	return &seconds
}

func (e *ClaudeAccountExecutor) StartDesktopLocalSession(ctx context.Context, authID string, request cliproxyexecutor.ClaudeDesktopLocalStart) (cliproxyexecutor.ClaudeDesktopLocalView, error) {
	started := time.Now()
	if strings.TrimSpace(request.Message) == "" || len(request.Message) > 65536 {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, claudesessions.ErrInvalid
	}
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	model, err := inner.resolveDesktopRemoteModel(request.Model)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	folder, hasRepo, err := helps.InspectDesktopWorkspace(ctx, request.Folder)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, errors.Join(claudesessions.ErrInvalid, err)
	}
	preflight := time.Since(started)
	startup := claudesessions.LocalStartupObservation{StartedAt: started.UTC(), PreflightMS: preflight.Milliseconds(), HasRepo: hasRepo}
	var admitted *claudefeatures.Host
	var queryElapsed time.Duration
	value, err := inner.desktopATIS.desktopRecords.CreateLocal(ctx, runtime.recordOwner, claudesessions.LocalConversation{Model: model, Folder: folder, InitialMessage: request.Message, Startup: startup}, func(scope string) (*claudefeatures.Host, error) {
		at := time.Now()
		host, errStart := inner.desktopATIS.featureHosts.Main(scope, "")
		admitted = host
		if errStart == nil {
			errStart = inner.desktopATIS.startSDKFeatureHost(host)
		}
		queryElapsed = time.Since(at)
		return host, errStart
	})
	if err != nil {
		if admitted != nil {
			inner.desktopATIS.featureHosts.Retire(admitted)
		}
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	// The durable record exists regardless of analytics availability.
	facts := claudetelemetry.LocalSessionFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, QueryID: value.QueryID, Model: model,
		MessageLength: len(utf16.Encode([]rune(request.Message))), HasRepo: hasRepo, CreatedAt: started,
		PreflightMS: preflight.Milliseconds(), QueryMS: queryElapsed.Milliseconds(), InitMS: time.Since(started).Milliseconds() - preflight.Milliseconds()}
	if err := inner.desktopTelemetry.RecordDesktopLocalCreated(auth, facts); err != nil {
		helps.LogWithRequestID(ctx).WithError(err).Warn("local session created; creation telemetry could not be persisted")
	}
	if err := inner.desktopATIS.desktopRecords.SetLocalStartup(runtime.recordOwner, value.ID, facts.QueryMS, facts.InitMS); err != nil {
		view, errRead := e.GetDesktopLocalSession(authID, value.ID)
		return view, errors.Join(err, errRead)
	}
	return e.GetDesktopLocalSession(authID, value.ID)
}

func (e *ClaudeAccountExecutor) GetDesktopLocalSession(authID, id string) (cliproxyexecutor.ClaudeDesktopLocalView, error) {
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	defer runtime.release()
	value, local, busy, err := runtime.executor.desktopATIS.desktopRecords.LocalConversation(runtime.recordOwner, id)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	view := cliproxyexecutor.ClaudeDesktopLocalView{InitialMessage: local.InitialMessage, Session: cliproxyexecutor.ClaudeDesktopSession(value), Model: local.Model, Folder: local.Folder, Busy: busy, LastError: local.LastError,
		Messages: make([]cliproxyexecutor.ClaudeDesktopLocalMessage, len(local.Messages)), PromptID: local.LastTurn.PromptID, AssistantID: local.LastTurn.AssistantID}
	for i, message := range local.Messages {
		view.Messages[i] = cliproxyexecutor.ClaudeDesktopLocalMessage(message)
	}
	return view, nil
}

func (e *ClaudeAccountExecutor) ResumeDesktopLocalSession(ctx context.Context, authID string, operation cliproxyexecutor.ClaudeDesktopSessionResume) (cliproxyexecutor.ClaudeDesktopLocalView, error) {
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	started := time.Now()
	var admitted *claudefeatures.Host
	value, err := inner.desktopATIS.desktopRecords.ResumeLocal(ctx, runtime.recordOwner, operation.SessionID, operation.ExpectedGeneration, func(scope, session string) (*claudefeatures.Host, error) {
		host, err := inner.desktopATIS.featureHosts.Main(scope, session)
		admitted = host
		if err == nil {
			err = inner.desktopATIS.startSDKFeatureHost(host)
		}
		return host, err
	})
	if err != nil {
		if admitted != nil {
			inner.desktopATIS.featureHosts.Retire(admitted)
		}
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	if value.Generation != operation.ExpectedGeneration {
		inner.recordDesktopResumeLifecycle(ctx, auth, value, started, 0, 0)
	}
	return e.GetDesktopLocalSession(authID, operation.SessionID)
}

func (e *ClaudeAccountExecutor) SendDesktopLocalMessage(ctx context.Context, authID, id string, request cliproxyexecutor.ClaudeDesktopLocalInput) (cliproxyexecutor.ClaudeDesktopLocalView, error) {
	if len(request.Message) > 65536 {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, claudesessions.ErrInvalid
	}
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	defer runtime.release()
	inner := runtime.executor
	turn, value, local, err := inner.desktopATIS.desktopRecords.BeginLocalTurn(ctx, runtime.recordOwner, id, request.ExpectedGeneration, request.Message)
	if err != nil {
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	observed := local.LastTurn
	var payload []byte
	defer func() { _ = turn.Finish(nil, observed, errors.New("local request ended without a committed result")) }()
	host := turn.Host()
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		_ = turn.Finish(nil, observed, err)
		return cliproxyexecutor.ClaudeDesktopLocalView{}, err
	}
	owner := claudeDesktopQueryContext{manager: inner.desktopATIS, host: host, accountID: auth.ID, profileID: inner.desktopProfile.ProfileID,
		egress: auth.ProxyURL, identity: claudeDesktopATISIdentityHash(enrollment), session: host.SessionID(), lifetime: host.Context(), desktopSessionID: value.ID, remoteInput: true}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancel := context.AfterFunc(host.Context(), cancel)
	defer stopCancel()
	ctx = context.WithValue(ctx, claudeDesktopQueryContextKey{}, owner)
	ctx = cliproxyexecutor.WithClaudeDesktopSessionBinding(ctx, cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: auth.ID, ProfileID: owner.profileID, Egress: auth.ProxyURL, SessionID: host.SessionID()})
	messages := make([]map[string]any, 0, len(local.Messages))
	for _, message := range local.Messages {
		messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
	}
	body, _ := json.Marshal(map[string]any{"model": local.Model, "messages": messages, "max_tokens": 4096, "stream": true})
	metadata := map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID, "working_dir": local.Folder, claudeDesktopPromptIDMetadataKey: observed.PromptID}
	stream, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: local.Model, Payload: body, Format: sdktranslator.FormatClaude},
		cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Metadata: metadata})
	if err == nil && (stream == nil || stream.Chunks == nil) {
		err = errors.New("local session returned no model stream")
	}
	if err == nil {
		observed.RequestCount = 1
		if stream.Headers != nil {
			observed.RequestID = stream.Headers.Get("Request-Id")
		}
		var buffer bytes.Buffer
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
				break
			}
			buffer.Write(chunk.Payload)
			if observed.FirstAssistantAt.IsZero() && bytes.Contains(buffer.Bytes(), []byte(`"message_start"`)) {
				observed.FirstAssistantAt = time.Now().UTC()
			}
		}
		if err == nil {
			var response claudeprompt.Response
			response.EnableNativeContent()
			response.ObservePayload(buffer.Bytes(), true)
			payload, err = response.NativeCompletedMessage()
			for _, block := range gjson.GetBytes(payload, "content").Array() {
				if block.Get("type").String() == "tool_use" {
					observed.ToolCount++
				}
			}
		}
	}
	errSave := turn.Finish(payload, observed, err)
	if err == nil && errSave == nil && observed.IsFirstTurn {
		facts := claudetelemetry.LocalSessionFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, QueryID: value.QueryID, Model: local.Model,
			CreatedAt: local.Startup.StartedAt, RequestStartedAt: observed.StartedAt, FirstAssistantAt: observed.FirstAssistantAt,
			PreflightMS: local.Startup.PreflightMS, QueryMS: local.Startup.QueryMS, InitMS: local.Startup.InitMS}
		if err := inner.desktopTelemetry.RecordDesktopLocalStartTiming(auth, facts); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("local session started; startup telemetry could not be persisted")
		}
	}
	view, errRead := e.GetDesktopLocalSession(authID, id)
	return view, errors.Join(err, errSave, errRead)
}

func (e *ClaudeAccountExecutor) ObserveDesktopSessionUI(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopUIObservation) error {
	if _, err := uuid.Parse(observation.ViewID); err != nil {
		return claudesessions.ErrInvalid
	}
	allowed := map[string]bool{
		"duration_ms": true, "paint_ms": true, "after_paint_ms": true, "route_ms": true,
		"transcript_ms": true, "parse_ms": true, "mount_ms": true, "tab_age_ms": true,
		"receipt_ms": true, "settled_ms": true, "first_rows_ms": true, "skeleton_ms": true,
	}
	for key, value := range observation.Metrics {
		if !allowed[key] || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 604800000 {
			return claudesessions.ErrInvalid
		}
	}
	required := map[string][]string{
		"input_ready":             {"duration_ms", "mount_ms", "route_ms", "tab_age_ms", "after_paint_ms"},
		"first_text":              {"paint_ms", "receipt_ms", "after_paint_ms", "tab_age_ms"},
		"switch_painted":          {"paint_ms", "after_paint_ms"},
		"transcript_open_settled": {"settled_ms", "first_rows_ms", "skeleton_ms"},
	}
	for _, key := range required[observation.Kind] {
		if _, ok := observation.Metrics[key]; !ok {
			return claudesessions.ErrInvalid
		}
	}
	if observation.Kind == "first_text" || observation.Kind == "switch_painted" {
		if observation.Metrics["after_paint_ms"] < observation.Metrics["paint_ms"] {
			return claudesessions.ErrInvalid
		}
	}
	if observation.Kind == "first_text" && observation.Metrics["paint_ms"] < observation.Metrics["receipt_ms"] {
		return claudesessions.ErrInvalid
	}
	if observation.Kind == "transcript_open_settled" && (observation.Metrics["settled_ms"] < observation.Metrics["first_rows_ms"] || observation.Metrics["first_rows_ms"] < observation.Metrics["skeleton_ms"]) {
		return claudesessions.ErrInvalid
	}
	switch observation.Kind {
	case "input_ready":
		if observation.SessionID != "" {
			return claudesessions.ErrInvalid
		}
	case "first_text":
		if observation.PromptID == "" || observation.AssistantID == "" || observation.SessionID == "" {
			return claudesessions.ErrInvalid
		}
	case "switch_started", "switch_painted":
		if _, err := uuid.Parse(observation.SwitchID); err != nil || observation.SessionID == "" {
			return claudesessions.ErrInvalid
		}
	case "sidebar_session_opened":
		if _, err := uuid.Parse(observation.SwitchID); err != nil || observation.SessionID == "" || len(observation.Metrics) != 0 || observation.WasHidden || observation.CacheHit {
			return claudesessions.ErrInvalid
		}
	case "transcript_open_settled":
		if _, err := uuid.Parse(observation.SwitchID); err != nil || observation.SessionID == "" || observation.CacheHit {
			return claudesessions.ErrInvalid
		}
	case "pending_turn_stuck_idle":
		if observation.SessionID == "" || observation.SwitchID != "" || observation.PromptID != "" || observation.AssistantID != "" || len(observation.Metrics) != 0 || observation.CacheHit {
			return claudesessions.ErrInvalid
		}
	case "sessions_watch_demand_suppressed":
		if observation.SessionID == "" || observation.ExpectedGeneration == "" || observation.SwitchID != "" || observation.PromptID != "" || observation.AssistantID != "" || len(observation.Metrics) != 0 || !observation.WasHidden || observation.CacheHit {
			return claudesessions.ErrInvalid
		}
	case "sessions_watch_demand_restored":
		if observation.SessionID == "" || observation.ExpectedGeneration == "" || observation.SwitchID != "" || observation.PromptID != "" || observation.AssistantID != "" || len(observation.Metrics) != 0 || observation.WasHidden || observation.CacheHit {
			return claudesessions.ErrInvalid
		}
	default:
		return claudesessions.ErrInvalid
	}
	auth, err := e.desktopRemoteAuth(authID)
	if err != nil {
		return err
	}
	runtime, err := e.acquireDesktopSessionRuntime(authID)
	if err != nil {
		return err
	}
	defer runtime.release()
	registry := runtime.executor.desktopATIS.desktopRecords
	var pendingAgeMS int64
	if observation.Kind == "first_text" {
		_, local, _, err := registry.LocalConversation(runtime.recordOwner, observation.SessionID)
		if err != nil {
			return err
		}
		if local.LastTurn.PromptID != observation.PromptID || local.LastTurn.AssistantID != observation.AssistantID || local.LastError != "" {
			return claudesessions.ErrStaleQuery
		}
		hasText := false
		for _, message := range local.Messages {
			if message.ID != observation.AssistantID || message.Role != "assistant" {
				continue
			}
			for _, block := range gjson.ParseBytes(message.Content).Array() {
				if block.Get("type").String() == "text" && strings.TrimSpace(block.Get("text").String()) != "" {
					hasText = true
				}
			}
		}
		if !hasText {
			return claudesessions.ErrInvalid
		}
	}
	if observation.Kind == "transcript_open_settled" {
		_, local, _, err := registry.LocalConversation(runtime.recordOwner, observation.SessionID)
		if err != nil {
			return err
		}
		if len(local.Messages) == 0 {
			return claudesessions.ErrInvalid
		}
	}
	if observation.Kind == "pending_turn_stuck_idle" {
		value, local, busy, err := registry.LocalConversation(runtime.recordOwner, observation.SessionID)
		if err != nil {
			return err
		}
		if !busy || !value.Running || local.LastTurn.PromptID == "" || local.LastTurn.StartedAt.IsZero() {
			return claudesessions.ErrStaleQuery
		}
		pendingAge := runtime.executor.desktopATIS.now().Sub(local.LastTurn.StartedAt)
		if pendingAge < desktopPendingTurnStuckIdleAfter {
			return claudesessions.ErrInvalid
		}
		pendingAgeMS = pendingAge.Milliseconds()
		observation.PromptID = local.LastTurn.PromptID
	}
	if isDesktopSessionWatchObservation(observation.Kind) {
		value, _, _, err := registry.LocalConversation(runtime.recordOwner, observation.SessionID)
		if err != nil {
			return err
		}
		if value.Generation != observation.ExpectedGeneration {
			return claudesessions.ErrStaleQuery
		}
		watchFacts := claudetelemetry.LocalSessionFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, QueryID: value.QueryID}
		scope := observation.ViewID + "\x00" + observation.SessionID + "\x00" + observation.ExpectedGeneration
		observedAt := runtime.executor.desktopATIS.now()
		changed, suppressedAt, err := runtime.observeDesktopSessionWatchDemand(scope, observation.Kind, observedAt, func(duration time.Duration) error {
			watchFacts.SuppressedDurationMS = duration.Milliseconds()
			return runtime.executor.desktopTelemetry.RecordDesktopSessionWatchDemand(auth, observation.Kind, watchFacts)
		})
		if err != nil || !changed {
			return err
		}
		activity, errActivity := runtime.executor.desktopTelemetry.DesktopActivity(auth, value.ID, value.SDKSessionID)
		if errActivity != nil {
			helps.LogWithRequestID(ctx).WithError(errActivity).Warn("claude desktop: session activity could not be read for idle telemetry")
		}
		active := activity.Found && activity.PendingRequests > 0
		var seconds *int64
		if activity.Found {
			seconds = desktopIdleSecondsSince(observedAt, activity.LastActivityAt)
		}
		lifecycleFacts := claudetelemetry.DesktopCodeLifecycleFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, SecondsSinceLastActivity: seconds}
		visible := observation.Kind == "sessions_watch_demand_restored"
		if err := runtime.executor.desktopTelemetry.RecordDesktopSessionVisibility(auth, lifecycleFacts, visible, active); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: session visibility telemetry could not be persisted")
		}
		if visible {
			if err := runtime.executor.desktopTelemetry.RecordDesktopSessionIdleTimeoutCancelled(auth, lifecycleFacts); err != nil {
				helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: idle timeout cancellation telemetry could not be persisted")
			}
		} else {
			if seconds != nil {
				if err := runtime.executor.desktopTelemetry.RecordDesktopSessionIdleTimeoutStarted(auth, lifecycleFacts); err != nil {
					helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: idle timeout telemetry could not be persisted")
				}
			}
			runtime.scheduleDesktopSessionIdleTimeout(e, scope, value.ID, value.Generation, suppressedAt)
		}
		return nil
	}
	key := observation.ViewID + ":" + observation.Kind + ":" + observation.SessionID + ":" + observation.ExpectedGeneration + ":" + observation.PromptID + ":" + observation.SwitchID
	var prerequisites []string
	if observation.Kind == "pending_turn_stuck_idle" {
		// A stuck turn is a turn fact, not a tab fact. Deduplicate it across
		// every renderer view that may observe the same durable pending turn.
		key = observation.Kind + ":" + observation.SessionID + ":" + observation.ExpectedGeneration + ":" + observation.PromptID
	} else if observation.Kind == "switch_painted" {
		prerequisites = []string{strings.Replace(key, ":switch_painted:", ":switch_started:", 1)}
	} else if observation.Kind == "transcript_open_settled" {
		prerequisites = []string{
			strings.Replace(key, ":transcript_open_settled:", ":sidebar_session_opened:", 1),
			strings.Replace(key, ":transcript_open_settled:", ":switch_started:", 1),
		}
	}
	claimed, value, local, err := registry.ClaimUIObservation(runtime.recordOwner, key, observation.SessionID, observation.ExpectedGeneration, prerequisites...)
	if err != nil || !claimed {
		return err
	}
	values, err := registry.List(runtime.recordOwner)
	if err != nil {
		return err
	}
	running := 0
	for _, item := range values {
		if item.Running {
			running++
		}
	}
	facts := claudetelemetry.LocalSessionFacts{SessionID: value.ID, SDKSessionID: value.SDKSessionID, QueryID: value.QueryID, Model: local.Model,
		PromptID: local.LastTurn.PromptID, AssistantID: local.LastTurn.AssistantID, RequestID: local.LastTurn.RequestID,
		IsFirstTurn: local.LastTurn.IsFirstTurn, RequestCount: local.LastTurn.RequestCount, ToolCount: local.LastTurn.ToolCount,
		SessionCount: len(values), RunningCount: running, EntryCount: len(local.Messages), IsRunning: value.Running,
		SwitchID: observation.SwitchID, Metrics: observation.Metrics, WasHidden: observation.WasHidden, CacheHit: observation.CacheHit}
	if observation.Kind == "pending_turn_stuck_idle" {
		facts.Metrics = map[string]float64{"pending_age_ms": float64(pendingAgeMS)}
		facts.IsRunning = true
	}
	return runtime.executor.desktopTelemetry.RecordDesktopSessionUI(auth, observation.Kind, facts)
}
