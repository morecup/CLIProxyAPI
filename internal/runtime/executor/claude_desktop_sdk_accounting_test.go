package executor

import (
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
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopSDKLedgerAcrossEntryPointsAndTelemetryAvailability(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		for _, availability := range []string{"absent", "disabled", "sdk-worker-unavailable", "active"} {
			t.Run(mode+"/"+availability, func(t *testing.T) {
				e := newClaudeDesktopTestExecutor(t)
				e.desktopTelemetry = nil
				auth := newClaudeDesktopRawRequestTestAuth(t)
				doer := &claudeDesktopTelemetryTestDoer{}
				var manager *claudetelemetry.Manager
				if availability != "absent" {
					root := ""
					if availability != "disabled" {
						root = t.TempDir()
						if availability == "sdk-worker-unavailable" {
							// Fail only the optional SDK queue. The renderer's durable
							// request-lineage storage is a separate prerequisite.
							queueRoot := filepath.Join(root, "telemetry")
							if err := os.MkdirAll(queueRoot, 0700); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(queueRoot, e.desktopProfile.SDKTelemetry.EndpointRole), []byte("synthetic SDK storage failure"), 0600); err != nil {
								t.Fatal(err)
							}
						}
					}
					manager = claudetelemetry.NewManager(claudetelemetry.Options{StatePath: root, Bundle: e.desktopProfile,
						DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
					t.Cleanup(manager.Close)
					e.desktopTelemetry = manager
				}
				session, mainCalls := "", 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					reply := `{"input_tokens":1}`
					header := http.Header{}
					if request.URL.Path == "/v1/messages" {
						mainCalls++
						session = gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()
						reply = `{"type":"message","role":"assistant","model":"claude-opus-5","id":"msg_accounting","content":[{"type":"text","text":"synthetic reply"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
						if gjson.GetBytes(body, "stream").Bool() {
							reply = reactiveSummaryStream("msg_accounting", []string{"synthetic reply"})
							header.Set("Content-Type", "text/event-stream")
						}
						header.Set("Request-Id", "req_accounting")
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				now := time.Now()
				ctx = cliproxyexecutor.WithUpstreamAttemptChain(ctx, now.Add(-3*time.Second))
				ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, now.Add(-500*time.Millisecond))
				promptID, clientID := uuid.NewString(), uuid.NewString()
				body := []byte(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"synthetic input"}]}`)
				headers := http.Header{"X-Session-Id": {"synthetic-accounting-session"}}
				options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude,
					Metadata: map[string]any{"claude_desktop_prompt_id": promptID, "claude_desktop_client_request_id": clientID}}
				request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
				switch mode {
				case "execute":
					if _, err := e.Execute(ctx, auth, request, options); err != nil {
						t.Fatal(err)
					}
				case "stream":
					result, err := e.ExecuteStream(ctx, auth, request, options)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "http":
					raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
					if err != nil {
						t.Fatal(err)
					}
					raw.Header = headers
					promptID, _ = claudeDesktopRequestUUIDForHTTPRequest(raw)
					result, err := e.HttpRequest(ctx, auth, raw)
					if err != nil {
						t.Fatal(err)
					}
					_, errRead := io.Copy(io.Discard, result.Body)
					errClose := result.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatalf("read=%v close=%v", errRead, errClose)
					}
				}
				scope, _ := json.Marshal([]string{auth.ID, e.desktopProfile.ProfileID, auth.ProxyURL})
				owner := e.desktopPrompts.BindSDKHelper(claudeprompt.Input{AccountID: string(scope), SessionID: session, ParentPromptID: promptID})
				if owner == nil || mainCalls != 1 {
					t.Fatal("real entrypoint lost parent identity")
				}
				snapshot := owner.Snapshot()
				if !snapshot.CompleteFacts || snapshot.Queries != 1 || snapshot.APIDurationMS < 3000 {
					t.Fatalf("accounting depended on telemetry: %+v", snapshot)
				}
				if availability == "active" {
					if err := manager.Flush(t.Context()); err != nil {
						t.Fatal(err)
					}
					events := promptDeliveredEvents(t, doer)
					if len(events["tengu_api_success"]) != 1 {
						t.Fatal("success telemetry missing")
					}
					metadata := events["tengu_api_success"][0]
					if metadata["durationMsIncludingRetries"] != float64(snapshot.APIDurationMS) {
						t.Fatalf("ledger and emitted success clocks differ: %v vs %d", metadata["durationMsIncludingRetries"], snapshot.APIDurationMS)
					}
				}
			})
		}
	}
}

