package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopAutomaticPTLRecoveryAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, outcome := range []string{"success", "gzip", "summary-failed", "summary-ptl", "summary-media", "summary-exhausted", "continuation-failed", "continuation-retry", "second-ptl", "cancel-summary", "non-ptl", "unowned-history", "context-write-failed", "telemetry-event-failed"} {
			for _, telemetry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/telemetry-%t", mode, outcome, telemetry), func(t *testing.T) {
					e := newClaudeDesktopTestExecutor(t)
					contextRoot := t.TempDir()
					e.desktopContexts = helps.NewClaudeDesktopContextStore(contextRoot, "")
					e.desktopTelemetry = nil
					doer := &claudeDesktopTelemetryTestDoer{}
					if telemetry {
						if outcome == "telemetry-event-failed" {
							delete(e.desktopProfile.SDKTelemetry.Events, claudetelemetry.FactSDKReactiveCompactTriggered)
						}
						e.desktopTelemetry = claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: e.desktopProfile,
							DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
						t.Cleanup(e.desktopTelemetry.Close)
					}
					auth := newClaudeDesktopRawRequestTestAuth(t)
					headers := http.Header{"X-Session-Id": {uuid.NewString()}}
					session := helps.ClaudeAgentSessionUUIDForRequest(headers, nil, nil, false)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					mainCalls, summaryCalls := 0, 0
					followupPhase := false
					var clients []string
					var finalBody []byte
					const ptl = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
					transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						status, reply := 200, `{"input_tokens":1}`
						header := http.Header{}
						if request.URL.Path == "/v1/messages" {
							if e.classifyClaudeDesktopRequestRole(body) == claudeprofile.RoleCompaction {
								summaryCalls++
								if telemetry {
									if err := e.desktopTelemetry.Flush(t.Context()); err != nil {
										t.Fatal(err)
									}
									events := promptDeliveredEvents(t, doer)
									wantTriggers := 1
									if outcome == "telemetry-event-failed" {
										wantTriggers = 0
									}
									if len(events["tengu_reactive_compact_triggered"]) != wantTriggers || len(events["tengu_reactive_compact_attempt"]) != summaryCalls {
										t.Fatal("actual helper dispatched before its trigger/iteration was persisted")
									}
								}
								if outcome == "context-write-failed" {
									if err := os.WriteFile(filepath.Join(contextRoot, "owned-context"), []byte("synthetic write obstruction"), 0600); err != nil {
										t.Fatal(err)
									}
								}
								if got := gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String(); got != session {
									t.Fatal("helper changed session")
								}
								attempt, ok := cliproxyexecutor.UpstreamAttemptFromContext(request.Context())
								if !ok || attempt.Number != 1 {
									t.Fatal("helper inherited main retries")
								}
								if outcome == "cancel-summary" {
									cancel()
									return nil, context.Canceled
								}
								if outcome == "summary-failed" {
									status, reply = 502, `{"type":"error","error":{"type":"server_error","message":"synthetic helper failure"}}`
								} else if outcome == "summary-exhausted" || (outcome == "summary-ptl" && summaryCalls == 1) {
									status, reply = 400, ptl
								} else if outcome == "summary-media" && summaryCalls == 1 {
									status, reply = 400, `{"type":"error","error":{"type":"invalid_request_error","message":"image exceeds API limit"}}`
								} else {
									reply = reactiveSummaryStream("msg_summary", []string{"<summary>synthetic recovery summary</summary>"})
									header.Set("Content-Type", "text/event-stream")
								}
								header.Set("Request-Id", "req_recovery_summary")
							} else {
								mainCalls++
								header.Set("Request-Id", fmt.Sprintf("req_main_%d", mainCalls))
								if mainCalls >= 4 {
									clients = append(clients, helps.HeaderValueCaseInsensitive(request.Header, "X-Client-Request-Id"))
								}
								if mainCalls == 4 {
									status, reply = 400, ptl
									if outcome == "non-ptl" {
										reply = `{"type":"error","error":{"type":"invalid_request_error","message":"invalid model"}}`
									}
									if outcome == "gzip" {
										var compressed bytes.Buffer
										writer := gzip.NewWriter(&compressed)
										_, _ = writer.Write([]byte(reply))
										_ = writer.Close()
										reply = compressed.String()
										header.Set("Content-Encoding", "gzip")
									}
								} else {
									if mainCalls > 4 {
										finalBody = bytes.Clone(body)
									}
									if mainCalls > 4 && !followupPhase {
										if files, errFiles := os.ReadDir(filepath.Join(contextRoot, "owned-context")); outcome != "context-write-failed" && (errFiles != nil || len(files) != 1) {
											t.Fatal("actual adoption was not persisted before sending its continuation", errFiles)
										}
										attempt, ok := cliproxyexecutor.UpstreamAttemptFromContext(request.Context())
										wantAttempt := mainCalls - 4
										if !ok || attempt.Number != wantAttempt || (wantAttempt == 1 && attempt.StartedAt != attempt.ChainStartedAt) {
											t.Fatal("continuation did not start a fresh API query")
										}
										if !strings.Contains(gjson.GetBytes(body, "system.0.text").String(), "cc_prev_req=req_recovery_summary;") {
											t.Fatal("continuation did not reference completed summary response")
										}
										plan, errPlan := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", nil)
										if errPlan != nil || helps.HeaderValueCaseInsensitive(request.Header, "Anthropic-Beta") != plan.anthropicBeta() {
											t.Fatal("continuation inherited helper headers")
										}
									}
									text := fmt.Sprintf("synthetic reply %d", mainCalls)
									reply = fmt.Sprintf(`{"id":"msg_main_%d","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, mainCalls, text)
									if gjson.GetBytes(body, "stream").Bool() {
										reply = reactiveSummaryStream(fmt.Sprintf("msg_main_%d", mainCalls), []string{text})
										header.Set("Content-Type", "text/event-stream")
									}
									if mainCalls > 4 && (outcome == "continuation-failed" || (outcome == "continuation-retry" && mainCalls == 5)) {
										status, reply = 502, `{"type":"error","error":{"type":"server_error","message":"synthetic continuation failure"}}`
									}
									if mainCalls > 4 && outcome == "second-ptl" {
										status, reply = 400, ptl
									}
								}
							}
						}
						return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
					})
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
					var messages []map[string]any
					invocationID, invocationClient := uuid.NewString(), uuid.NewString()
					invoke := func() (string, int, string, error) {
						body, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": messages, "tools": []any{}, "stream": mode == "http-stream"})
						id := invocationID
						opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{"claude_desktop_prompt_id": id, "claude_desktop_client_request_id": invocationClient}}
						req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
						switch mode {
						case "execute":
							response, err := e.Execute(ctx, auth, req, opts)
							return string(response.Payload), 200, id, err
						case "stream":
							response, err := e.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								return "", 0, id, err
							}
							var payload strings.Builder
							for chunk := range response.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
								payload.Write(chunk.Payload)
							}
							return payload.String(), 200, id, err
						default:
							rawCtx := context.WithValue(ctx, claudeDesktopHTTPRequestIdentityContextKey{}, claudeDesktopHTTPRequestIdentity{promptID: id, clientRequestID: invocationClient})
							raw, _ := http.NewRequestWithContext(rawCtx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
							raw.Header = headers.Clone()
							id, _ = claudeDesktopRequestUUIDForHTTPRequest(raw)
							response, err := e.HttpRequest(ctx, auth, raw)
							if err != nil {
								return "", 0, id, err
							}
							payload, err := io.ReadAll(response.Body)
							_ = response.Body.Close()
							return string(payload), response.StatusCode, id, err
						}
					}
					for index := 1; index <= 3; index++ {
						messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("synthetic input %d", index)})
						if _, _, _, err := invoke(); err != nil {
							t.Fatal(err)
						}
						messages = append(messages, map[string]any{"role": "assistant", "content": fmt.Sprintf("synthetic reply %d", index)})
						invocationID, invocationClient = uuid.NewString(), uuid.NewString()
					}
					if outcome == "unowned-history" {
						e.desktopPrompts = claudeprompt.Tracker{}
					}
					ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
					release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
					defer release()
					messages = append(messages, map[string]any{"role": "user", "content": "synthetic current input"})
					payload, status, promptID, err := invoke()
					if outcome == "continuation-retry" {
						if err == nil && status < 400 {
							t.Fatal("synthetic failed continuation was hidden")
						}
						ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
						payload, status, promptID, err = invoke()
					}
					release()
					if telemetry {
						if err := e.desktopTelemetry.Flush(t.Context()); err != nil {
							t.Fatal(err)
						}
						events := promptDeliveredEvents(t, doer)
						wantTriggers := 1
						if outcome == "non-ptl" || outcome == "unowned-history" || outcome == "telemetry-event-failed" {
							wantTriggers = 0
						}
						if len(events["tengu_reactive_compact_triggered"]) != wantTriggers || len(events["tengu_reactive_compact_attempt"]) != summaryCalls || len(events["tengu_reactive_compact_succeeded"]) != 0 {
							t.Fatal("failed/retried/cancelled recovery misreported its actual lifecycle")
						}
						for index, metadata := range events["tengu_reactive_compact_attempt"] {
							wantAttempt := index + 1
							if outcome == "summary-media" {
								wantAttempt = 1
							}
							if metadata["attempt"] != float64(wantAttempt) || metadata["cc_prompt_id"] != promptID || metadata["strippedMedia"] != (outcome == "summary-media" && index == 1) {
								t.Fatalf("attempt borrowed HTTP retry or helper ownership: %v", metadata)
							}
						}
					}
					if outcome == "non-ptl" || outcome == "unowned-history" {
						if summaryCalls != 0 || mainCalls != 4 {
							t.Fatal("non-PTL triggered compaction")
						}
						return
					}
					wantSummaryCalls := 1
					if outcome == "summary-ptl" || outcome == "summary-media" || outcome == "summary-exhausted" {
						wantSummaryCalls = 2
					}
					if summaryCalls != wantSummaryCalls {
						t.Fatalf("summary calls=%d main calls=%d error=%v status=%d", summaryCalls, mainCalls, err, status)
					}
					if outcome == "summary-failed" || outcome == "cancel-summary" || outcome == "summary-exhausted" {
						if mainCalls != 4 {
							t.Fatal("failed/cancelled summary retransmitted main")
						}
						if (outcome == "summary-failed" || outcome == "summary-exhausted") && mode != "execute" && mode != "stream" && (payload != ptl || status != 400) {
							t.Fatal("raw original error was not preserved")
						}
						if outcome == "cancel-summary" && err == nil {
							t.Fatal("cancelled recovery returned an HTTP response")
						}
						return
					}
					wantCalls := 5
					if outcome == "continuation-retry" {
						wantCalls = 6
						if len(clients) != 3 || clients[1] != clients[2] {
							t.Fatal("retry lost recovered API identity")
						}
					}
					if mainCalls != wantCalls || len(clients) != wantCalls-3 || clients[0] == clients[1] || !bytes.Contains(finalBody, []byte("synthetic recovery summary")) {
						t.Fatal("main did not adopt and retransmit with fresh identity")
					}
					if len(gjson.GetBytes(finalBody, "messages").Array()) >= len(messages) {
						t.Fatal("continuation kept uncompressed input")
					}
					if outcome == "continuation-failed" || outcome == "second-ptl" {
						if err == nil && status < 400 {
							t.Fatal("failed continuation became success")
						}
						return
					}
					if err != nil || status != 200 || !strings.Contains(payload, fmt.Sprintf("synthetic reply %d", wantCalls)) {
						t.Fatalf("recovery failed: status=%d err=%v", status, err)
					}
					scope, _ := json.Marshal([]string{auth.ID, e.desktopProfile.ProfileID, auth.ProxyURL})
					owner := e.desktopPrompts.BindSDKHelper(claudeprompt.Input{AccountID: string(scope), SessionID: session, ParentPromptID: promptID})
					if owner == nil {
						t.Fatal("recovery replaced logical prompt")
					}
					snapshot := owner.Snapshot()
					if !snapshot.SawCompact || snapshot.RecoveredAPIFailures != 1 || snapshot.Queries != 2 || snapshot.NumTurns != 2 || snapshot.PendingCompactions != 0 || !snapshot.CompleteFacts {
						t.Fatalf("incorrect recovered query accounting: %+v", snapshot)
					}
					if telemetry {
						if errFlush := e.desktopTelemetry.Flush(t.Context()); errFlush != nil {
							t.Fatal(errFlush)
						}
						events := promptDeliveredEvents(t, doer)
						if len(events["tengu_api_success"]) != 5 {
							t.Fatal("failed query was emitted as an API success or a success was lost")
						}
						if len(events["tengu_turn_end"]) != 4 || len(events["desktop_ccd_message_cycle_start"]) != 4 || len(events["desktop_ccd_message_cycle_outcome"]) != 4 {
							t.Fatalf("recovery prematurely ended or restarted the logical cycle: turn=%d start=%d outcome=%d", len(events["tengu_turn_end"]), len(events["desktop_ccd_message_cycle_start"]), len(events["desktop_ccd_message_cycle_outcome"]))
						}
						if len(events["tengu_reactive_compact_succeeded"]) != 0 {
							t.Fatal("text-only recovery claimed unimplemented native restoration/hook success")
						}
						{
							visible := false
							for _, endpoint := range e.desktopTelemetry.Status().DeliveryEndpoints {
								visible = visible || (endpoint.Role == "sdk-event-logging" && strings.Contains(endpoint.Reason, "reactive compaction facts"))
							}
							if !visible {
								t.Fatal("absent native context or event persistence failure disappeared after successful main continuation")
							}
						}
						if outcome == "context-write-failed" {
							visible := false
							for _, endpoint := range e.desktopTelemetry.Status().DeliveryEndpoints {
								if endpoint.Role == "sdk-event-logging" && strings.Contains(endpoint.Reason, "owned conversation context") {
									visible = true
								}
							}
							if !visible {
								t.Fatal("real adopted-state write failure disappeared after successful response")
							}
						}
					}
					if outcome == "success" || outcome == "continuation-retry" {
						followupPhase = true
						for restart := range 2 {
							if restart == 1 {
								if e.desktopTelemetry != nil {
									e.desktopTelemetry.Close()
								}
								e = newClaudeDesktopTestExecutor(t)
								e.desktopTelemetry = nil
								e.desktopContexts = helps.NewClaudeDesktopContextStore(contextRoot, "")
							}
							ctx = context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
							invocationID, invocationClient = uuid.NewString(), uuid.NewString()
							messages = append(messages,
								map[string]any{"role": "assistant", "content": fmt.Sprintf("synthetic reply %d", mainCalls)},
								map[string]any{"role": "user", "content": fmt.Sprintf("synthetic followup %d", restart)})
							payload, status, _, err := invoke()
							if err != nil || status != 200 || !strings.Contains(payload, fmt.Sprintf("synthetic reply %d", mainCalls)) {
								t.Fatalf("independent followup failed after restart=%d: %v", restart, err)
							}
							rows := gjson.GetBytes(finalBody, "messages").Array()
							if summaryCalls != 1 || len(rows) >= len(messages) || !bytes.Contains(finalBody, []byte("synthetic recovery summary")) || !strings.Contains(rows[len(rows)-1].Raw, fmt.Sprintf("synthetic followup %d", restart)) {
								t.Fatal("independent request lost compaction or its new user input")
							}
							if client := clients[len(clients)-1]; client != invocationClient || client == clients[0] || client == clients[1] {
								t.Fatal("independent turn inherited the recovered request identity")
							}
							if restart == 0 {
								followup := e.desktopPrompts.BindSDKHelper(claudeprompt.Input{AccountID: string(scope), SessionID: session, ParentPromptID: invocationID})
								if followup == nil || followup.Snapshot().Queries != 1 || followup.Snapshot().NumTurns != 1 || !followup.Snapshot().CompleteFacts {
									t.Fatal("owned continuation was recounted as a new compaction or history became unknown")
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestClaudeDesktopOpus55CompactionContinuationRevalidatesRequest(t *testing.T) {
	e := newClaudeDesktopTestExecutor(t)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	session := uuid.NewString()
	ctx := t.Context()

	var (
		messages []map[string]any
		owner    *claudeprompt.Request
		body     []byte
	)
	for index := 0; index < 3; index++ {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("synthetic input %d", index)})
		requestBody := map[string]any{"model": "claude-opus-5-5", "messages": messages}
		if index == 2 {
			requestBody["tools"] = []map[string]any{{"type": "computer_20251124", "name": "computer"}}
		}
		var errMarshal error
		body, errMarshal = json.Marshal(requestBody)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		owner = helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, "main", session, uuid.NewString(), uuid.NewString(), body)
		owner.ObserveSDKQuery(body)
		if index == 2 {
			break
		}
		var response claudeprompt.Response
		response.ObservePayloadAt([]byte(fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"synthetic reply"}],"stop_reason":"end_turn"}`, index)), false, time.Now())
		owner.ObserveSDKHistory(response.SDKHistoryMessages())
		owner.ObserveSDKWireResponse(response.SDKWireFingerprint())
		owner.FinishSuccess(time.Now(), "end_turn", nil)
		messages = append(messages, map[string]any{"role": "assistant", "content": "synthetic reply"})
	}

	view, errView := owner.CompactionView(body)
	if errView != nil {
		t.Fatal(errView)
	}
	defer view.Discard()
	var summary claudeprompt.SDKCompactionResponse
	summary.ObserveJSON([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"<summary>synthetic recovery summary</summary>"}]}`))
	text, known := summary.TakeText(e.desktopProfile.DesktopVersion, e.desktopProfile.CodeVersion)
	if !known {
		t.Fatal("synthetic summary was not selected")
	}
	history := view.History()
	groups := claudeprompt.GroupSDKHistory(history.Messages)
	owner.RecordSDKCompactionSuccess("compact", "synthetic-helper", 100,
		claudeprompt.CompletedSDKCompaction(claudeprompt.ObserveSDKCompactionInput(body), text.Fingerprint()))
	application, errApplication := view.PrepareApplication(text, claudeprompt.SDKCompactionWrapOptions{SuppressFollowUpQuestions: true}, groups[len(groups)-1], "synthetic-helper")
	if errApplication != nil {
		t.Fatal(errApplication)
	}
	defer application.Discard()

	plan, errPlan := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	plan.PromptID = owner.Identity().PromptID
	plan.ClientRequestID = uuid.NewString()
	plan.NativePrompt = owner
	facts := e.newClaudeDesktopRuntimeFactsForPlan(auth, session, "claude-opus-5-5", plan.PromptID, plan.ClientRequestID, "", plan)
	facts.Prompt = owner
	state := claudeDesktopRequestExecution{ctx: ctx, body: body, sourceBody: body, plan: plan, facts: facts}
	original, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	_, continuation, err := e.prepareClaudeDesktopCompactionContinuation(auth, original, state, application)
	assertClaudeOpus55RequestValidationError(t, err)
	if continuation != nil {
		t.Fatal("invalid Opus 5.5 continuation was rendered")
	}
	if owner.Snapshot().SDK.SawCompact {
		t.Fatal("invalid Opus 5.5 continuation committed compacted history")
	}
}
