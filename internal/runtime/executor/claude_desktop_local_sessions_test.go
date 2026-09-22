package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudedesktop "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func newLocalExecutorTest(t *testing.T, telemetryFactories ...claudetelemetry.EndpointDoerFactory) (*ClaudeAccountExecutor, []*cliproxyauth.Auth) {
	t.Helper()
	var e *ClaudeAccountExecutor
	var auths []*cliproxyauth.Auth
	if len(telemetryFactories) == 0 {
		e, auths = newExecutionSessionAccountTest(t)
	} else {
		e = newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
		e.telemetryEndpointDoerFactory = telemetryFactories[0]
		t.Cleanup(e.Close)
		auths = []*cliproxyauth.Auth{
			newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000001", "26100000-0000-4000-8000-000000000001", "37100000-0000-4000-8000-000000000001"),
			newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000002", "26100000-0000-4000-8000-000000000002", "37100000-0000-4000-8000-000000000002"),
		}
		for _, auth := range auths {
			auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = claudedesktop.TelemetryMaterials{
				SegmentWriteKey:         "segment0123456789abcdef01234567",
				DatadogLogsAPIKey:       "datadoglogs0123456789abcdef01234567",
				DatadogRUMClientToken:   "datadogrum0123456789abcdef012345678",
				DatadogRUMApplicationID: "77777777-7777-4777-8777-777777777777",
				SentryPublicKey:         "abcdef0123456789abcdef0123456789",
			}
		}
		prepareExecutionSessionAccountTest(t, e, auths)
	}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(e)
	e.credentialManager = manager
	for _, auth := range auths {
		auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	return e, auths
}

func newLocalTelemetryExecutorTest(t *testing.T) (*ClaudeAccountExecutor, *cliproxyauth.Auth, *claudeDesktopTelemetryTestDoer) {
	t.Helper()
	doer := &claudeDesktopTelemetryTestDoer{}
	e, auths := newLocalExecutorTest(t, func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			response, err := doer.Do(request)
			if response != nil && (role == "sdk-event-logging" || role == "datadog-logs" || role == "datadog-logs-browser") {
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
			}
			return response, err
		})
	})
	return e, auths[0], doer
}

func TestLocalExecutorStopWaitsForRequestCompletion(t *testing.T) {
	e, auths := newLocalExecutorTest(t)
	auth := auths[0]
	view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "blocked input"})
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce bool
	defer func() {
		if !releaseOnce {
			close(release)
		}
	}()
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(canceled)
		<-release
		return nil, r.Context().Err()
	})))
	sent := make(chan error, 1)
	go func() {
		_, err := e.SendDesktopLocalMessage(ctx, auth.ID, view.Session.ID, cliproxyexecutor.ClaudeDesktopLocalInput{ExpectedGeneration: view.Session.Generation, Message: "blocked input"})
		sent <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	stopped := make(chan error, 1)
	go func() {
		_, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: view.Session.ID, ExpectedQueryID: view.Session.QueryID})
		stopped <- err
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("request not canceled")
	}
	if _, err := e.ResumeDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: view.Session.ID, ExpectedGeneration: view.Session.Generation}); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("overlapping successor", err)
	}
	select {
	case err := <-stopped:
		t.Fatal("stop returned before callback completed", err)
	default:
	}
	releaseOnce = true
	close(release)
	select {
	case err := <-sent:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("request cancellation not visible", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request failed to join")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop failed to join")
	}
	resumed, err := e.ResumeDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: view.Session.ID, ExpectedGeneration: view.Session.Generation})
	if err != nil || resumed.Session.Generation == view.Session.Generation || len(resumed.Messages) != 1 {
		t.Fatal("resume lost saved input", resumed, err)
	}
	t.Log("LOCAL_EXECUTOR_STOP_VERIFIED upstream_canceled=true final_callback_joined=true early_resume=rejected history_preserved=true")
}

