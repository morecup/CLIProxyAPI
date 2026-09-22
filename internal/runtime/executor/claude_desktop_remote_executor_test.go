package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type remoteExecutorPipe struct {
	*io.PipeReader
	writer *io.PipeWriter
}

func (p *remoteExecutorPipe) Close() error {
	return errors.Join(p.PipeReader.Close(), p.writer.Close())
}

func awaitRemoteExecutor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("remote executor did not complete")
		var zero T
		return zero
	}
}

func TestClaudeDesktopRemoteRejectsMissingTrustedDevice(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"15200000-0000-4000-8000-000000000001",
		"26200000-0000-4000-8000-000000000001",
		"37200000-0000-4000-8000-000000000001",
	)
	delete(auth.Metadata, claudedesktop.MetadataTrustedDeviceTokenKey)
	auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
	e := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(e.Close)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(e)
	e.credentialManager = manager
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("register inference credential: %v", errRegister)
	}
	if _, errRemote := e.desktopRemoteAuth(auth.ID); errRemote == nil || !strings.Contains(errRemote.Error(), "trusted-device credential is missing") {
		t.Fatalf("desktopRemoteAuth() error = %v", errRemote)
	}
}

func TestClaudeDesktopRemoteActualIngressExecution(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(e)
	e.credentialManager = manager
	for _, auth := range auths {
		auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}, {ID: "claude-sonnet-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	auth := auths[0]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	runtimeRef := accountRuntimeForAuth(t, e, auth.ID)
	defer func() {
		e.Close()
		deadline := time.After(10 * time.Second)
		for !runtimeRef.isClosed() {
			select {
			case <-deadline:
				t.Error("remote test runtime did not join")
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	inner.desktopControlPlane.Close()
	writers := make(chan *io.PipeWriter, 4)
	events := make(chan json.RawMessage, 128)
	var unarchives, creates atomic.Int32
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile,
		DoerFactory: func(_ context.Context, _ string, current *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			if current.ID != auth.ID {
				return nil, errors.New("cross-account control request")
			}
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				body, status := `{}`, 200
				switch {
				case request.URL.Path == "/v1/code/sessions":
					creates.Add(1)
					body = `{"session":{"id":"cse_fallback"}}`
				case strings.HasSuffix(request.URL.Path, "/unarchive"):
					unarchives.Add(1)
					status = 409
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
				case strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
					reader, writer := io.Pipe()
					writers <- writer
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &remoteExecutorPipe{reader, writer}}, nil
				case strings.HasSuffix(request.URL.Path, "/worker/events"):
					payload, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					for _, event := range gjson.GetBytes(payload, "events").Array() {
						events <- json.RawMessage(event.Get("payload").Raw)
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		}})
	type inference struct {
		model   string
		body    []byte
		owner   claudeDesktopQueryContext
		headers http.Header
	}
	requests := make(chan inference, 16)
	setup, cancelSetup := context.WithCancel(t.Context())
	setup = context.WithValue(setup, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages" {
			reader, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				return nil, err
			}
			requests <- inference{gjson.GetBytes(body, "model").String(), body, request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext), request.Header.Clone()}
			if strings.Contains(gjson.GetBytes(body, "messages").Raw, "interrupt remote input") && !strings.Contains(gjson.GetBytes(body, "messages").Raw, "after interrupt") {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
		}
		response := executionSessionTestResponse(t, request)
		if response.Header.Get("Content-Type") == "text/event-stream" {
			// The older observation-only fixture omits message.type/content.
			// A consuming SDK query needs a complete Messages protocol frame.
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				return nil, err
			}
			payload := strings.Replace(string(body), `"message":{"id"`, `"message":{"type":"message","content":[],"id"`, 1)
			payload = strings.ReplaceAll(payload, `"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`)
			response.Body = io.NopCloser(strings.NewReader(payload))
		}
		return response, nil
	})))
	value, err := e.StartDesktopRemoteSession(setup, auth.ID, cliproxyexecutor.ClaudeDesktopRemoteStart{RemoteSessionID: "session_remoteOne", Folder: `C:\code`, Model: "claude-opus-5"})
	if err != nil || !value.Running || value.RemoteState != "attached" {
		t.Fatal("start", value, err)
	}
	cancelSetup()
	writer := awaitRemoteExecutor(t, writers)
	if len(requests) != 0 || creates.Load() != 0 || unarchives.Load() != 1 {
		t.Fatal("startup fabricated inference or ignored remote origin")
	}
	accountScope, _ := json.Marshal([]string{auth.ID, inner.desktopProfile.ProfileID, auth.ProxyURL})
	transcriptScopeJSON, _ := json.Marshal([]string{string(accountScope), value.SDKSessionID})
	transcriptHash := sha256.Sum256(transcriptScopeJSON)
	transcriptScope := hex.EncodeToString(transcriptHash[:])
	transcript := helps.NewClaudeDesktopTranscriptStore(inner.desktopDurableStatePath, e.cfg.ProxyURL)
	if err := inner.desktopPrompts.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	index, err := transcript.LoadTranscript(transcriptScope)
	if err != nil || index.Revision != "" || len(index.UUIDs) != 0 {
		t.Fatal("remote attachment fabricated a transcript before input", err)
	}
	if _, err := os.Stat(index.Path); !os.IsNotExist(err) {
		t.Fatal("remote attachment created an empty transcript file", err)
	}
	if _, err := e.StartDesktopRemoteSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopRemoteStart{RemoteSessionID: "cse_remoteOne", Folder: `C:\code`, Model: "claude-opus-5"}); !errors.Is(err, claudesessions.ErrRemoteBound) {
		t.Fatal("duplicate creation", err)
	}
	if _, err := e.AttachDesktopRemoteSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); err != nil || unarchives.Load() != 1 {
		t.Fatal("reattach duplicated query", err)
	}
	if _, err := e.AttachDesktopRemoteSession(t.Context(), auths[1].ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); !errors.Is(err, claudesessions.ErrNotFound) {
		t.Fatal("foreign adoption", err)
	}
	send := func(sequence int, payload string) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintf(writer, "id: %d\nevent: client_event\ndata: {\"event_id\":\"event%d\",\"event_type\":\"user\",\"payload\":%s}\n\n", sequence, sequence, payload)
			done <- err
		}()
		if err := awaitRemoteExecutor(t, done); err != nil {
			t.Fatal(err)
		}
	}
	waitEvent := func(kind string) json.RawMessage {
		t.Helper()
		for {
			event := awaitRemoteExecutor(t, events)
			if gjson.GetBytes(event, "type").String() == "user" {
				t.Fatal("remote input echoed upstream as new local user")
			}
			if gjson.GetBytes(event, "type").String() == kind {
				return event
			}
		}
	}
	firstPayload := `{"type":"user","uuid":"one","model":"injected-model","system":"injected-system","headers":{"User-Agent":"injected-header"},"message":{"role":"user","content":"first remote input"}}`
	send(1, firstPayload)
	first := awaitRemoteExecutor(t, requests)
	if first.model != "claude-opus-5" || first.owner.accountID != auth.ID || first.owner.host.ID() != value.QueryID || !first.owner.remoteInput || strings.Contains(string(first.body), "injected-") || strings.Contains(first.headers.Get("User-Agent"), "injected-") {
		t.Fatal("input lost owned request boundaries")
	}
	if err := first.owner.host.RecordShellTelemetry(claudefeatures.ShellTelemetryEvent{
		Name: "tengu_powershell_tool_command_executed", Model: first.model, PromptID: "prompt-owned-shell",
		Metadata: json.RawMessage(`{"command_type":"cmdlet_get","stdout_length":37,"stderr_length":0,"exit_code":0,"interrupted":false,"powershell_edition":"desktop","destructive_category":"none","destructive_target_scope":"none","permission_mode":"default"}`),
	}); err != nil {
		t.Fatalf("remote query shell telemetry observer was not installed: %v", err)
	}
	waitEvent("assistant")
	// A transport duplicate sequence is not enough to drop a new UUID.
	send(1, firstPayload)
	send(1, `{"type":"control_request","request_id":"switch","request":{"subtype":"set_model","model":"claude-sonnet-5","system_prompt":"synthetic persisted remote system"}}`)
	control := waitEvent("control_response")
	if gjson.GetBytes(control, "response.subtype").String() != "success" {
		t.Fatal("model control", string(control))
	}
	// Read through a fresh protected store after the actual bridge control
	// acknowledgment. An actor-local assignment cannot satisfy this check.
	catalogKey := sha256.Sum256([]byte("desktop-session-record-catalog-v1"))
	saved, revision, err := helps.NewClaudeDesktopSessionRecordStore(inner.desktopDurableStatePath, "").Load(hex.EncodeToString(catalogKey[:]))
	if err != nil || revision == "" {
		t.Fatal("remote control did not persist", err)
	}
	var found bool
	for _, record := range gjson.GetBytes(saved, "records").Array() {
		if record.Get("id").String() != value.ID {
			continue
		}
		found = true
		settings := record.Get("remote_control_spawn")
		if settings.Get("model").String() != "claude-sonnet-5" || settings.Get("defaultModel").String() != "claude-opus-5" || settings.Get("systemPrompt").String() != "synthetic persisted remote system" {
			t.Fatal("bridge control acknowledged an uncommitted configuration")
		}
	}
	if !found {
		t.Fatal("remote record missing from protected store")
	}
	send(2, `{"type":"user","uuid":"two","message":{"role":"user","content":"second remote input"}}`)
	second := awaitRemoteExecutor(t, requests)
	if second.model != "claude-sonnet-5" || second.owner.host != first.owner.host || !strings.Contains(string(second.body), "synthetic response") || !strings.Contains(string(second.body), "second remote input") || !strings.Contains(string(second.body), "synthetic persisted remote system") {
		t.Fatalf("next turn: model=%s same_host=%t history=%t input=%t messages=%s", second.model, second.owner.host == first.owner.host, strings.Contains(string(second.body), "synthetic response"), strings.Contains(string(second.body), "second remote input"), gjson.GetBytes(second.body, "messages").Raw)
	}
	waitEvent("assistant")
	deadline := time.After(10 * time.Second)
	for inner.desktopControlPlane.Status().InboundCompleted != 2 {
		select {
		case <-deadline:
			t.Fatal("model completions not observed", inner.desktopControlPlane.Status())
		case <-time.After(time.Millisecond):
		}
	}
	if status := inner.desktopControlPlane.Status(); status.InboundDispatched != 2 || status.InboundExecutionFailed != 0 || status.InboundFailed != 0 {
		t.Fatal("wrong input observations", status)
	}
	if len(requests) != 0 {
		t.Fatal("duplicate input reached inference")
	}
	if err := inner.desktopPrompts.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	var seeded bool
	_, err = transcript.ReadTranscript(transcriptScope, func(lines []byte) error {
		for _, line := range strings.Split(strings.TrimSuffix(string(lines), "\n"), "\n") {
			switch gjson.Get(line, "type").String() {
			case "bridge-session":
				row, err := claudeprompt.ParseSDKBridgeTranscriptRecord([]byte(line))
				if err != nil || row.SessionID != value.SDKSessionID || row.BridgeSessionID != "cse_remoteOne" || row.LastSequenceNum != 0 {
					return errors.New("remote first input lost the exact attachment seed")
				}
				seeded = true
			case "user", "assistant":
				if !seeded {
					return errors.New("remote message preceded its attachment seed")
				}
			}
		}
		return nil
	})
	if err != nil || !seeded {
		t.Fatal("actual worker input did not persist native bridge metadata", err)
	}
	send(3, `{"type":"user","uuid":"three","message":{"role":"user","content":"interrupt remote input"}}`)
	third := awaitRemoteExecutor(t, requests)
	send(4, `{"type":"control_request","request_id":"interrupt","request":{"subtype":"interrupt"}}`)
	control = waitEvent("control_response")
	if gjson.GetBytes(control, "response.subtype").String() != "success" {
		t.Fatal("interrupt control", string(control))
	}
	send(5, `{"type":"user","uuid":"four","message":{"role":"user","content":"after interrupt"}}`)
	fourth := awaitRemoteExecutor(t, requests)
	if third.owner.host != fourth.owner.host || fourth.owner.host.Context().Err() != nil {
		t.Fatal("interrupt retired the query instead of its turn")
	}
	waitEvent("assistant")
	deadline = time.After(10 * time.Second)
	for inner.desktopControlPlane.Status().InboundCompleted != 3 {
		select {
		case <-deadline:
			t.Fatal("post-interrupt completion absent", inner.desktopControlPlane.Status())
		case <-time.After(time.Millisecond):
		}
	}
	if status := inner.desktopControlPlane.Status(); status.InboundCanceled != 1 || status.InboundExecutionFailed != 0 {
		t.Fatal("interrupt was hidden or treated as failure", status)
	}
	if _, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: "stale"}); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("stale stop", err)
	}
	if _, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); err != nil {
		t.Fatal("stop", err)
	}
	if first.owner.host.Context().Err() == nil {
		t.Fatal("stop did not retire exact query")
	}
	if err := first.owner.host.RecordShellTelemetry(claudefeatures.ShellTelemetryEvent{
		Name: "tengu_powershell_tool_command_executed", Model: first.model, Metadata: json.RawMessage(`{"exit_code":0}`),
	}); err == nil {
		t.Fatal("retired remote query still accepted shell telemetry")
	}
	if _, err := e.AttachDesktopRemoteSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("stopped generation revived", err)
	}
}

type remoteStopWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *remoteStopWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestClaudeDesktopRemoteStopJoinsOutcomeOutsideAdmissionLocks(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			runtime := accountRuntimeForAuth(t, e, auths[0].ID)
			inner := runtime.executor
			inner.desktopControlPlane.Close()
			inner.desktopControlPlane = nil
			inner.desktopTelemetry.Close()
			inner.desktopTelemetry = nil
			grant, value, err := inner.desktopATIS.desktopRecords.CreateRemote(t.Context(), runtime.recordOwner, "cse_stop", `C:\synthetic`, "claude-opus-5", func(scope string) (*claudefeatures.Host, error) {
				return inner.desktopATIS.featureHosts.Main(scope, "")
			})
			if err != nil {
				t.Fatal(err)
			}
			_, host, _, _, _, _ := grant.Read()
			entered, outcomeEntered, release := make(chan struct{}), make(chan error, 1), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			input := helps.NewClaudeDesktopRemoteInput(host.Context(), helps.ClaudeDesktopRemoteInputConfiguration{Model: "claude-opus-5"},
				func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				}, nil, nil, func(err error) { outcomeEntered <- err; <-release })
			if err := grant.BindInputDone(input.Done()); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			runtime.remoteInputs = map[string]*helps.ClaudeDesktopRemoteInput{host.ID(): input}
			runtime.mu.Unlock()
			input.Start()
			if err := input.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"owned input"}}`)); err != nil {
				t.Fatal(err)
			}
			awaitRemoteExecutor(t, entered)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiting := &remoteStopWaitContext{Context: ctx, waiting: make(chan struct{})}
			operation := cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}
			result := make(chan error, 1)
			go func() { _, err := e.StopDesktopSession(waiting, auths[0].ID, operation); result <- err }()
			if err := awaitRemoteExecutor(t, outcomeEntered); !errors.Is(err, context.Canceled) {
				t.Fatal("stop changed a canceled input into success", err)
			}
			awaitRemoteExecutor(t, waiting.waiting)
			listed := make(chan error, 1)
			go func() { _, err := e.ListDesktopSessions(auths[0].ID); listed <- err }()
			if err := awaitRemoteExecutor(t, listed); err != nil {
				t.Fatal("join held a record/runtime admission lock", err)
			}
			select {
			case err := <-result:
				t.Fatal("stop returned before its final callback", err)
			default:
			}
			if canceled {
				cancel()
				if err := awaitRemoteExecutor(t, result); !errors.Is(err, context.Canceled) {
					t.Fatal("canceled stop wait falsely succeeded", err)
				}
				select {
				case <-input.Done():
					t.Fatal("request cancellation skipped the final callback")
				default:
				}
			}
			releaseOnce.Do(func() { close(release) })
			awaitRemoteExecutor(t, input.Done())
			if !canceled {
				if err := awaitRemoteExecutor(t, result); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.StopDesktopSession(t.Context(), auths[0].ID, operation); err != nil {
				t.Fatal("repeat stop lost completed retirement", err)
			}
		})
	}
}

func TestClaudeDesktopRemoteRuntimeRetirementCancelsOwnedActors(t *testing.T) {
	// Exercise both public account dispositions with a real owned actor, not a
	// timeout substitute. The actor does not hold an active runtime admission.
	for _, quarantine := range []bool{false, true} {
		t.Run(fmt.Sprint(quarantine), func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			runtime := accountRuntimeForAuth(t, e, auths[0].ID)
			entered := make(chan struct{})
			input := helps.NewClaudeDesktopRemoteInput(t.Context(), helps.ClaudeDesktopRemoteInputConfiguration{Model: "claude-opus-5"}, func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}, nil, nil, nil)
			runtime.remoteInputs = map[string]*helps.ClaudeDesktopRemoteInput{"query": input}
			input.Start()
			if err := input.Enqueue(t.Context(), json.RawMessage(`{"type":"user","message":{"role":"user","content":"cancel this input"}}`)); err != nil {
				t.Fatal(err)
			}
			awaitRemoteExecutor(t, entered)
			if quarantine {
				e.QuarantineAuth(auths[0].ID)
			} else {
				e.CloseAuth(auths[0].ID)
			}
			awaitRemoteExecutor(t, input.Done())
			if !runtime.isClosed() {
				t.Fatal("runtime did not retire")
			}
		})
	}
}
