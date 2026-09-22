package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func sdkSessionSyntheticSignature() string {
	// A synthetic provider-recognizable envelope, not a captured signature.
	channel := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 12)
	channel = protowire.AppendString(protowire.AppendTag(channel, 6, protowire.BytesType), "claude-opus-5")
	container := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), channel)
	payload := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), container)
	payload = protowire.AppendVarint(protowire.AppendTag(payload, 3, protowire.VarintType), 1)
	return base64.StdEncoding.EncodeToString(payload)
}

func sdkSessionContentResponse(t *testing.T, content string, streaming bool) string {
	t.Helper()
	if !streaming {
		return `{"id":"msg_state_3","type":"message","role":"assistant","model":"claude-opus-5","content":` + content + `,"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":2}}`
	}
	var result strings.Builder
	result.WriteString("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_state_3\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(content), &blocks); err != nil {
		t.Fatal(err)
	}
	for index, block := range blocks {
		fmt.Fprintf(&result, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\n", index, block)
		kind := gjson.GetBytes(block, "type").String()
		var deltas []map[string]any
		if kind == "tool_use" {
			deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": gjson.GetBytes(block, "input").Raw})
		}
		if kind == "thinking" {
			deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": gjson.GetBytes(block, "thinking").String()}, map[string]any{"type": "signature_delta", "signature": gjson.GetBytes(block, "signature").String()})
		}
		for _, delta := range deltas {
			raw, _ := json.Marshal(delta)
			fmt.Fprintf(&result, "data: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":%s}\n\n", index, raw)
		}
		fmt.Fprintf(&result, "data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
	}
	result.WriteString("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	return result.String()
}

func TestClaudeAccountSDKSessionStateDefaultRuntimeAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, scenario := range []string{"history", "reactive", "corrupt", "native-corrupt", "transcript-corrupt", "transcript-content-mismatch", "transcript-late-failure", "tool", "tool-reactive", "mixed-reactive"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				mixed := scenario == "mixed-reactive"
				toolMode := scenario == "tool" || scenario == "tool-reactive" || mixed
				recoverMode := scenario == "reactive" || strings.HasSuffix(scenario, "-reactive")
				auth := newClaudeAccountRuntimeTestAuth(t, uuid.NewString(), uuid.NewString(), uuid.NewString())
				prepareH73RuntimeAuth(auth)
				cfg := &config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}}
				account := newClaudeAccountTestExecutor(cfg)
				t.Cleanup(func() { account.Close() })
				if err := account.Provision(auth); err != nil {
					t.Fatal(err)
				}
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				session := ""
				scope, _ := json.Marshal([]string{auth.ID, account.bundle.ProfileID, auth.ProxyURL})
				var prior claudeprompt.SDKHistorySnapshot
				const toolBlock = `{"type":"tool_use","id":"tool_state_owned","name":"Read","input":{"file_path":"C:/synthetic/not-opened"}}`
				toolContent := `[` + toolBlock + `]`
				if mixed {
					toolContent = `[{"type":"thinking","thinking":"synthetic persisted thought","signature":"` + sdkSessionSyntheticSignature() + `"},` + toolBlock + `]`
					fixture := []byte(`{"messages":[{"role":"assistant","content":` + toolContent + `}]}`)
					if sanitized := sanitizeClaudeMessagesForClaudeUpstreamWithDebug(t.Context(), fixture, "claude-opus-5"); !bytes.Equal(fixture, sanitized) {
						t.Fatal("synthetic fixture is not a stable provider-recognized signature")
					}
				}
				mainCalls, summaryCalls := 0, 0
				var transcriptPath, transcriptScope string
				var corrupt []byte
				restoredBeforeSend := false
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					var body []byte
					if request.Body != nil {
						var err error
						body, err = io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
					}
					status, reply := 200, `{}`
					if request.URL.Path == "/v1/messages/count_tokens" {
						reply = `{"input_tokens":10}`
					}
					responseHeaders := http.Header{}
					if request.URL.Path == "/v1/messages" {
						observeDefaultSDKSession(t, request, &session)
						runtime := accountRuntimeForAuth(t, account, auth.ID).executor
						if runtime.classifyClaudeDesktopRequestRole(body) == claudeprofile.RoleCompaction {
							summaryCalls++
							reply = reactiveSummaryStream("msg_state_compact", []string{"<summary>synthetic restored state summary</summary>"})
							responseHeaders.Set("Content-Type", "text/event-stream")
							responseHeaders.Set("Request-Id", "req_state_compact")
							if mixed && summaryCalls == 1 {
								if !strings.Contains(string(body), "synthetic-media-payload") {
									t.Error("first summary lost the observed media")
								}
								status, reply = 400, `{"type":"error","error":{"type":"invalid_request_error","message":"image exceeds maximum size"}}`
								responseHeaders.Set("Content-Type", "application/json")
							} else if mixed && (!strings.Contains(string(body), "[image]") || strings.Contains(string(body), "synthetic-media-payload")) {
								t.Error("media retry did not send native placeholders")
							}
						} else {
							mainCalls++
							if mainCalls == 4 {
								actual := runtime.desktopPrompts.SDKHistory(string(scope), session)
								if scenario != "corrupt" {
									restoredBeforeSend = actual.OwnedMessagesKnown && len(actual.Messages) == len(prior.Messages)+1 && reflect.DeepEqual(actual.Messages[:len(prior.Messages)], prior.Messages)
									if !restoredBeforeSend {
										t.Errorf("default runtime did not restore owned UUIDs before sending: reason=%s prior=%d actual=%d wire_thinking=%d", actual.IncompleteReason, len(prior.Messages), len(actual.Messages), len(gjson.GetBytes(body, `messages.#(role=="assistant")#.content.#(type=="thinking")#`).Array()))
									}
								} else if actual.OwnedMessagesKnown {
									t.Error("corrupt state became known native history")
								}
								if scenario == "transcript-late-failure" {
									// Corrupt the synthetic log after the default writer loaded
									// its index, so the asynchronous append must report failure.
									corrupt = []byte("synthetic corrupt transcript after load")
									if err := os.WriteFile(transcriptPath, corrupt, 0600); err != nil {
										t.Error(err)
									}
								}
							}
							responseHeaders.Set("Request-Id", fmt.Sprintf("req_state_%d", mainCalls))
							if mainCalls == 4 && recoverMode {
								status = 400
								reply = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
							} else {
								text := fmt.Sprintf("synthetic persisted reply %d", mainCalls)
								reply = fmt.Sprintf(`{"id":"msg_state_%d","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, mainCalls, text)
								if gjson.GetBytes(body, "stream").Bool() {
									reply = reactiveSummaryStream(fmt.Sprintf("msg_state_%d", mainCalls), []string{text})
									responseHeaders.Set("Content-Type", "text/event-stream")
								}
								if mainCalls == 3 && toolMode {
									reply = sdkSessionContentResponse(t, toolContent, gjson.GetBytes(body, "stream").Bool())
								}
							}
						}
					}
					return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				var messages []map[string]any
				invoke := func() error {
					body, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": messages, "tools": []any{}, "stream": mode == "http-stream"})
					opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
					request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
					switch mode {
					case "execute":
						_, err := account.Execute(ctx, auth, request, opts)
						return err
					case "stream":
						stream, err := account.ExecuteStream(ctx, auth, request, opts)
						if err != nil {
							return err
						}
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
						return err
					default:
						raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
						if err != nil {
							return err
						}
						raw.Header = headers.Clone()
						response, err := account.HttpRequest(ctx, auth, raw)
						if err != nil {
							return err
						}
						payload, readErr := io.ReadAll(response.Body)
						closeErr := response.Body.Close()
						if readErr != nil {
							return readErr
						}
						if closeErr != nil {
							return closeErr
						}
						if response.StatusCode != 200 {
							return fmt.Errorf("synthetic API failed: %d (%d bytes)", response.StatusCode, len(payload))
						}
						return nil
					}
				}
				for index := 1; index <= 3; index++ {
					messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("synthetic persisted input %d", index)})
					if index == 1 && mixed {
						messages[len(messages)-1]["content"] = json.RawMessage(`[{"type":"text","text":"synthetic persisted input 1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"synthetic-media-payload"}}]`)
					}
					if err := invoke(); err != nil {
						t.Fatal(err)
					}
					var content any = fmt.Sprintf("synthetic persisted reply %d", index)
					if index == 3 && toolMode {
						content = json.RawMessage(toolContent)
					}
					messages = append(messages, map[string]any{"role": "assistant", "content": content})
				}
				firstRuntime := accountRuntimeForAuth(t, account, auth.ID)
				prior = firstRuntime.executor.desktopPrompts.SDKHistory(string(scope), session)
				if !prior.OwnedMessagesKnown || len(prior.Messages) < 6 || (!mixed && len(prior.Messages) != 6) {
					t.Fatal("initial runtime did not own history", prior)
				}
				nativeBefore := firstRuntime.executor.desktopPrompts.NativeContent(string(scope), session)
				assertDefaultNativeContent(t, nativeBefore, prior, session)
				if !bytes.Contains(nativeBefore.Messages[0].Message, []byte("synthetic persisted input 1")) ||
					!bytes.Contains(nativeBefore.Messages[1].Message, []byte("synthetic persisted reply 1")) {
					t.Fatal("default runtime did not own actual input and response content")
				}
				if mixed && (!bytes.Contains(nativeBefore.Messages[0].Message, []byte("synthetic-media-payload")) ||
					!bytes.Contains(nativeBefore.Messages[5].Message, []byte("synthetic persisted thought"))) {
					t.Fatal("default native content lost observed media or thinking")
				}
				durableStatePath := firstRuntime.executor.desktopDurableStatePath
				nativePaths, err := filepath.Glob(filepath.Join(durableStatePath, "sdk-native-content", "*.json"))
				if err != nil || len(nativePaths) != 1 {
					t.Fatal("default native content store was not installed")
				}
				protectedContent, err := os.ReadFile(nativePaths[0])
				if err != nil || bytes.Contains(protectedContent, []byte("synthetic persisted")) || bytes.Contains(protectedContent, []byte("synthetic-media-payload")) {
					t.Fatal("native content checkpoint was not protected at rest")
				}
				paths, err := filepath.Glob(filepath.Join(durableStatePath, "sdk-sessions", "*.json"))
				if err != nil || len(paths) != 1 {
					t.Fatal("ordinary runtime did not persist the session", paths, err)
				}
				toolPromptID := ""
				if toolMode {
					identity, _ := json.Marshal([]string{string(scope), session})
					digest := sha256.Sum256(identity)
					payload, _, err := helps.NewClaudeDesktopSDKSessionStore(durableStatePath, "").Load(hex.EncodeToString(digest[:]))
					if err != nil {
						t.Fatal(err)
					}
					gjson.GetBytes(payload, "prompts").ForEach(func(_, value gjson.Result) bool {
						if len(value.Get("pending").Map()) > 0 {
							toolPromptID = value.Get("identity.PromptID").String()
						}
						return true
					})
					if toolPromptID == "" {
						t.Fatal("actual tool response did not persist its owner")
					}
				}
				account.Close()
				transcriptPaths, err := filepath.Glob(filepath.Join(durableStatePath, "sdk-transcripts", "*.jsonl.enc"))
				if err != nil || len(transcriptPaths) != 1 {
					t.Fatal("default runtime did not append a protected transcript")
				}
				transcriptPath = transcriptPaths[0]
				transcriptIdentity, _ := json.Marshal([]string{string(scope), session})
				transcriptDigest := sha256.Sum256(transcriptIdentity)
				transcriptScope = hex.EncodeToString(transcriptDigest[:])
				transcriptIndex, err := helps.NewClaudeDesktopTranscriptStore(durableStatePath, "").LoadTranscript(transcriptScope)
				if err != nil || len(transcriptIndex.UUIDs) != len(nativeBefore.Messages) {
					t.Fatal("close did not drain actual owned transcript rows", err)
				}
				for i, row := range nativeBefore.Messages {
					if transcriptIndex.UUIDs[i] != row.UUID {
						t.Fatal("transcript differs from default native UUID ownership")
					}
				}
				transcriptBytes, err := os.ReadFile(transcriptPath)
				if err != nil || bytes.Contains(transcriptBytes, []byte("synthetic persisted")) {
					t.Fatal("transcript exposed message text")
				}
				corruptPath := paths[0]
				if scenario == "native-corrupt" {
					corruptPath = nativePaths[0]
				}
				if scenario == "transcript-corrupt" || scenario == "transcript-late-failure" {
					corruptPath = transcriptPath
				}
				if scenario == "corrupt" || scenario == "native-corrupt" || scenario == "transcript-corrupt" {
					corrupt = []byte("synthetic corrupted checkpoint")
					if err := os.WriteFile(corruptPath, corrupt, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "transcript-content-mismatch" {
					// Keep valid protection and every UUID, while changing only the
					// checkpoint content. The independent transcript must catch it.
					store := helps.NewClaudeDesktopNativeContentStore(durableStatePath, "")
					payload, revision, err := store.Load(transcriptScope)
					if err != nil {
						t.Fatal(err)
					}
					changed := bytes.Replace(payload, []byte("synthetic persisted input 1"), []byte("different protected input 1"), 1)
					if bytes.Equal(changed, payload) {
						t.Fatal("test did not change protected content")
					}
					if _, err := store.Save(transcriptScope, revision, changed); err != nil {
						t.Fatal(err)
					}
					corruptPath = nativePaths[0]
					corrupt, err = os.ReadFile(corruptPath)
					if err != nil {
						t.Fatal(err)
					}
				}
				account = newClaudeAccountTestExecutor(cfg)
				if err := account.Provision(auth); err != nil {
					t.Fatal(err)
				}
				var nextContent any = "synthetic post-restart input"
				if toolMode {
					nextContent = []map[string]any{{"type": "tool_result", "tool_use_id": "tool_state_owned", "content": "synthetic tool output"}}
				}
				messages = append(messages, map[string]any{"role": "user", "content": nextContent})
				if err := invoke(); err != nil {
					t.Fatal("ordinary post-restart request failed", err)
				}
				runtime := accountRuntimeForAuth(t, account, auth.ID)
				if scenario == "transcript-late-failure" {
					deadline := time.Now().Add(5 * time.Second)
					for runtime.executor.desktopPrompts.SDKSessionStateError(string(scope), session) == nil && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
				}
				nativeAfter := runtime.executor.desktopPrompts.NativeContent(string(scope), session)
				if scenario != "corrupt" && scenario != "native-corrupt" && scenario != "transcript-corrupt" && scenario != "transcript-content-mismatch" && scenario != "transcript-late-failure" {
					assertDefaultNativeContent(t, nativeAfter, runtime.executor.desktopPrompts.SDKHistory(string(scope), session), session)
					for index, previous := range nativeBefore.Messages {
						current := nativeAfter.Messages[index]
						if previous.UUID != current.UUID || previous.Timestamp != current.Timestamp || !reflect.DeepEqual(previous.ParentUUID, current.ParentUUID) ||
							previous.RequestID != current.RequestID || previous.PromptID != current.PromptID ||
							gjson.GetBytes(previous.Message, "content").Raw != gjson.GetBytes(current.Message, "content").Raw {
							t.Fatal("restart/compaction changed prior native content or yield identity")
						}
					}
					if toolMode {
						found := false
						for _, row := range nativeAfter.Messages {
							if row.Type == "user" && bytes.Contains(row.Message, []byte("synthetic tool output")) {
								found = row.SourceToolAssistantUUID != "" && row.ParentUUID != nil && *row.ParentUUID == row.SourceToolAssistantUUID
							}
						}
						if !found {
							t.Fatal("ordinary tool result lost native assistant-parent ownership")
						}
					}
				} else if !nativeAfter.PersistenceError {
					t.Fatal("corrupt native/structural checkpoint remained healthy")
				}
				if toolMode {
					owner := runtime.executor.desktopPrompts.BindSDKHelper(claudeprompt.Input{AccountID: string(scope), SessionID: session, ParentPromptID: toolPromptID, StartedAt: time.Now()})
					if owner == nil || owner.SDKSessionStateError() != nil {
						t.Fatal("ordinary tool continuation lost original owner")
					}
					queries, turns, toolCount := 2, 2, 1
					if recoverMode {
						queries, turns, toolCount = 3, 3, 0
					}
					if snapshot := owner.Snapshot(); snapshot.Queries != queries || snapshot.NumTurns != turns || snapshot.ToolResultUserYields != 1 || snapshot.ToolUseCount != toolCount {
						t.Fatal("ordinary tool result opened a new prompt or duplicated accounting", snapshot)
					}
				}
				if recoverMode {
					wantSummaries := 1
					if mixed {
						wantSummaries = 2
					}
					if summaryCalls != wantSummaries || mainCalls != 5 || !restoredBeforeSend {
						t.Fatal("restored history did not feed actual automatic recovery", mainCalls, summaryCalls)
					}
					final := runtime.executor.desktopPrompts.SDKHistory(string(scope), session)
					if !final.OwnedMessagesKnown || len(final.Messages) == 0 || final.Messages[0].Subtype != "compact_boundary" {
						t.Fatal("automatic recovered history was not adopted", final)
					}
				} else if summaryCalls != 0 || mainCalls != 4 {
					t.Fatal("unexpected helper/retry", summaryCalls, mainCalls)
				}
				if scenario == "corrupt" || scenario == "native-corrupt" || scenario == "transcript-corrupt" || scenario == "transcript-content-mismatch" || scenario == "transcript-late-failure" {
					actual, _ := os.ReadFile(corruptPath)
					if !bytes.Equal(actual, corrupt) {
						t.Fatal("corrupt original overwritten")
					}
					visible := make(map[string]bool)
					for _, endpoint := range runtime.executor.desktopTelemetry.Status().DeliveryEndpoints {
						if strings.Contains(endpoint.Reason, "SDK session ownership state") {
							visible[endpoint.Role] = true
						}
					}
					if !visible["sdk-event-logging"] || !visible["datadog-logs"] {
						t.Fatalf("successful request hid state loss: visible_roles=%v", visible)
					}
				} else if !restoredBeforeSend {
					t.Fatal("state restoration was not observed on the real request path")
				}
				account.Close()
				if corrupt == nil {
					index, err := helps.NewClaudeDesktopTranscriptStore(runtime.executor.desktopDurableStatePath, "").LoadTranscript(transcriptScope)
					if err != nil || len(index.UUIDs) != len(nativeAfter.Messages) {
						t.Fatal("restart/recovery lost or duplicated transcript appends", err)
					}
					finalBytes, err := os.ReadFile(transcriptPath)
					if err != nil || !bytes.HasPrefix(finalBytes, transcriptBytes) {
						t.Fatal("restart or compaction rewrote earlier transcript bytes")
					}
				}
			})
		}
	}
}
