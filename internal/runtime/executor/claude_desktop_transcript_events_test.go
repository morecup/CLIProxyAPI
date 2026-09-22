package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopRemoteChainDiagnosticsActualAttachment(t *testing.T) {
	for _, mode := range []string{"timestamp", "parallel", "plain", "sampled-out", "disabled", "adoption-failed"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var emitted []json.RawMessage
			var expectedAuthID string
			e, auths := newExecutionSessionAccountTest(t, func(_ string, role string, auth *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
				return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					if strings.Contains(string(body), "tengu_chain_") {
						if role != "sdk-event-logging" || auth.ID != expectedAuthID {
							t.Error("chain telemetry crossed delivery/account ownership")
						}
						mu.Lock()
						for _, event := range gjson.GetBytes(body, "events").Array() {
							if strings.HasPrefix(event.Get("event_data.event_name").String(), "tengu_chain_") {
								emitted = append(emitted, json.RawMessage(event.Get("event_data").Raw))
							}
						}
						mu.Unlock()
					}
					response := &http.Response{StatusCode: 204, Proto: "HTTP/2.0", ProtoMajor: 2, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
					if role == "sdk-event-logging" || role == "datadog-logs" {
						response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
					}
					return response, nil
				})
			})
			auth := auths[0]
			expectedAuthID = auth.ID
			auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(e)
			e.credentialManager = manager
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}, {ID: "claude-sonnet-5"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			inner := accountRuntimeForAuth(t, e, auth.ID).executor
			inner.desktopATIS.startFeatureHost = func(host *claudefeatures.Host) error {
				ticket, sink, err := inner.desktopATIS.bindFeatureHost(auth, host)
				if err != nil {
					return err
				}
				rate, disabled := 1, false
				if mode == "sampled-out" {
					rate = 0
				}
				if mode == "disabled" {
					disabled = true
				}
				body := fmt.Sprintf(`{"features":{"tengu_event_sampling_config":{"value":{"tengu_chain_timestamp_fallback":{"sample_rate":%d}}},"tengu_frond_boric":{"value":{"firstParty":%t}}}}`, rate, disabled)
				_, err = host.Service().Observe(ticket, []byte(body), host.SessionID(), sink)
				return err
			}
			inner.desktopControlPlane.Close()
			writers := make(chan *io.PipeWriter, 1)
			inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile,
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
					return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
						body, status := `{}`, 200
						switch {
						case strings.HasSuffix(request.URL.Path, "/unarchive"):
							status = 409
						case strings.HasSuffix(request.URL.Path, "/bridge"):
							body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
						case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/worker"):
							body = `{"worker":{"external_metadata":{"model":"claude-sonnet-5","system_prompt":"FOREIGN_SYSTEM"}}}`
						case strings.HasSuffix(request.URL.Path, "/worker/internal-events"):
							body = `{"data":[]}`
							if request.URL.Query().Get("subagents") != "true" {
								list, err := e.ListDesktopSessions(auth.ID)
								if err != nil || len(list) != 1 {
									return nil, fmt.Errorf("test lost query")
								}
								user, assistant, missing := uuid.NewString(), uuid.NewString(), uuid.NewString()
								parent := user
								if mode != "plain" && mode != "parallel" {
									parent = missing
								}
								stop := "end_turn"
								if mode == "adoption-failed" {
									stop = "tool_use"
								}
								rows := []claudeprompt.SDKNativeMessage{
									{Type: "user", UUID: user, SessionID: list[0].SDKSessionID, Timestamp: "2026-09-07T10:00:00.000Z", Message: json.RawMessage(`{"role":"user","content":"PRIVATE_REMOTE_USER"}`)},
									{Type: "assistant", UUID: assistant, ParentUUID: &parent, SessionID: list[0].SDKSessionID, Timestamp: "2026-09-07T10:00:01.000Z", Message: json.RawMessage(fmt.Sprintf(`{"id":"msg_remote_chain","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"PRIVATE_REMOTE_REPLY"}],"stop_reason":%q,"usage":{"input_tokens":5,"output_tokens":2}}`, stop))},
								}
								if mode == "parallel" {
									sibling := rows[1]
									sibling.UUID = uuid.NewString()
									sibling.Timestamp = "2026-09-07T10:00:00.500Z"
									sibling.Message = json.RawMessage(strings.ReplaceAll(string(sibling.Message), "PRIVATE_REMOTE_REPLY", "PRIVATE_PARALLEL_REPLY"))
									rows = []claudeprompt.SDKNativeMessage{rows[0], sibling, rows[1]}
								}
								events := make([]map[string]any, 0, len(rows))
								for _, row := range rows {
									events = append(events, map[string]any{"event_id": row.UUID, "payload": row})
								}
								encoded, _ := json.Marshal(map[string]any{"data": events})
								body = string(encoded)
							}
						case strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
							reader, writer := io.Pipe()
							writers <- writer
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &remoteExecutorPipe{reader, writer}}, nil
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
					}), nil
				}})
			requests := make(chan []byte, 1)
			var calls atomic.Int32
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/messages" {
					return executionSessionTestResponse(t, request), nil
				}
				reader, err := request.GetBody()
				if err != nil {
					return nil, err
				}
				body, err := io.ReadAll(reader)
				_ = reader.Close()
				if err != nil {
					return nil, err
				}
				calls.Add(1)
				requests <- body
				payload := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_local_chain\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"
				payload += "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"LOCAL_REPLY\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req-local-chain"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
			})))
			started := time.Now()
			value, err := e.StartDesktopRemoteSession(ctx, auth.ID, cliproxyexecutor.ClaudeDesktopRemoteStart{RemoteSessionID: "cse_chain", Folder: `C:\code`, Model: "claude-opus-5"})
			if (err != nil) != (mode == "adoption-failed") {
				t.Fatal("unexpected adoption result", err)
			}
			if calls.Load() != 0 {
				t.Fatal("restoration invented model request")
			}
			if err := inner.desktopTelemetry.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			observed := append([]json.RawMessage(nil), emitted...)
			mu.Unlock()
			want := 1
			if mode == "plain" || mode == "sampled-out" || mode == "disabled" {
				want = 0
			}
			if len(observed) != want {
				t.Fatal("actual reconstruction lost or fabricated event", len(observed), want)
			}
			if want == 1 {
				data := gjson.ParseBytes(observed[0])
				name := claudeprompt.SDKChainTimestamp
				if mode == "parallel" {
					name = claudeprompt.SDKChainParallelResult
				}
				if data.Get("event_name").String() != name || data.Get("session_id").String() != value.SDKSessionID || data.Get("model").String() != "claude-sonnet-5" {
					t.Fatal("event used pre-restoration or foreign dimensions")
				}
				at, parseErr := time.Parse(time.RFC3339Nano, data.Get("client_timestamp").String())
				if parseErr != nil || at.Before(started.Truncate(time.Millisecond)) || at.After(time.Now()) {
					t.Fatal("event timestamp came from historical transcript")
				}
				meta, decodeErr := base64.StdEncoding.DecodeString(data.Get("additional_metadata").String())
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if gjson.GetBytes(meta, "cc_prompt_id").Exists() || strings.Contains(string(meta), "PRIVATE_") || strings.Contains(string(meta), "FOREIGN_") {
					t.Fatal("transcript content or invented prompt leaked")
				}
				if mode == "parallel" && gjson.GetBytes(meta, "recovered_count").Int() != 1 {
					t.Fatal("recovery count not taken from traversal")
				}
			}
			if mode == "adoption-failed" {
				return
			}
			writer := awaitRemoteExecutor(t, writers)
			sent := make(chan error, 1)
			go func() {
				_, err := fmt.Fprint(writer, "id: 1\nevent: client_event\ndata: {\"event_id\":\"chain-input\",\"event_type\":\"user\",\"payload\":{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"LOCAL_INPUT\"}}}\n\n")
				sent <- err
			}()
			if err := awaitRemoteExecutor(t, sent); err != nil {
				t.Fatal(err)
			}
			body := awaitRemoteExecutor(t, requests)
			if gjson.GetBytes(body, "model").String() != "claude-sonnet-5" || !strings.Contains(string(body), "PRIVATE_REMOTE_REPLY") || !strings.Contains(string(body), "PRIVATE_REMOTE_USER") || strings.Contains(string(body), "FOREIGN_SYSTEM") {
				t.Fatal("actual Messages did not consume restored chain and owned model")
			}
			if mode == "parallel" && !strings.Contains(string(body), "PRIVATE_PARALLEL_REPLY") {
				t.Fatal("parallel repair event without actual repaired Messages")
			}
			deadline := time.After(10 * time.Second)
			for inner.desktopControlPlane.Status().InboundCompleted != 1 {
				select {
				case <-deadline:
					t.Fatal("inbound did not complete")
				case <-time.After(time.Millisecond):
				}
			}
			if _, err := e.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