func TestLocalUIObservationsRejectUnmeasuredOrOutOfOrderFacts(t *testing.T) {
	e, auths := newLocalExecutorTest(t)
	auth := auths[0]
	view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []cliproxyexecutor.ClaudeDesktopUIObservation{
		{Kind: "input_ready", ViewID: uuid.NewString(), Metrics: map[string]float64{"after_paint_ms": 1}},
		{Kind: "switch_painted", ViewID: uuid.NewString(), SessionID: view.Session.ID, ExpectedGeneration: view.Session.Generation, SwitchID: uuid.NewString(), Metrics: map[string]float64{"paint_ms": 2, "after_paint_ms": 1}},
		{Kind: "switch_painted", ViewID: uuid.NewString(), SessionID: view.Session.ID, ExpectedGeneration: view.Session.Generation, SwitchID: uuid.NewString(), Metrics: map[string]float64{"paint_ms": 1, "after_paint_ms": 2}},
	}
	for _, test := range cases {
		if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, test); !errors.Is(err, claudesessions.ErrInvalid) {
			t.Fatal("unobserved UI fact was accepted", test, err)
		}
	}
	t.Log("LOCAL_UI_FACT_VALIDATION_VERIFIED missing_measurement=rejected inverted_time=rejected unstarted_switch=rejected")
}

func TestLocalUIPendingTurnUsesServerOwnerAgeAndTurnDedupe(t *testing.T) {
	doer := &claudeDesktopTelemetryTestDoer{}
	e, auths := newLocalExecutorTest(t, func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			response, err := doer.Do(request)
			if response != nil && (role == "sdk-event-logging" || role == "datadog-logs" || role == "datadog-logs-browser") {
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
			}
			return response, err
		})
	})
	auth := auths[0]
	view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "pending input"})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))))
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		_, errSend := e.SendDesktopLocalMessage(ctx, auth.ID, view.Session.ID, cliproxyexecutor.ClaudeDesktopLocalInput{ExpectedGeneration: view.Session.Generation, Message: "pending input"})
		sent <- errSend
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pending request did not start")
	}

	runtime := accountRuntimeForAuth(t, e, auth.ID)
	_, local, busy, err := runtime.executor.desktopATIS.desktopRecords.LocalConversation(runtime.recordOwner, view.Session.ID)
	if err != nil || !busy || local.LastTurn.PromptID == "" || local.LastTurn.StartedAt.IsZero() {
		t.Fatal("real pending turn was not recorded", err)
	}
	originalNow := runtime.executor.desktopATIS.now
	observedNow := local.LastTurn.StartedAt.Add(desktopPendingTurnStuckIdleAfter - time.Millisecond)
	runtime.executor.desktopATIS.now = func() time.Time { return observedNow }
	t.Cleanup(func() { runtime.executor.desktopATIS.now = originalNow })

	observation := cliproxyexecutor.ClaudeDesktopUIObservation{
		Kind: "pending_turn_stuck_idle", ViewID: uuid.NewString(), SessionID: view.Session.ID,
		ExpectedGeneration: view.Session.Generation, Metrics: map[string]float64{},
	}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, observation); !errors.Is(err, claudesessions.ErrInvalid) {
		t.Fatal("pending turn was accepted before the server-side age threshold", err)
	}

	observedNow = local.LastTurn.StartedAt.Add(desktopPendingTurnStuckIdleAfter + time.Second)
	foreign := observation
	foreign.ViewID = uuid.NewString()
	if err := e.ObserveDesktopSessionUI(t.Context(), auths[1].ID, foreign); !errors.Is(err, claudesessions.ErrNotFound) {
		t.Fatal("foreign account observed another owner's pending turn", err)
	}
	stale := observation
	stale.ViewID, stale.ExpectedGeneration = uuid.NewString(), uuid.NewString()
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, stale); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("stale generation observed a pending turn", err)
	}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, observation); err != nil {
		t.Fatal(err)
	}
	duplicate := observation
	duplicate.ViewID = uuid.NewString()
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, duplicate); err != nil {
		t.Fatal(err)
	}
	if err := runtime.executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	segment, desktop := countLocalUITelemetryEvent(t, doer.Requests(), "claudeai.epitaxy.session.pending_turn_stuck_idle")
	if segment != 1 || desktop != 1 {
		t.Fatalf("pending turn deliveries = Segment %d, Desktop %d; want one owner-scoped turn event per endpoint", segment, desktop)
	}

	cancel()
	select {
	case err := <-sent:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("pending request did not end through its real context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request did not stop")
	}
	t.Log("LOCAL_PENDING_UI_VERIFIED owner_scoped=true early_rejected=true stale_rejected=true turn_deduped=true dual_fire=true")
}

