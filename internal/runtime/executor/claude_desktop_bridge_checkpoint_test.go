package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type bridgeTranscriptFailingStore struct {
	*helps.ClaudeDesktopTranscriptStore
	fail     atomic.Bool
	attempts atomic.Int32
}

func (s *bridgeTranscriptFailingStore) AppendTranscript(scope, revision string, lines []byte) (string, error) {
	if s.fail.Load() && bytes.Contains(lines, []byte(`"type":"bridge-session"`)) {
		s.attempts.Add(1)
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	return s.ClaudeDesktopTranscriptStore.AppendTranscript(scope, revision, lines)
}

func TestClaudeDesktopActualBridgeTranscriptShutdownFailurePersistsDiagnostics(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	inner.desktopControlPlane.Close()
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
		StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				body := `{}`
				switch {
				case request.URL.Path == "/v1/code/sessions":
					body = `{"session":{"id":"cse_shutdown_failure"}}`
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic"}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		},
	})
	if err := inner.desktopPrompts.Close(); err != nil {
		t.Fatal(err)
	}
	root := inner.desktopDurableStatePath
	transcript := &bridgeTranscriptFailingStore{ClaudeDesktopTranscriptStore: helps.NewClaudeDesktopTranscriptStore(root, e.cfg.ProxyURL)}
	inner.desktopPrompts = claudeprompt.NewTracker(helps.NewClaudeDesktopSDKSessionStore(root, e.cfg.ProxyURL), claudeprompt.SDKNativeContentOptions{
		Store: helps.NewClaudeDesktopNativeContentStore(root, e.cfg.ProxyURL), TranscriptStore: transcript,
		Version: inner.desktopProfile.CodeVersion, Entrypoint: "claude-desktop", Cwd: inner.desktopProfile.Environment.DefaultWorkingDir,
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, "execute", nil, desktopExecutionMetadata("bridge-close-failure")); err != nil {
		t.Fatal(err)
	}
	if err := inner.desktopPrompts.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	if status := e.AccountStatus(auth.ID).ControlPlane; status.BridgeTranscriptFailed != 0 || status.InitializationFailed != 0 {
		t.Fatal("healthy attachment unexpectedly failed", status)
	}
	transcript.fail.Store(true)
	e.Close()
	// This reader has no live runtime and can only obtain the diagnostic from
	// the real account lifecycle file written after final transcript draining.
	status := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auth.ID)
	if transcript.attempts.Load() != 1 || status.ControlPlane.BridgeTranscriptFailed != 1 || status.ControlPlane.Failed != 0 || status.RuntimeLoaded || status.RuntimeStopping {
		t.Fatal("final transcript failure was lost or mislabeled as retirement failure", transcript.attempts.Load(), status)
	}
	if sibling := (&ClaudeAccountExecutor{stateRoot: e.stateRoot}).AccountStatus(auths[1].ID); sibling.ControlPlane.BridgeTranscriptFailed != 0 {
		t.Fatal("transcript failure crossed account ownership")
	}
}