func TestClaudeDesktopSDKCompactionCallbackAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, telemetry := range []bool{false, true} {
			name := mode + "/disabled"
			if telemetry {
				name = mode + "/enabled"
			}
			t.Run(name, func(t *testing.T) {
				e := newClaudeDesktopTestExecutor(t)
				e.desktopTelemetry = nil
				auth := newClaudeDesktopRawRequestTestAuth(t)
				if telemetry {
					doer := &claudeDesktopTelemetryTestDoer{}
					manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: e.desktopProfile,
						DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
					t.Cleanup(manager.Close)
					e.desktopTelemetry = manager
				}
				sessionID := uuid.NewString()
				ctx := cliproxyexecutor.WithClaudeDesktopSessionBinding(t.Context(), cliproxyexecutor.ClaudeDesktopSessionBinding{
					AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: sessionID})
				owner := helps.BeginClaudeDesktopPrompt(&e.desktopPrompts, ctx, auth, e.desktopProfile.ProfileID, "main", sessionID, uuid.NewString(), uuid.NewString(), []byte(`{"messages":[{"role":"user","content":"synthetic parent"}]}`))
				owner.ObserveSDKQuery([]byte(`{"messages":[{"role":"user","content":"synthetic parent"}]}`))
				ctx = cliproxyexecutor.WithClaudeDesktopParentPromptID(ctx, owner.Identity().PromptID)
				ctx = cliproxyexecutor.WithIndependentUpstreamAttempt(ctx, time.Now().Add(-time.Second))
				instruction, err := e.desktopProfile.CompactionInstruction("")
				if err != nil {
					t.Fatal(err)
				}
				// Direct API entries retain the bundle's transport stream policy.
				// The automatic summary dispatcher always selects ExecuteStream.
				body, err := json.Marshal(map[string]any{"model": "claude-opus-5", "stream": mode == "http-stream", "messages": []map[string]any{{"role": "user", "content": instruction}}})
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					wire, errRead := io.ReadAll(request.Body)
					if errRead != nil {
						return nil, errRead
					}
					if request.URL.Path == "/v1/messages/count_tokens" {
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":1}`)), Request: request}, nil
					}
					if request.URL.Path != "/v1/messages" || e.classifyClaudeDesktopRequestRole(wire) != "compaction" {
						return nil, fmt.Errorf("unexpected compact wire shape: path=%s", request.URL.Path)
					}
					calls++
					reply := `{"type":"message","role":"assistant","id":"msg_compact","content":[{"type":"text","text":"<summary>synthetic summary</summary>"}],"stop_reason":"end_turn"}`
					contentType := "application/json"
					if gjson.GetBytes(wire, "stream").Bool() {
						reply = reactiveSummaryStream("msg_compact", []string{"<summary>synthetic summary</summary>"})
						contentType = "text/event-stream"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}, "Request-Id": {"req_compact_accounting"}},
						Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				switch mode {
				case "execute":
					if _, err := e.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				case "stream":
					result, err := e.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "http", "http-stream":
					raw, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
					result, err := e.HttpRequest(ctx, auth, raw)
					if err != nil {
						t.Fatal(err)
					}
					_, errRead := io.Copy(io.Discard, result.Body)
					errClose := result.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatal("helper stream was not consumed")
					}
				}
				if snapshot := owner.Snapshot().SDK; calls != 1 || snapshot.PendingCompactions != 1 || snapshot.APIDurationMS < 1000 || snapshot.SawCompact || snapshot.Queries != 1 {
					t.Fatalf("compact callback lost accounting or adopted a summary: %+v", snapshot)
				}
			})
		}
	}
}

func TestClaudeDesktopSDKScheduledRetryWithoutTelemetryKeepsParentOpen(t *testing.T) {
	span := newClaudeSDKTimingSpan()
	ctx := cliproxyexecutor.WithUpstreamAttemptChain(t.Context(), time.Now())
	release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
	defer release()
	err := statusErr{code: 502, msg: "synthetic failure"}
	span.FinishFailure(ctx, "server_error", err)
	span.RecordScheduledRetry(ctx, 1, time.Second, err)
	snapshot := span.prompt.Snapshot()
	if snapshot.Failed || !snapshot.SDK.SawRetry || snapshot.SDK.RetryStatus != 502 {
		t.Fatalf("retry either lost its status or finalized its parent: %+v", snapshot)
	}
	release()
	if !span.prompt.Snapshot().Failed {
		t.Fatal("exhausted invocation failed to settle its parent")
	}
}