func TestLocalUISessionWatchDemandUsesServerVisibilityState(t *testing.T) {
	doer := &claudeDesktopTelemetryTestDoer{}
	e, auths := newLocalExecutorTest(t, func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			response, err := doer.Do(request)
			if response != nil && (role == "sdk-event-logging" || role == "datadog-logs" || role == "datadog-logs-browser") {
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
			}
			return response, err
		})
	})
	auth := auths[0]
	view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "watch demand"})
	if err != nil {
		t.Fatal(err)
	}
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	observedAt := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	originalNow := runtime.executor.desktopATIS.now
	runtime.executor.desktopATIS.now = func() time.Time { return observedAt }
	t.Cleanup(func() { runtime.executor.desktopATIS.now = originalNow })

	base := cliproxyexecutor.ClaudeDesktopUIObservation{
		ViewID: uuid.NewString(), SessionID: view.Session.ID, ExpectedGeneration: view.Session.Generation,
		Metrics: map[string]float64{}, CacheHit: false,
	}
	restoredBeforeSuppression := base
	restoredBeforeSuppression.Kind = "sessions_watch_demand_restored"
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, restoredBeforeSuppression); !errors.Is(err, claudesessions.ErrInvalid) {
		t.Fatal("restoration without an accepted suppression", err)
	}
	malformed := base
	malformed.Kind, malformed.WasHidden = "sessions_watch_demand_suppressed", true
	malformed.Metrics = map[string]float64{"duration_ms": 1}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, malformed); !errors.Is(err, claudesessions.ErrInvalid) {
		t.Fatal("client-supplied watch duration was accepted", err)
	}

	suppressed := base
	suppressed.Kind, suppressed.WasHidden = "sessions_watch_demand_suppressed", true
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, suppressed); err != nil {
		t.Fatal(err)
	}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, suppressed); err != nil {
		t.Fatal("duplicate suppression should not create another lifecycle", err)
	}
	wrongView := restoredBeforeSuppression
	wrongView.ViewID = uuid.NewString()
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, wrongView); !errors.Is(err, claudesessions.ErrInvalid) {
		t.Fatal("different view restored another view's suppression", err)
	}
	stale := restoredBeforeSuppression
	stale.ExpectedGeneration = uuid.NewString()
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, stale); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("stale generation restored a current view suppression", err)
	}

	observedAt = observedAt.Add(2345 * time.Millisecond)
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, restoredBeforeSuppression); err != nil {
		t.Fatal(err)
	}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, restoredBeforeSuppression); !errors.Is(err, claudesessions.ErrInvalid) {
		t.Fatal("duplicate restoration was accepted", err)
	}
	if err := runtime.executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		"claudeai.television.sessions_watch.demand_suppressed",
		"claudeai.television.sessions_watch.demand_restored",
	} {
		segment, desktop := countLocalUITelemetryEvent(t, doer.Requests(), event)
		if segment != 3 || desktop != 3 {
			t.Fatalf("%s deliveries = Segment %d, Desktop %d; want three server-controlled watcher tags per endpoint", event, segment, desktop)
		}
	}
	for _, properties := range localUITelemetrySegmentProperties(t, doer.Requests(), "claudeai.television.sessions_watch.demand_restored") {
		if !strings.Contains(properties, `"suppressed_duration_ms":2345`) || !strings.Contains(properties, `"trigger":"focus"`) {
			t.Fatalf("restoration did not use the server-measured duration: %s", properties)
		}
		if strings.Contains(properties, "host_slept_in_gap") {
			t.Fatalf("restoration invented a host sleep state: %s", properties)
		}
	}
	if _, desktop := countLocalUITelemetryEvent(t, doer.Requests(), "desktop_ccd_session_idle_timeout_started"); desktop != 0 {
		t.Fatalf("hidden session without measured request activity invented %d idle timeout events", desktop)
	}
	t.Log("LOCAL_SESSION_WATCH_UI_VERIFIED server_duration=true tags_controlled=true view_scoped=true generation_scoped=true dual_fire=true")
}

