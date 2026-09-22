package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeAccountSDKLiveRetentionAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, outcome := range []string{"success", "http-error", "cancelled", "incomplete"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				auth := newClaudeAccountRuntimeTestAuth(t, uuid.NewString(), uuid.NewString(), uuid.NewString())
				prepareH73RuntimeAuth(auth)
				account := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
				t.Cleanup(account.Close)
				if err := account.Provision(auth); err != nil {
					t.Fatal(err)
				}
				headers := http.Header{"X-Session-Id": {uuid.NewString()}}
				session := ""
				accountScope, _ := json.Marshal([]string{auth.ID, account.bundle.ProfileID, auth.ProxyURL})
				scope := string(accountScope)
				clock := time.Now()
				var firstUUID string
				calls := 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					status, reply := 200, `{}`
					responseHeaders := http.Header{}
					if request.URL.Path == "/v1/messages/count_tokens" {
						reply = `{"input_tokens":10}`
					}
					if request.URL.Path == "/v1/messages" {
						observeDefaultSDKSession(t, request, &session)
						calls++
						tracker := &accountRuntimeForAuth(t, account, auth.ID).executor.desktopPrompts
						before := tracker.NativeContent(scope, session)
						if len(before.Messages) != 1 {
							t.Fatal("default request did not establish its native input owner")
						}
						firstUUID = before.Messages[0].UUID
						// Advance only the cache's synthetic observation clock. No
						// live network call, real delay, deadline or host action occurs.
						other := tracker.Begin(claudeprompt.Input{AccountID: scope, SessionID: uuid.NewString(), Role: "main", ClientRequestID: uuid.NewString(), StartedAt: clock.Add(2 * time.Hour), Body: []byte(`{"messages":[{"role":"user","content":"synthetic cache pressure"}]}`)})
						other.FinalizeFailure(clock.Add(2 * time.Hour))
						if retained := tracker.NativeContent(scope, session); len(retained.Messages) != 1 || retained.Messages[0].UUID != firstUUID {
							t.Fatal("real default request lost native ownership while upstream was active")
						}
						if outcome == "cancelled" {
							return nil, context.Canceled
						}
						body, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						reply = `{"id":"msg_retained","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic late reply"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
						if gjson.GetBytes(body, "stream").Bool() {
							reply = reactiveSummaryStream("msg_retained", []string{"synthetic late reply"})
							responseHeaders.Set("Content-Type", "text/event-stream")
						}
						if outcome == "http-error" {
							status, reply = 400, `{"type":"error","error":{"type":"invalid_request_error","message":"synthetic rejected request"}}`
							responseHeaders.Set("Content-Type", "application/json")
						} else if outcome == "incomplete" {
							reply = `{"type":"message","role":"assistant"`
							if gjson.GetBytes(body, "stream").Bool() {
								reply = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_incomplete\",\"role\":\"assistant\",\"content\":[]}}\n\n"
							}
						}
					}
					return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				body, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": []any{map[string]any{"role": "user", "content": "synthetic retained input"}}, "tools": []any{}, "stream": mode == "http-stream"})
				request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
				opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
				var callErr error
				switch mode {
				case "execute":
					_, callErr = account.Execute(ctx, auth, request, opts)
				case "stream":
					stream, err := account.ExecuteStream(ctx, auth, request, opts)
					callErr = err
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								callErr = chunk.Err
							}
						}
					}
				default:
					raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					raw.Header = headers.Clone()
					response, err := account.HttpRequest(ctx, auth, raw)
					callErr = err
					if err == nil {
						_, callErr = io.ReadAll(response.Body)
						if errClose := response.Body.Close(); callErr == nil {
							callErr = errClose
						}
						if response.StatusCode != 200 && callErr == nil {
							callErr = fmt.Errorf("synthetic response status %d", response.StatusCode)
						}
					}
				}
				if calls != 1 || firstUUID == "" || (outcome == "success" && callErr != nil) {
					t.Fatal("default entry did not complete the expected single attempt", calls, callErr)
				}
				runtime := accountRuntimeForAuth(t, account, auth.ID)
				tracker := &runtime.executor.desktopPrompts
				native := tracker.NativeContent(scope, session)
				wantRows := 1
				if outcome == "success" {
					wantRows = 2
					assertDefaultNativeContent(t, native, tracker.SDKHistory(scope, session), session)
				} else if outcome == "cancelled" {
					// A real cancellation owns the native user interruption row;
					// retaining only the input would itself lose native history.
					wantRows = 2
				}
				if native.PersistenceError || len(native.Messages) != wantRows || native.Messages[0].UUID != firstUUID {
					t.Fatalf("late completion lost native ownership: rows=%d want=%d write_error=%v issue=%q", len(native.Messages), wantRows, native.PersistenceError, native.IncompleteReason)
				}
				if outcome == "cancelled" && (native.Messages[1].Type != "user" || !bytes.Contains(native.Messages[1].Message, []byte("[Request interrupted by user]"))) {
					t.Fatal("late cancellation did not persist its actual interruption row")
				}
				if err := tracker.FlushNativeTranscript(); err != nil {
					t.Fatal(err)
				}
				identity, _ := json.Marshal([]string{scope, session})
				digest := sha256.Sum256(identity)
				scopeHash := hex.EncodeToString(digest[:])
				index, err := helps.NewClaudeDesktopTranscriptStore(runtime.executor.desktopDurableStatePath, "").LoadTranscript(scopeHash)
				if err != nil || len(index.UUIDs) != wantRows || index.UUIDs[0] != firstUUID {
					t.Fatal("late completion failed to append to its protected default transcript", err)
				}
				payload, _, err := helps.NewClaudeDesktopSDKSessionStore(runtime.executor.desktopDurableStatePath, "").Load(scopeHash)
				if err != nil || len(gjson.GetBytes(payload, "prompts").Map()) != 1 {
					t.Fatal("late completion lost its default durable structural owner", err)
				}
				gjson.GetBytes(payload, "prompts").ForEach(func(_, state gjson.Result) bool {
					if state.Get("failed").Bool() == (outcome == "success") {
						t.Error("persisted terminal disposition differs from the actual outcome")
					}
					return true
				})
				last := tracker.Begin(claudeprompt.Input{AccountID: scope, SessionID: uuid.NewString(), Role: "main", ClientRequestID: uuid.NewString(), StartedAt: clock.Add(4 * time.Hour), Body: []byte(`{"messages":[{"role":"user","content":"synthetic idle prune"}]}`)})
				last.FinalizeFailure(clock.Add(4 * time.Hour))
				if len(tracker.NativeContent(scope, session).Messages) != 0 {
					t.Fatal("completed default entry leaked an in-flight retention lease")
				}
			})
		}
	}
}
