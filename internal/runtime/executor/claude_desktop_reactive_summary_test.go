package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func reactiveSummaryStream(id string, texts []string) string {
	result := fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":999,\"output_tokens\":0}}}\n\n", id)
	for index, text := range texts {
		result += fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", index)
		result += fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", index, text)
		result += fmt.Sprintf("data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
	}
	return result + "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":5}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
}

func TestClaudeDesktopReactiveSummaryDispatchThroughRealExecutor(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		for _, retry := range []string{"none", "prompt-too-long", "media", "server-error", "empty-summary"} {
			t.Run(mode+"/"+retry, func(t *testing.T) {
				e := newClaudeDesktopTestExecutor(t)
				e.desktopTelemetry = nil
				auth := newClaudeDesktopRawRequestTestAuth(t)
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				session := helps.ClaudeAgentSessionUUIDForRequest(headers, nil, nil, false)
				seedCalls, summaryCalls := 0, 0
				var summaryRows []int
				var clients []string
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					status, response := 200, `{"input_tokens":1}`
					header := http.Header{}
					if request.URL.Path == "/v1/messages" {
						if e.classifyClaudeDesktopRequestRole(body) == claudeprofile.RoleCompaction {
							summaryCalls++
							summaryRows = append(summaryRows, len(gjson.GetBytes(body, "messages").Array()))
							clients = append(clients, helps.HeaderValueCaseInsensitive(request.Header, "X-Client-Request-Id"))
							if id := gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String(); id != session {
								t.Fatal("summary lost account/session ownership")
							}
							if attempt, ok := cliproxyexecutor.UpstreamAttemptFromContext(request.Context()); !ok || attempt.Number != 1 {
								t.Fatal("summary inherited the main request retry attempt")
							}
							if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "diagnostics").Exists() ||
								gjson.GetBytes(body, "thinking.display").String() != "omitted" || len(gjson.GetBytes(body, "system").Array()) != 4 {
								t.Fatalf("summary profile mismatch: stream=%v diagnostics=%v display=%v system_blocks=%d", gjson.GetBytes(body, "stream").Bool(), gjson.GetBytes(body, "diagnostics").Exists(), gjson.GetBytes(body, "thinking.display").Exists(), len(gjson.GetBytes(body, "system").Array()))
							}
							plan, errPlan := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleCompaction, "claude-opus-5", nil)
							if errPlan != nil || helps.HeaderValueCaseInsensitive(request.Header, "Anthropic-Beta") != plan.anthropicBeta() {
								t.Fatal("summary bypassed its compact header profile")
							}
							if retry == "prompt-too-long" && summaryCalls == 1 {
								status, response = 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
							} else if retry == "media" {
								status, response = 400, `{"type":"error","error":{"type":"invalid_request_error","message":"request_too_large"}}`
							} else if retry == "server-error" {
								status, response = 502, `{"type":"error","error":{"type":"server_error","message":"synthetic error"}}`
							} else {
								texts := []string{"<summary>synthetic selected summary</summary>", "not selected trailing text"}
								if retry == "empty-summary" {
									texts = []string{"  "}
								}
								response = reactiveSummaryStream("msg_summary", texts)
								header.Set("Content-Type", "text/event-stream")
							}
						} else {
							seedCalls++
							text := fmt.Sprintf("synthetic reply %d", seedCalls)
							if gjson.GetBytes(body, "stream").Bool() {
								response = reactiveSummaryStream(fmt.Sprintf("msg_seed_%d", seedCalls), []string{text})
								header.Set("Content-Type", "text/event-stream")
							} else {
								response = fmt.Sprintf(`{"id":"msg_seed_%d","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, seedCalls, text)
							}
						}
						header.Set("Request-Id", fmt.Sprintf("req_seed_%d_summary_%d", seedCalls, summaryCalls))
					}
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(response)), ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				var messages []map[string]any
				encode := func() []byte {
					body, err := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": messages, "tools": []any{}})
					if err != nil {
						t.Fatal(err)
					}
					return body
				}
				for index := 1; index <= 3; index++ {
					messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("synthetic input %d", index)})
					body := encode()
					opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
					req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
					switch mode {
					case "execute":
						if _, err := e.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
					case "stream":
						response, err := e.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					case "http":
						raw, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
						raw.Header = headers
						response, err := e.HttpRequest(ctx, auth, raw)
						if err != nil {
							t.Fatal(err)
						}
						_, errRead := io.Copy(io.Discard, response.Body)
						errClose := response.Body.Close()
						if errRead != nil || errClose != nil {
							t.Fatal("seed response did not drain")
						}
					}
					messages = append(messages, map[string]any{"role": "assistant", "content": fmt.Sprintf("synthetic reply %d", index)})
				}
				messages = append(messages, map[string]any{"role": "user", "content": "synthetic active input"})
				body := encode()
				owner := helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, "main", session, uuid.NewString(), uuid.NewString(), body)
				owner.ObserveSDKQuery(body)
				view, err := owner.CompactionView(body)
				if err != nil {
					history := owner.SDKHistory()
					t.Fatalf("%v: history_known=%v reason=%s messages=%d seeds=%d", err, history.OwnedMessagesKnown, history.IncompleteReason, len(history.Messages), seedCalls)
				}
				defer view.Discard()
				for range 7 {
					ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
				}
				result, err := helps.RunClaudeDesktopReactiveSummary(ctx, e, helps.ClaudeDesktopReactiveSummaryParams{
					Auth: auth, Bundle: e.desktopProfile, View: view, ParentRequest: body, SessionID: session})
				if err != nil {
					t.Fatal(err)
				}
				defer result.Payload.Discard()
				if seedCalls != 3 || owner.Snapshot().SDK.SawCompact || !view.Current() {
					t.Fatal("summary generation prematurely replaced/completed parent history")
				}
				switch retry {
				case "none", "prompt-too-long":
					wantCalls := 1
					if retry == "prompt-too-long" {
						wantCalls = 2
					}
					if !result.ReadyToApply || result.Attempts != wantCalls || summaryCalls != wantCalls || result.TotalGroups != 4 || result.Payload.AssistantMessages != 2 {
						t.Fatalf("native summary candidate differs: ready=%v attempts=%d calls=%d groups=%d", result.ReadyToApply, result.Attempts, summaryCalls, result.TotalGroups)
					}
					if result.Payload.Text.SelectedText() != "<summary>synthetic selected summary</summary>" || !result.Payload.UsageKnown || result.Payload.Usage != (claudeprompt.SDKTokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 20, CacheCreationInputTokens: 5}) {
						t.Fatal("native selected text or fork delta usage differs")
					}
					if retry == "prompt-too-long" && (summaryRows[0] != 5 || summaryRows[1] != 3 || clients[0] == clients[1]) {
						t.Fatalf("summary retry: rows=%v fresh_identity=%v", summaryRows, clients[0] != "" && clients[0] != clients[1])
					}
					application := result.Payload.Application
					if snapshot := owner.Snapshot().SDK; snapshot.PendingCompactions != 1 || snapshot.APIDurationMS <= 0 {
						t.Fatalf("real helper lost its ledger/disposition with telemetry disabled: %+v", snapshot)
					}
					if application == nil {
						t.Fatal("real summary did not prepare owned application")
					}
					nextBody, errBody := json.Marshal(map[string]any{"messages": application.Messages()})
					if errBody != nil {
						t.Fatal(errBody)
					}
					scope, _ := json.Marshal([]string{auth.ID, e.desktopProfile.ProfileID, auth.ProxyURL})
					next, errCommit := application.Commit(ctx, claudeprompt.Input{AccountID: string(scope), SessionID: session,
						PromptID: owner.Identity().PromptID, ClientRequestID: uuid.NewString(), Role: "main", Body: nextBody})
					if errCommit != nil || next == nil || !next.Snapshot().SDK.SawCompact || next.Snapshot().SDK.PendingCompactions != 0 || !next.SDKHistory().OwnedMessagesKnown {
						t.Fatal("real summary could not atomically replace owned text history")
					}
				case "media":
					if owner.Snapshot().SDK.PendingCompactions != 0 {
						t.Fatal("failed helper left an adoptable summary")
					}
					if result.ReadyToApply || result.Reason != "media_unstrippable" || result.Attempts != 1 || summaryCalls != 2 || summaryRows[0] != summaryRows[1] {
						t.Fatal("native media retry attempt semantics changed")
					}
				default:
					if result.ReadyToApply || result.Reason != "error" || summaryCalls != 1 {
						t.Fatal("failed summary produced an application candidate")
					}
				}
			})
		}
	}
}
