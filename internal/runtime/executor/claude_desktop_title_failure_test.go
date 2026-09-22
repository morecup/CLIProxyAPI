package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopTitleFailureWaitsForOutermostRetryAcrossEntryPoints(t *testing.T) {
	for _, entry := range []string{"execute", "stream", "http"} {
		for _, outcome := range []string{"http-error", "network-error", "exhausted", "successful-retry", "negative-retry", "cancelled-backoff", "missing-parent"} {
			t.Run(entry+"/"+outcome, func(t *testing.T) {
				// This scenario isolates title retry health, so the main context
				// must have valid storage rather than add an unrelated failure.
				executor := newClaudeDesktopTestExecutor(t)
				auth := newClaudeDesktopRawRequestTestAuth(t)
				doer := &claudeDesktopTelemetryTestDoer{}
				manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
				t.Cleanup(manager.Close)
				executor.desktopTelemetry = manager
				attempts := 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					body, errRead := io.ReadAll(request.Body)
					if errRead != nil {
						return nil, errRead
					}
					code, response := http.StatusOK, `{"input_tokens":1}`
					if request.URL.Path == "/v1/messages" {
						model := gjson.GetBytes(body, "model").String()
						response = `{"id":"msg_main","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic main reply"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":3}}`
						if model == "claude-haiku-4-5-20251001" {
							attempts++
							if strings.Contains(gjson.GetBytes(body, "system").Raw, "cc_prompt_id=") {
								t.Fatal("title inherited main system")
							}
							if outcome == "network-error" {
								return nil, errors.New("PRIVATE_TITLE_NETWORK_ERROR")
							}
							code, response = http.StatusBadGateway, `{"error":{"type":"server_error","message":"PRIVATE_TITLE_HTTP_ERROR"}}`
							if attempts == 2 && (outcome == "successful-retry" || outcome == "negative-retry") {
								text := `{"title":"PRIVATE_TITLE_SUCCESS"}`
								if outcome == "negative-retry" {
									text = `{"title":null}`
								}
								code = http.StatusOK
								response = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_title\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5-20251001\",\"content\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n" +
									"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
									fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text) +
									"data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
							}
						}
					}
					header := http.Header{"Request-Id": {fmt.Sprintf("req_synthetic_title_%d", attempts)}}
					if strings.HasPrefix(response, "data:") {
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: request, ContentLength: -1}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				headers := http.Header{"X-Session-Id": {"synthetic-title-terminal-session"}}
				parent := uuid.NewString()
				if _, errMain := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","max_tokens":32,"messages":[{"role":"user","content":"synthetic main prompt"}]}`)}, cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{claudeDesktopPromptIDMetadataKey: parent}}); errMain != nil {
					t.Fatal(errMain)
				}
				body := syntheticClaudeDesktopTitleBody(t, executor, auth)
				if outcome == "missing-parent" {
					parent = uuid.NewString()
				}
				ctx = cliproxyexecutor.WithClaudeDesktopParentPromptID(ctx, parent)
				ctx = cliproxyexecutor.WithUpstreamAttemptChain(ctx, time.Now())
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				direct := outcome == "http-error" || outcome == "network-error"
				release, releaseInner := func() {}, func() {}
				if !direct {
					release = cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
					releaseInner = cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
				}
				defer release()
				defer releaseInner()
				invoke := func() error {
					attemptCtx := cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
					request := cliproxyexecutor.Request{Model: "claude-haiku-4-5-20251001", Payload: body}
					options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{helps.ClaudeDesktopParentPromptIDMetadataKey: parent}}
					switch entry {
					case "execute":
						_, err := executor.Execute(attemptCtx, auth, request, options)
						return err
					case "stream":
						result, err := executor.ExecuteStream(attemptCtx, auth, request, options)
						if err != nil {
							return err
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
						return err
					default:
						raw, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
						if err != nil {
							return err
						}
						raw.Header = headers.Clone()
						response, err := executor.HttpRequest(attemptCtx, auth, raw)
						if err != nil {
							return err
						}
						_, errRead := io.Copy(io.Discard, response.Body)
						errClose := response.Body.Close()
						if response.StatusCode >= 400 {
							return statusErr{code: response.StatusCode, msg: "synthetic status"}
						}
						return errors.Join(errRead, errClose)
					}
				}
				if invoke() == nil {
					t.Fatal("expected first attempt to fail")
				}
				flush := func() map[string][]map[string]any {
					t.Helper()
					if err := manager.Flush(context.Background()); err != nil {
						t.Fatal(err)
					}
					return promptDeliveredEvents(t, doer)
				}
				if !direct {
					releaseInner()
					if len(flush()["tengu_session_title_generated"]) != 0 {
						t.Fatal("inner scope prematurely finalized title generation")
					}
					pending := false
					for _, endpoint := range manager.Status().DeliveryEndpoints {
						if endpoint.Role == "sdk-event-logging" && endpoint.Status == "awaiting-response-facts" {
							pending = true
						}
					}
					if !pending {
						t.Fatal("unresolved title retry decision hidden from status")
					}
				}
				if outcome == "successful-retry" || outcome == "negative-retry" || outcome == "exhausted" {
					errRetry := invoke()
					if (errRetry != nil) != (outcome == "exhausted") {
						t.Fatalf("retry result: %v", errRetry)
					}
				}
				if outcome == "cancelled-backoff" {
					cancel()
				}
				release()
				release()
				events := flush()
				titles := events["tengu_session_title_generated"]
				want := 1
				if outcome == "missing-parent" {
					want = 0
				}
				if len(titles) != want {
					t.Fatalf("titles=%v, want count=%d", titles, want)
				}
				if want != 0 && (titles[0]["cc_prompt_id"] != parent || titles[0]["success"] != (outcome == "successful-retry")) {
					t.Fatalf("terminal title=%v", titles)
				}
				if len(events["tengu_turn_end"]) != 1 || len(events["desktop_ccd_message_cycle_outcome"]) != 1 {
					t.Fatal("title failure finalized the main prompt again")
				}
				for _, endpoint := range manager.Status().DeliveryEndpoints {
					if endpoint.Role == "sdk-event-logging" && ((endpoint.Status == "awaiting-response-facts") != (outcome == "missing-parent")) {
						t.Fatalf("terminal title pending state=%+v", endpoint)
					}
				}
				for _, delivery := range doer.Requests() {
					for _, event := range gjson.GetBytes(delivery.Body, "events").Array() {
						decoded, _ := base64.StdEncoding.DecodeString(event.Get("event_data.additional_metadata").String())
						if strings.Contains(string(decoded), "PRIVATE_TITLE_") {
							t.Fatal("private title/error text exported")
						}
						if event.Get("event_data.event_name").String() == "tengu_session_title_generated" && event.Get("event_data.model").String() != "claude-opus-5" {
							t.Fatal("terminal title used helper model")
						}
					}
				}
			})
		}
	}
}
