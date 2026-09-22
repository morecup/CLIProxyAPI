package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	desktopauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// These operations use the provider's public controller. No direct Registry
// call or slash-command message stands in for the stop producer.
func TestClaudeDesktopSessionControllerStopsOwnedQueryAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream", "count", "http-count"} {
		t.Run(mode, func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			headers := http.Header{"X-Session-Id": {uuid.NewString()}}
			var owners []claudeDesktopQueryContext
			setup := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1/messages" {
					owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
				}
				return executionSessionTestResponse(t, request), nil
			})))
			for _, connection := range []string{"target", "sibling"} {
				if err := invokeQueryLifetimeEntry(setup, e, auths[0], headers, "execute", nil, desktopExecutionMetadata(connection)); err != nil {
					t.Fatal(err)
				}
			}
			list, err := e.ListDesktopSessions(auths[0].ID)
			if err != nil || len(list) != 2 || owners[0].desktopSessionID == "" || owners[0].desktopSessionID == owners[1].desktopSessionID || owners[0].session != owners[1].session {
				t.Fatalf("record identity: %+v %v", list, err)
			}
			operation := cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: owners[0].desktopSessionID, ExpectedQueryID: owners[0].host.ID()}
			if _, err := e.StopDesktopSession(t.Context(), auths[1].ID, operation); !errors.Is(err, claudesessions.ErrNotFound) {
				t.Fatal("cross-account stop", err)
			}
			started, blocked := make(chan struct{}, 1), make(chan struct{})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				response := executionSessionTestResponse(t, request)
				prefix := `{"unfinished":`
				if response.Header.Get("Content-Type") == "text/event-stream" {
					prefix = "data: {\"type\":\"ping\"}\n\n"
				}
				response.Body = &queryLifetimeBlockingBody{ctx: request.Context(), prefix: strings.NewReader(prefix), blocked: blocked}
				started <- struct{}{}
				return response, nil
			})))
			if strings.HasPrefix(mode, "http") {
				ctx, err = e.executionSessions.Bind(ctx, desktopExecutionMetadata("target"))
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				done <- invokeQueryLifetimeEntry(ctx, e, auths[0], headers, mode, nil, desktopExecutionMetadata("target"))
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("model was not dispatched")
			}
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("body did not block")
			}
			stopped, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation)
			if err != nil || stopped.Running || stopped.SDKSessionID != owners[0].session {
				t.Fatalf("stop %+v %v", stopped, err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stop did not cancel query")
			}
			if owners[1].host.Context().Err() != nil {
				t.Fatal("sibling query retired")
			}
			if _, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation); err != nil {
				t.Fatal("duplicate stop", err)
			}
			if err := invokeQueryLifetimeEntry(setup, e, auths[0], headers, "execute", nil, desktopExecutionMetadata("target")); err != nil {
				t.Fatal("resume", err)
			}
			next := owners[len(owners)-1]
			if next.desktopSessionID != stopped.ID || next.session != stopped.SDKSessionID || next.host.ID() == stopped.QueryID {
				t.Fatal("stop/restart conflated record, transcript and query")
			}
			if _, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation); !errors.Is(err, claudesessions.ErrStaleQuery) || next.host.Context().Err() != nil {
				t.Fatal("stale query targeted successor", err)
			}
		})
	}
}