func TestDesktopActivationScansOwnedTranscriptIndices(t *testing.T) {
	e, auth, doer := newLocalTelemetryExecutorTest(t)
	if _, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "lease candidate"}); err != nil {
		t.Fatal(err)
	}
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	if err := runtime.executor.Activate(auth); err != nil {
		t.Fatal(err)
	}
	if err := runtime.executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, request := range doer.Requests() {
		var batch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
					Metadata  string `json:"metadata"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			continue
		}
		for _, event := range batch.Events {
			if event.EventData.EventName != "desktop_ccd_transcript_lease_pass" {
				continue
			}
			var metadata struct {
				Candidates      int    `json:"candidates"`
				Renewed         int    `json:"renewed"`
				Fresh           int    `json:"fresh"`
				Missing         int    `json:"missing"`
				Errors          int    `json:"errors"`
				RetentionDays   int    `json:"retention_days"`
				RetentionSource string `json:"retention_source"`
			}
			if json.Unmarshal([]byte(event.EventData.Metadata), &metadata) == nil && metadata.Candidates == 1 {
				if metadata.Renewed != 0 || metadata.Fresh != 0 || metadata.Missing != 1 || metadata.Errors != 0 || metadata.RetentionDays != 30 || metadata.RetentionSource != "default" {
					t.Fatalf("transcript lease metadata = %+v", metadata)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("activation did not scan the actual durable session list")
	}
}

func TestDesktopResumeLifecycleUsesAdmittedLocalAndGeneralPaths(t *testing.T) {
	for _, general := range []bool{false, true} {
		name := "local"
		if general {
			name = "general"
		}
		t.Run(name, func(t *testing.T) {
			e, auth, doer := newLocalTelemetryExecutorTest(t)
			view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "resume lifecycle"})
			if err != nil {
				t.Fatal(err)
			}
			stopped, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: view.Session.ID, ExpectedQueryID: view.Session.QueryID})
			if err != nil || stopped.Running {
				t.Fatal("session did not stop", stopped, err)
			}
			operation := cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: stopped.ID, ExpectedGeneration: stopped.Generation}
			if general {
				if resumed, errResume := e.ResumeDesktopSession(t.Context(), auth.ID, operation); errResume != nil || !resumed.Running || resumed.Generation == stopped.Generation {
					t.Fatal("general resume failed", resumed, errResume)
				}
			} else if resumed, errResume := e.ResumeDesktopLocalSession(t.Context(), auth.ID, operation); errResume != nil || !resumed.Session.Running || resumed.Session.Generation == stopped.Generation {
				t.Fatal("local resume failed", resumed.Session, errResume)
			}
			runtime := accountRuntimeForAuth(t, e, auth.ID)
			if err := runtime.executor.desktopTelemetry.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			names := localUIDesktopEventNames(t, doer.Requests())
			last := -1
			for _, event := range []string{"desktop_ccd_session_idle_warm_start", "desktop_ccd_resume_bring_home_timing", "desktop_ccd_session_idle_warm_complete"} {
				position := indexString(names, event)
				if position <= last {
					t.Fatalf("resume lifecycle order = %v", names)
				}
				last = position
			}
		})
	}
}

func TestLocalUIIdleLifecycleUsesRealRequestAndRemoteControlState(t *testing.T) {
	e, auth, doer := newLocalTelemetryExecutorTest(t)
	view, err := e.StartDesktopLocalSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopLocalStart{Model: "claude-opus-5", Folder: t.TempDir(), Message: "idle lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))))
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		_, errSend := e.SendDesktopLocalMessage(ctx, auth.ID, view.Session.ID, cliproxyexecutor.ClaudeDesktopLocalInput{ExpectedGeneration: view.Session.Generation, Message: "idle lifecycle"})
		sent <- errSend
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not become active")
	}
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	activity, err := runtime.executor.desktopTelemetry.DesktopActivity(auth, view.Session.ID, view.Session.SDKSessionID)
	if err != nil || !activity.Found || activity.PendingRequests != 1 || activity.LastActivityAt.IsZero() {
		t.Fatal("real request activity was not visible", activity, err)
	}
	observedAt := activity.LastActivityAt.Add(16 * time.Minute)
	originalNow := runtime.executor.desktopATIS.now
	runtime.executor.desktopATIS.now = func() time.Time { return observedAt }
	t.Cleanup(func() { runtime.executor.desktopATIS.now = originalNow })
	var fire func()
	var idleTimer *time.Timer
	runtime.watchAfter = func(delay time.Duration, callback func()) *time.Timer {
		if delay != desktopSessionIdleTimeout {
			t.Fatalf("idle delay = %s", delay)
		}
		fire = callback
		idleTimer = time.NewTimer(time.Hour)
		return idleTimer
	}
	t.Cleanup(func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	})
	viewID := uuid.NewString()
	hidden := cliproxyexecutor.ClaudeDesktopUIObservation{Kind: "sessions_watch_demand_suppressed", ViewID: viewID, SessionID: view.Session.ID,
		ExpectedGeneration: view.Session.Generation, Metrics: map[string]float64{}, WasHidden: true}
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, hidden); err != nil || fire == nil {
		t.Fatal("hidden session did not arm the owned timer", err)
	}
	runtime.mu.Lock()
	runtime.remoteInputs = map[string]*helps.ClaudeDesktopRemoteInput{"owned-remote-control": {}}
	runtime.mu.Unlock()
	fire()
	runtime.mu.Lock()
	runtime.remoteInputs = nil
	runtime.mu.Unlock()
	observedAt = observedAt.Add(time.Second)
	visible := hidden
	visible.Kind, visible.WasHidden = "sessions_watch_demand_restored", false
	if err := e.ObserveDesktopSessionUI(t.Context(), auth.ID, visible); err != nil {
		t.Fatal(err)
	}
	if err := runtime.executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for event, want := range map[string]int{
		"desktop_ccd_session_visibility_changed":     2,
		"desktop_ccd_session_idle_timeout_started":   1,
		"desktop_ccd_session_pause_blocked_by_rc":    1,
		"desktop_ccd_session_idle_pause_declined":    1,
		"desktop_ccd_session_idle_timeout_cancelled": 1,
	} {
		_, desktop := countLocalUITelemetryEvent(t, doer.Requests(), event)
		if desktop != want {
			t.Fatalf("%s count = %d, want %d", event, desktop, want)
		}
	}
	cancel()
	select {
	case err := <-sent:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("active request did not end through cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active request did not stop")
	}
}

func localUIDesktopEventNames(t *testing.T, requests []claudeDesktopTelemetryRecordedRequest) []string {
	t.Helper()
	var names []string
	for _, request := range requests {
		var batch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			continue
		}
		for _, event := range batch.Events {
			names = append(names, event.EventData.EventName)
		}
	}
	return names
}

func indexString(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func countLocalUITelemetryEvent(t *testing.T, requests []claudeDesktopTelemetryRecordedRequest, eventName string) (segment, desktop int) {
	t.Helper()
	for _, request := range requests {
		var segmentBatch struct {
			Batch []struct {
				Event string `json:"event"`
			} `json:"batch"`
		}
		if err := json.Unmarshal(request.Body, &segmentBatch); err == nil {
			for _, item := range segmentBatch.Batch {
				if item.Event == eventName {
					segment++
				}
			}
		}
		var desktopBatch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &desktopBatch); err == nil {
			for _, item := range desktopBatch.Events {
				if item.EventData.EventName == eventName {
					desktop++
				}
			}
		}
	}
	return segment, desktop
}

func localUITelemetrySegmentProperties(t *testing.T, requests []claudeDesktopTelemetryRecordedRequest, eventName string) []string {
	t.Helper()
	var properties []string
	for _, request := range requests {
		var segmentBatch struct {
			Batch []struct {
				Event      string          `json:"event"`
				Properties json.RawMessage `json:"properties"`
			} `json:"batch"`
		}
		if err := json.Unmarshal(request.Body, &segmentBatch); err != nil {
			continue
		}
		for _, item := range segmentBatch.Batch {
			if item.Event == eventName {
				properties = append(properties, string(item.Properties))
			}
		}
	}
	return properties
}