// Enter through the public executor, consume the real worker SSE parser, stop
// through management's controller, reconstruct the account from protected disk,
// and inspect the next real worker request. No old query file is transferred.
func TestClaudeDesktopActualBridgeRecordRestoresAcrossAccountReconstruction(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		t.Run(mode, func(t *testing.T) {
			e, auths := newExecutionSessionAccountTest(t)
			auth := auths[0]
			auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
			var creates, unarchives, bridges atomic.Int32
			type stream struct {
				request *http.Request
				writer  *io.PipeWriter
			}
			streams := make(chan stream, 8)
			heartbeats := make(chan struct{}, 8)
			install := func(provider *ClaudeAccountExecutor) {
				inner := accountRuntimeForAuth(t, provider, auth.ID).executor
				inner.desktopControlPlane.Close()
				inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{
					StatePath: t.TempDir(), Bundle: inner.desktopProfile,
					DoerFactory: func(_ context.Context, _ string, current *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
						if current.ID != auth.ID {
							return nil, errors.New("foreign account control request")
						}
						return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
							body := `{}`
							switch {
							case request.URL.Path == "/v1/code/sessions":
								creates.Add(1)
								body = `{"session":{"id":"cse_record_cursor"}}`
							case strings.HasSuffix(request.URL.Path, "/unarchive"):
								unarchives.Add(1)
							case strings.HasSuffix(request.URL.Path, "/bridge"):
								epoch := bridges.Add(1)
								body = fmt.Sprintf(`{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"%d","worker_jwt":"worker-%d"}`, epoch, epoch)
							case strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
								reader, writer := io.Pipe()
								streams <- stream{request, writer}
								return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &remoteExecutorPipe{reader, writer}}, nil
							case strings.HasSuffix(request.URL.Path, "/worker/heartbeat"):
								heartbeats <- struct{}{}
								body = `{"heartbeat_interval_seconds":20}`
							}
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
						}), nil
					},
				})
			}
			install(e)
			header := http.Header{"X-Session-Id": {uuid.NewString()}}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				return executionSessionTestResponse(t, request), nil
			})))
			invoke := func(provider *ClaudeAccountExecutor) {
				t.Helper()
				entry := ctx
				if strings.HasPrefix(mode, "http") {
					var err error
					entry, err = provider.executionSessions.Bind(entry, desktopExecutionMetadata("bridge-record"))
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := invokeQueryLifetimeEntry(entry, provider, auth, header, mode, nil, desktopExecutionMetadata("bridge-record")); err != nil {
					t.Fatal(err)
				}
			}
			invoke(e)
			first := awaitRemoteExecutor(t, streams)
			if first.request.URL.Query().Has("from_sequence_num") || first.request.Header.Get("Last-Event-ID") != "" {
				t.Fatal("new bridge invented a saved cursor")
			}
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(first.writer, "id: 41\nevent: server_event\ndata: {}\n\nid: 17\nevent: server_event\ndata: {}\n\nid: 900\nevent: ephemeral_event\ndata: {\"event_type\":\"heartbeat_probe\"}\n\n")
				written <- err
			}()
			if err := awaitRemoteExecutor(t, written); err != nil {
				t.Fatal(err)
			}
			awaitRemoteExecutor(t, heartbeats)
			list, err := e.ListDesktopSessions(auth.ID)
			if err != nil || len(list) != 1 {
				t.Fatal(list, err)
			}
			before := list[0]
			if _, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: before.ID, ExpectedQueryID: before.QueryID}); err != nil {
				t.Fatal(err)
			}
			firstRuntime := accountRuntimeForAuth(t, e, auth.ID).executor
			scope, err := json.Marshal([]string{auth.ID, firstRuntime.desktopProfile.ProfileID, auth.ProxyURL})
			if err != nil {
				t.Fatal(err)
			}
			resume, err := firstRuntime.desktopPrompts.RestoreNativeContent(string(scope), before.SDKSessionID)
			if err != nil || resume.TranscriptBridge == nil || resume.TranscriptBridge.LastSequenceNum != 41 ||
				resume.TranscriptBridge.BridgeSessionID != "cse_record_cursor" || resume.TranscriptBridge.SessionID != before.SDKSessionID {
				t.Fatal("actual Stop did not persist the native bridge transcript row", err)
			}
			e.Close()
			restored := newClaudeAccountTestExecutor(e.cfg)
			t.Cleanup(restored.Close)
			prepareExecutionSessionAccountTest(t, restored, auths)
			install(restored)
			invoke(restored)
			second := awaitRemoteExecutor(t, streams)
			if second.request.URL.Path != first.request.URL.Path || second.request.URL.Query().Get("from_sequence_num") != "41" || second.request.Header.Get("Last-Event-ID") != "41" {
				t.Fatal("new query failed to use the record-owned remote ID and exact cursor", second.request.URL)
			}
			if second.request.Header.Get("Authorization") != "Bearer worker-2" || creates.Load() != 1 || unarchives.Load() != 1 || bridges.Load() != 2 {
				t.Fatal("restoration reminted a session or reused the old query credential", creates.Load(), unarchives.Load(), bridges.Load())
			}
			list, err = restored.ListDesktopSessions(auth.ID)
			if err != nil || len(list) != 1 || list[0].ID != before.ID || list[0].SDKSessionID != before.SDKSessionID || list[0].QueryID == before.QueryID {
				t.Fatal("record, transcript and query ownership were conflated", list, err)
			}
			// Close must join final bridge production before sealing transcript
			// writes. A third account runtime reads disk without a model call.
			go func() {
				_, err := io.WriteString(second.writer, "id: 88\nevent: server_event\ndata: {}\n\nid: 999\nevent: ephemeral_event\ndata: {\"event_type\":\"heartbeat_probe\"}\n\n")
				written <- err
			}()
			if err := awaitRemoteExecutor(t, written); err != nil {
				t.Fatal(err)
			}
			awaitRemoteExecutor(t, heartbeats)
			restored.Close()
			reader := newClaudeAccountTestExecutor(e.cfg)
			t.Cleanup(reader.Close)
			prepareExecutionSessionAccountTest(t, reader, auths)
			last := accountRuntimeForAuth(t, reader, auth.ID).executor
			resume, err = last.desktopPrompts.RestoreNativeContent(string(scope), before.SDKSessionID)
			if err != nil || resume.TranscriptBridge == nil || resume.TranscriptBridge.LastSequenceNum != 88 ||
				resume.TranscriptBridge.BridgeSessionID != "cse_record_cursor" {
				t.Fatal("account close sealed the transcript before its final bridge checkpoint", err)
			}
		})
	}
}