func TestClaudeDesktopRecordIdentityPersistsWithoutTelemetry(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	for _, auth := range auths {
		inner := accountRuntimeForAuth(t, e, auth.ID).executor
		inner.desktopTelemetry.Close()
		inner.desktopTelemetry = nil
	}
	header := http.Header{"X-Session-Id": {uuid.NewString()}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, e, auths[0], header, "execute", nil, desktopExecutionMetadata("persisted")); err != nil {
		t.Fatal(err)
	}
	list, err := e.ListDesktopSessions(auths[0].ID)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	before := list[0]
	e.Close()
	restored := newClaudeAccountTestExecutor(e.cfg)
	t.Cleanup(restored.Close)
	prepareExecutionSessionAccountTest(t, restored, auths)
	for _, auth := range auths {
		inner := accountRuntimeForAuth(t, restored, auth.ID).executor
		inner.desktopTelemetry.Close()
		inner.desktopTelemetry = nil
	}
	list, err = restored.ListDesktopSessions(auths[0].ID)
	if err != nil || len(list) != 1 || list[0].ID != before.ID || list[0].Running || list[0].QueryID != "" {
		t.Fatal("durable record was not independently restored", list, err)
	}
	if err := invokeQueryLifetimeEntry(ctx, restored, auths[0], header, "execute", nil, desktopExecutionMetadata("persisted")); err != nil {
		t.Fatal(err)
	}
	list, err = restored.ListDesktopSessions(auths[0].ID)
	if err != nil || len(list) != 1 || list[0].ID != before.ID || list[0].SDKSessionID != before.SDKSessionID || list[0].QueryID == before.QueryID || !list[0].Running {
		t.Fatal("query reconstruction changed durable identities", list, err)
	}
}

