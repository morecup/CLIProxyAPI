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
	"sync"
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

func TestClaudeDesktopNativeRecoveryContextAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, outcome := range []string{"native", "wait-post", "pre-blocked", "pre-error", "reset-error", "missing-operations", "post-error", "post-cancel", "restore-fallback", "foreign-account", "foreign-profile", "foreign-session", "foreign-egress"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				e := newClaudeDesktopTestExecutor(t)
				root := t.TempDir()
				e.desktopContexts = helps.NewClaudeDesktopContextStore(root, "")
				doer := &claudeDesktopTelemetryTestDoer{}
				e.desktopTelemetry = claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: e.desktopProfile,
					DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
				t.Cleanup(e.desktopTelemetry.Close)
				auth := newClaudeDesktopRawRequestTestAuth(t)
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				session := helps.ClaudeAgentSessionUUIDForRequest(headers, nil, nil, false)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var mu sync.Mutex
				stages := make(map[string]int)
				record := func(stage string) { mu.Lock(); stages[stage]++; mu.Unlock() }
				count := func(stage string) int { mu.Lock(); defer mu.Unlock(); return stages[stage] }
				postEntered, releasePost := make(chan struct{}), make(chan struct{})
				var unblock sync.Once
				t.Cleanup(func() { unblock.Do(func() { close(releasePost) }) })
				mainCalls, summaryCalls := 0, 0
				var finalBody []byte
				const ptl = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
				const summary = "<summary>synthetic native recovery summary</summary>"
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					status, reply, responseHeaders := 200, `{"input_tokens":1}`, http.Header{}
					if request.URL.Path == "/v1/messages" {
						if e.classifyClaudeDesktopRequestRole(body) == claudeprofile.RoleCompaction {
							summaryCalls++
							if count("PreCompact") != 1 || count("snapshot") != 0 || !bytes.Contains(body, []byte("synthetic precompact instructions")) {
								t.Error("summary did not run between PreCompact and snapshot/reset with the hook's instructions")
							}
							reply = reactiveSummaryStream("msg_native_summary", []string{summary})
							responseHeaders.Set("Content-Type", "text/event-stream")
							responseHeaders.Set("Request-Id", "req_native_summary")
						} else {
							mainCalls++
							responseHeaders.Set("Request-Id", fmt.Sprintf("req_native_main_%d", mainCalls))
							if mainCalls == 4 {
								status, reply = 400, ptl
							} else {
								if mainCalls >= 5 {
									finalBody = bytes.Clone(body)
									if count("post-complete") != 1 {
										t.Error("main sent before awaited PostCompact completed")
									}
								}
								text := fmt.Sprintf("synthetic native reply %d", mainCalls)
								reply = fmt.Sprintf(`{"id":"msg_native_%d","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, mainCalls, text)
								if gjson.GetBytes(body, "stream").Bool() {
									reply = reactiveSummaryStream(fmt.Sprintf("msg_native_%d", mainCalls), []string{text})
									responseHeaders.Set("Content-Type", "text/event-stream")
								}
							}
						}
					}
					return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
				var messages []map[string]any
				encode := func() []byte {
					body, err := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": messages, "tools": []any{}, "stream": mode == "http-stream"})
					if err != nil {
						t.Fatal(err)
					}
					return body
				}
				for index := 1; index <= 3; index++ {
					messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("synthetic native input %d", index)})
					if _, err := e.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: encode()}, cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}); err != nil {
						t.Fatal(err)
					}
					messages = append(messages, map[string]any{"role": "assistant", "content": fmt.Sprintf("synthetic native reply %d", index)})
				}
				wrap := func(payload string) json.RawMessage {
					row, err := claudeprompt.WrapSDKAttachment([]byte(payload), nil, nil)
					if err != nil {
						t.Error(err)
					}
					return row
				}
				native := helps.ClaudeDesktopRecoveryContext{
					Binding: cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: session},
					Hooks: func(hookCtx context.Context, input claudeprompt.SDKCompactHookInput) ([]claudeprompt.SDKCompactHookExecution, error) {
						record(input.Event)
						if input.Trigger != "auto" || input.CustomInstructions != nil {
							t.Error("native hook input differs")
						}
						if input.Event == "PreCompact" {
							if outcome == "pre-error" {
								return nil, errors.New("synthetic PreCompact failure")
							}
							return []claudeprompt.SDKCompactHookExecution{{Command: "synthetic-pre", Succeeded: true, Blocked: outcome == "pre-blocked", Output: "synthetic precompact instructions"}}, nil
						}
						if input.Event != "PostCompact" || input.CompactSummary != summary || count("snapshot") != 1 {
							t.Error("PostCompact used another summary or ran before snapshot/reset")
						}
						if outcome == "post-error" {
							return nil, errors.New("synthetic PostCompact failure")
						}
						if outcome == "post-cancel" {
							cancel()
							return nil, hookCtx.Err()
						}
						if outcome == "wait-post" {
							close(postEntered)
							select {
							case <-releasePost:
							case <-hookCtx.Done():
								return nil, hookCtx.Err()
							}
						}
						record("post-complete")
						return []claudeprompt.SDKCompactHookExecution{{Command: "synthetic-post", Succeeded: true}}, nil
					},
					Normalize: claudeprompt.SDKAttachmentNormalizeOptions{ReadTextDisplay: func(json.RawMessage) (claudeprompt.SDKReadTextDisplay, error) {
						return claudeprompt.SDKReadTextDisplay{TabAwareSeparator: true}, nil
					}},
					SnapshotAndReset: func(_ context.Context, preserved []claudeprompt.SDKHistoryMessage) (claudeprompt.SDKCompactionRestorationOps, error) {
						record("snapshot")
						if summaryCalls != 1 || len(preserved) == 0 || preserved[len(preserved)-1].Type != "user" || preserved[0].UUID == "" {
							t.Error("snapshot/reset lost the actual native preserve selection")
						}
						if outcome == "reset-error" {
							return claudeprompt.SDKCompactionRestorationOps{}, errors.New("synthetic reset failure")
						}
						if outcome == "missing-operations" {
							return claudeprompt.SDKCompactionRestorationOps{}, nil
						}
						return claudeprompt.SDKCompactionRestorationOps{
							ReadFiles: func(context.Context) ([]json.RawMessage, error) {
								record("files")
								if outcome == "restore-fallback" {
									return nil, errors.New("synthetic read restoration failure")
								}
								return []json.RawMessage{wrap(`{"type":"file","filename":"C:/synthetic/read.txt","content":{"type":"text","file":{"content":"PRIVATE_READ\n\tsecond","startLine":2,"numLines":2,"totalLines":2}}}`), wrap(`{"type":"already_read_file","filename":"C:/synthetic/kept.txt"}`)}, nil
							},
							LocalTasks: func(context.Context) ([]json.RawMessage, error) {
								record("tasks")
								return []json.RawMessage{wrap(`{"type":"task_status","taskId":"synthetic-agent","taskType":"local_agent","description":"PRIVATE_TASK","status":"running"}`)}, nil
							},
							PlanFile: func(context.Context) (json.RawMessage, error) {
								return wrap(`{"type":"plan_file_reference","planFilePath":"C:/synthetic/plan.md","planContent":"PRIVATE_PLAN"}`), nil
							},
							PlanMode: func(context.Context) (json.RawMessage, error) {
								if outcome == "restore-fallback" {
									return wrap(`{"type":"critical_system_reminder","content":"PRIVATE_FALLBACK"}`), nil
								}
								return nil, nil
							},
							InvokedSkills: func(context.Context) (json.RawMessage, error) {
								return wrap(`{"type":"invoked_skills","skills":[{"name":"synthetic-skill","path":"synthetic/skill","content":"PRIVATE_SKILL"}]}`), nil
							},
							DerivedContext: func(context.Context) ([]json.RawMessage, error) {
								return []json.RawMessage{[]byte(`{"type":"nested_memory","content":{"path":"synthetic/memory","content":"PRIVATE_MEMORY"}}`)}, nil
							},
							WrapAttachment: func(payload json.RawMessage) (json.RawMessage, error) {
								return claudeprompt.WrapSDKAttachment(payload, nil, nil)
							},
							SessionStart: func(context.Context) ([]json.RawMessage, error) {
								record("SessionStart")
								return []json.RawMessage{wrap(`{"type":"hook_additional_context","hookName":"SessionStart","content":["PRIVATE_HOOK"]}`)}, nil
							},
						}, nil
					},
				}
				switch outcome {
				case "foreign-account":
					native.Binding.AccountID += "-foreign"
				case "foreign-profile":
					native.Binding.ProfileID += "-foreign"
				case "foreign-session":
					native.Binding.SessionID = uuid.NewString()
				case "foreign-egress":
					native.Binding.Egress += "-foreign"
				}
				ctx = helps.WithClaudeDesktopRecoveryContext(ctx, native)
				messages = append(messages, map[string]any{"role": "user", "content": "synthetic current native input"})
				body := encode()
				invoke := func() error {
					opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
					req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
					switch mode {
					case "execute":
						_, err := e.Execute(ctx, auth, req, opts)
						return err
					case "stream":
						stream, err := e.ExecuteStream(ctx, auth, req, opts)
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
						raw, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
						raw.Header = headers.Clone()
						response, err := e.HttpRequest(ctx, auth, raw)
						if err != nil {
							return err
						}
						payload, err := io.ReadAll(response.Body)
						_ = response.Body.Close()
						if response.StatusCode >= 400 {
							if string(payload) != ptl {
								t.Error("failed recovery changed the original API error")
							}
							return errors.New("original prompt-too-long")
						}
						return err
					}
				}
				done := make(chan error, 1)
				go func() { done <- invoke() }()
				if outcome == "wait-post" {
					select {
					case <-postEntered:
						if mainCalls != 4 || count("post-complete") != 0 {
							t.Fatal("blocked PostCompact did not hold the main continuation")
						}
						unblock.Do(func() { close(releasePost) })
					case err := <-done:
						t.Fatal("recovery exited before PostCompact", err)
					case <-time.After(20 * time.Second):
						t.Fatal("PostCompact was never reached")
					}
				}
				err := <-done
				beforeSummary := strings.HasPrefix(outcome, "foreign-") || outcome == "pre-blocked" || outcome == "pre-error"
				success := outcome == "native" || outcome == "wait-post" || outcome == "restore-fallback"
				wantMain, wantSummary := 4, 1
				if success {
					wantMain = 5
				}
				if beforeSummary {
					wantSummary = 0
				}
				if mainCalls != wantMain || summaryCalls != wantSummary || (success && err != nil) || (!success && err == nil) {
					t.Fatalf("wrong recovery path: main=%d summary=%d err=%v", mainCalls, summaryCalls, err)
				}
				if strings.HasPrefix(outcome, "foreign-") && (count("PreCompact") != 0 || count("snapshot") != 0) {
					t.Fatal("foreign context callbacks executed")
				}
				if errFlush := e.desktopTelemetry.Flush(t.Context()); errFlush != nil {
					t.Fatal(errFlush)
				}
				events := promptDeliveredEvents(t, doer)
				wantSuccessEvents := 0
				if success {
					wantSuccessEvents = 1
				}
				if len(events["tengu_reactive_compact_succeeded"]) != wantSuccessEvents || len(events["tengu_reactive_compact_failed"]) != 0 {
					t.Fatalf("native recovery terminal events: success=%d failed=%d", len(events["tengu_reactive_compact_succeeded"]), len(events["tengu_reactive_compact_failed"]))
				}
				if success {
					metadata := events["tengu_reactive_compact_succeeded"][0]
					if metadata["trigger"] != "auto" || metadata["splitKind"] != "round" || metadata["headTruncations"] != float64(0) ||
						metadata["precomputed"] != false || metadata["querySource"] != "sdk" || metadata["desktop_app_version"] != "1.40609.0.0" ||
						metadata["attempts"] != float64(1) || metadata["restoredAttachmentCount"] == nil || metadata["postCompactTokens"] == nil {
						t.Fatalf("native recovery success metadata=%v", metadata)
					}
				}
				if !success && outcome != "pre-blocked" || outcome == "restore-fallback" {
					visible := false
					for _, endpoint := range e.desktopTelemetry.Status().DeliveryEndpoints {
						visible = visible || endpoint.Role == "sdk-event-logging" && strings.Contains(endpoint.Reason, "reactive compaction facts")
					}
					if !visible {
						t.Fatal("recovery stage failure was hidden by later API activity")
					}
				}
				if !success {
					return
				}
				markers := []string{"PRIVATE_READ", "PRIVATE_TASK", "PRIVATE_PLAN", "PRIVATE_SKILL", "PRIVATE_MEMORY", "PRIVATE_HOOK"}
				if outcome == "restore-fallback" {
					markers = []string{"PRIVATE_FALLBACK"}
					if bytes.Contains(finalBody, []byte("PRIVATE_READ")) || bytes.Contains(finalBody, []byte("PRIVATE_TASK")) {
						t.Fatal("failed restoration kept partial attachments")
					}
				}
				for restart := range 3 {
					for _, marker := range markers {
						if !bytes.Contains(finalBody, []byte(marker)) {
							t.Fatal("actual Messages omitted restored content", restart, marker)
						}
					}
					if restart == 2 {
						break
					}
					if restart == 1 {
						e.desktopTelemetry.Close()
						e = newClaudeDesktopTestExecutor(t)
						e.desktopTelemetry = nil
						e.desktopContexts = helps.NewClaudeDesktopContextStore(root, "")
					}
					messages = append(messages, map[string]any{"role": "assistant", "content": fmt.Sprintf("synthetic native reply %d", mainCalls)}, map[string]any{"role": "user", "content": "synthetic independent followup"})
					followupCtx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
					if _, err = e.Execute(followupCtx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: encode()}, cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}); err != nil {
						t.Fatal("restored wire continuity failed", err)
					}
					if count("snapshot") != 1 || count("PreCompact") != 1 || count("post-complete") != 1 || summaryCalls != 1 {
						t.Fatal("independent followup repeated recovery operations")
					}
				}
			})
		}
	}
}
