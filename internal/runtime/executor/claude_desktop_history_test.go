package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopTextHistoryAcrossEntryPointsWithoutTelemetry(t *testing.T) {
	for _, modes := range [][2]string{{"execute", "stream"}, {"stream", "http"}, {"http", "execute"}} {
		t.Run(modes[0]+"_to_"+modes[1], func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			executor.desktopTelemetry = nil
			auth := newClaudeDesktopRawRequestTestAuth(t)
			calls, session := 0, ""
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				response := `{"input_tokens":1}`
				header := http.Header{}
				if request.URL.Path == "/v1/messages" {
					calls++
					session = gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()
					text := fmt.Sprintf("synthetic reply %d", calls)
					response = fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":3}}`, calls, text)
					if gjson.GetBytes(body, "stream").Bool() {
						header.Set("Content-Type", "text/event-stream")
						response = fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n", calls) +
							"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
							fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text) +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(response)), ContentLength: -1, Request: request}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			headers := http.Header{"X-Session-Id": {"synthetic-text-history"}}
			for index, mode := range modes {
				messages := `[{"role":"user","content":"synthetic first input"}]`
				if index == 1 {
					messages = `[{"role":"user","content":"synthetic first input"},{"role":"assistant","content":"synthetic reply 1"},{"role":"user","content":"synthetic second input"}]`
				}
				body := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":` + messages + `}`)
				request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body}
				options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
				switch mode {
				case "execute":
					if _, err := executor.Execute(ctx, auth, request, options); err != nil {
						t.Fatal(err)
					}
				case "stream":
					response, err := executor.ExecuteStream(ctx, auth, request, options)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "http":
					raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
					if err != nil {
						t.Fatal(err)
					}
					raw.Header = headers.Clone()
					response, err := executor.HttpRequest(ctx, auth, raw)
					if err != nil {
						t.Fatal(err)
					}
					_, errRead := io.Copy(io.Discard, response.Body)
					errClose := response.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatalf("read=%v close=%v", errRead, errClose)
					}
				}
			}
			scope, _ := json.Marshal([]string{auth.ID, executor.desktopProfile.ProfileID, auth.ProxyURL})
			history := executor.desktopPrompts.SDKHistory(string(scope), session)
			if session == "" || calls != 2 || !history.OwnedMessagesKnown || len(history.Messages) != 4 || len(history.Groups) != 3 {
				t.Fatalf("text continuation without telemetry=%+v", history)
			}
			if !history.TokenEstimate.Known || history.TokenEstimate.Tokens != 7 {
				t.Fatalf("native usage anchor without telemetry=%+v", history)
			}
			for _, message := range history.Messages {
				if !message.TokenEstimate.Known {
					t.Fatalf("unobserved content estimate: %+v", message)
				}
			}
		})
	}
}