func TestClaudeDesktopRecordStopProducesActualTelemetryDelivery(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	factory := func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if request.URL.Hostname() == "claude.ai" {
				mu.Lock()
				bodies = append(bodies, body)
				mu.Unlock()
			}
			proto, major, minor := "HTTP/2.0", 2, 0
			if request.URL.Hostname() == "api.anthropic.com" {
				proto, major, minor = "HTTP/1.1", 1, 1
			}
			return &http.Response{StatusCode: http.StatusNoContent, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})
	}
	e, auths := newExecutionSessionAccountTest(t, factory)
	headers := http.Header{"X-Session-Id": {uuid.NewString()}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	for _, connection := range []string{"stop-target", "retained"} {
		if err := invokeQueryLifetimeEntry(ctx, e, auths[0], headers, "execute", nil, desktopExecutionMetadata(connection)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.ListDesktopSessions(auths[0].ID)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	target := list[0]
	if _, err := e.StopDesktopSession(t.Context(), auths[0].ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: target.ID, ExpectedQueryID: target.QueryID}); err != nil {
		t.Fatal(err)
	}
	if err := accountRuntimeForAuth(t, e, auths[0].ID).executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.Close()
	mu.Lock()
	defer mu.Unlock()
	initialized := map[string]int{}
	var targetStops []gjson.Result
	for _, body := range bodies {
		if !gjson.ValidBytes(body) {
			t.Fatal("delivered Renderer body is not JSON")
		}
		for _, event := range gjson.GetBytes(body, "events").Array() {
			metadata := gjson.Parse(event.Get("event_data.metadata").String())
			switch event.Get("event_data.event_name").String() {
			case "desktop_ccd_session_initialized":
				initialized[metadata.Get("session_id").String()]++
			case "desktop_ccd_session_stopped":
				if metadata.Get("session_id").String() == target.ID {
					targetStops = append(targetStops, metadata)
				}
			}
		}
	}
	if initialized[list[0].ID] != 1 || initialized[list[1].ID] != 1 || len(initialized) != 2 {
		t.Fatalf("ordinary delivery merged Desktop identities: %+v", initialized)
	}
	if len(targetStops) != 1 || targetStops[0].Get("trigger").String() != "user" || targetStops[0].Get("cli_session_id").String() != target.SDKSessionID || targetStops[0].Get("had_pending_cycle").Bool() || targetStops[0].Get("pending_seconds").Type != gjson.Null {
		t.Fatalf("operation-to-delivery mismatch or duplicate shutdown: %+v", targetStops)
	}
}

func TestClaudeDesktopSessionHeartbeatUsesOwnerRegistryAndDeliversTelemetry(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var requestTargets []string
	factory := func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			bodies = append(bodies, body)
			requestTargets = append(requestTargets, role+" "+request.URL.String())
			mu.Unlock()
			proto, major, minor := "HTTP/2.0", 2, 0
			if role == "sdk-event-logging" || role == "datadog-logs" || role == "datadog-logs-browser" {
				proto, major, minor = "HTTP/1.1", 1, 1
			}
			return &http.Response{StatusCode: http.StatusNoContent, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})
	}
	e := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	e.telemetryEndpointDoerFactory = factory
	t.Cleanup(e.Close)
	auths := []*cliproxyauth.Auth{
		newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000001", "26100000-0000-4000-8000-000000000001", "37100000-0000-4000-8000-000000000001"),
		newClaudeAccountRuntimeTestAuth(t, "15100000-0000-4000-8000-000000000002", "26100000-0000-4000-8000-000000000002", "37100000-0000-4000-8000-000000000002"),
	}
	for _, auth := range auths {
		auth.Metadata[desktopauth.MetadataTelemetryMaterialsKey] = desktopauth.TelemetryMaterials{
			SegmentWriteKey:         "segment0123456789abcdef01234567",
			DatadogLogsAPIKey:       "datadoglogs0123456789abcdef01234567",
			DatadogRUMClientToken:   "datadogrum0123456789abcdef012345678",
			DatadogRUMApplicationID: "77777777-7777-4777-8777-777777777777",
			SentryPublicKey:         "abcdef0123456789abcdef0123456789",
		}
	}
	prepareExecutionSessionAccountTest(t, e, auths)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(e)
	e.credentialManager = manager
	for _, auth := range auths {
		auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	headers := http.Header{"X-Session-Id": {uuid.NewString()}}
	var owners []claudeDesktopQueryContext
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages" {
			owners = append(owners, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext))
		}
		return executionSessionTestResponse(t, request), nil
	})))
	for _, entry := range []struct {
		auth       *cliproxyauth.Auth
		connection string
	}{{auths[0], "heartbeat-live"}, {auths[0], "heartbeat-closed"}, {auths[1], "heartbeat-foreign"}} {
		if err := invokeQueryLifetimeEntry(ctx, e, entry.auth, headers, "execute", nil, desktopExecutionMetadata(entry.connection)); err != nil {
			t.Fatal(err)
		}
	}
	owners[1].host.Close()
	if _, err := e.desktopRemoteAuth(auths[0].ID); err != nil {
		t.Fatalf("heartbeat auth lookup: %v", err)
	}
	runtime, err := e.acquireDesktopSessionRuntime(auths[0].ID)
	if err != nil {
		t.Fatalf("heartbeat runtime lookup: %v", err)
	}
	if len(runtime.recordOwner) != 64 {
		t.Fatalf("heartbeat record owner = %q", runtime.recordOwner)
	}
	runtime.release()
	if err := e.CheckDesktopSessionHeartbeats(t.Context(), auths[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := accountRuntimeForAuth(t, e, auths[0].ID).executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	const eventName = "claudeai.code.sessions.heartbeat_check_batch"
	segmentCount, desktopCount := 0, 0
	var segmentProperties, desktopProperties gjson.Result
	mu.Lock()
	defer mu.Unlock()
	for _, body := range bodies {
		for _, item := range gjson.GetBytes(body, "batch").Array() {
			if item.Get("event").String() == eventName {
				segmentCount++
				segmentProperties = item.Get("properties")
			}
		}
		for _, event := range gjson.GetBytes(body, "events").Array() {
			if event.Get("event_data.event_name").String() == eventName {
				desktopCount++
				desktopProperties = gjson.Parse(event.Get("event_data.properties").String())
			}
		}
	}
	if segmentCount != 1 || desktopCount != 1 {
		t.Fatalf("heartbeat deliveries: segment=%d desktop=%d targets=%v", segmentCount, desktopCount, requestTargets)
	}
	for name, properties := range map[string]gjson.Result{"segment": segmentProperties, "desktop": desktopProperties} {
		if properties.Get("sent").Int() != 2 || properties.Get("probe_dispatched").Int() != 1 || properties.Get("no_worker").Int() != 1 || properties.Get("trigger").String() != "tick" {
			t.Fatalf("%s heartbeat properties = %s", name, properties.Raw)
		}
	}
	if desktopProperties.Get("_dual_fire").Bool() != true || desktopProperties.Get("path").String() != "/epitaxy" {
		t.Fatalf("Desktop heartbeat copy suffix = %s", desktopProperties.Raw)
	}
}
