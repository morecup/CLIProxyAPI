package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopFastOverageEventAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		t.Run(mode, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				if gjson.GetBytes(body, "speed").String() != "fast" {
					t.Fatal("fast request did not reach the wire")
				}
				return &http.Response{StatusCode: 429, Request: request,
					Header: http.Header{"Request-Id": {"req_synthetic"}, "anthropic-ratelimit-unified-overage-disabled-reason": {"org_level_disabled"}},
					Body:   io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"synthetic rejection"}}`))}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			body := []byte(`{"model":"claude-opus-5","speed":"fast","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`)
			headers := http.Header{"X-Session-Id": {"synthetic-overage-session"}}
			request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body, Metadata: map[string]any{}}
			options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
			switch mode {
			case "execute":
				if _, errRun := executor.Execute(ctx, auth, request, options); errRun == nil {
					t.Fatal("expected HTTP failure")
				}
			case "stream":
				if _, errRun := executor.ExecuteStream(ctx, auth, request, options); errRun == nil {
					t.Fatal("expected HTTP failure")
				}
			case "http":
				raw, errNew := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
				if errNew != nil {
					t.Fatal(errNew)
				}
				raw.Header = headers
				response, errRun := executor.HttpRequest(ctx, auth, raw)
				if errRun != nil {
					t.Fatal(errRun)
				}
				if response.StatusCode != 429 {
					t.Fatalf("status = %d, want 429", response.StatusCode)
				}
				if _, errRead := io.Copy(io.Discard, response.Body); errRead != nil {
					t.Fatal(errRead)
				}
				if errClose := response.Body.Close(); errClose != nil {
					t.Fatal(errClose)
				}
			}
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			events := promptDeliveredEvents(t, doer)
			rejections := events["tengu_fast_mode_overage_rejected"]
			if calls != 1 || len(rejections) != 1 || rejections[0]["overage_disabled_reason"] != "org_level_disabled" || rejections[0]["cc_prompt_id"] == "" {
				t.Fatalf("rejection events = %v, calls = %d", rejections, calls)
			}
			if len(events["tengu_api_retry"]) != 0 || len(events["tengu_api_success"]) != 0 || len(events["tengu_turn_end"]) != 0 {
				t.Fatal("rejection fabricated a retry or success")
			}
		})
	}
}

func TestClaudeDesktopCacheDiagnosisAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		t.Run(mode, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			previousMessageID := ""
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				responseBody := `{"input_tokens":1}`
				if request.URL.Path == "/v1/messages" {
					calls++
					diagnostics := ""
					if calls == 2 {
						previousMessageID = gjson.GetBytes(body, "diagnostics.previous_message_id").String()
						if previousMessageID == "" {
							t.Fatal("request diagnostics were not carried to the final wire")
						}
						diagnostics = `,"diagnostics":{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":42}}`
					}
					responseBody = fmt.Sprintf(`{"id":"msg_synthetic_%d","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"synthetic response"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}%s}`, calls, diagnostics)
					if gjson.GetBytes(body, "stream").Bool() {
						responseBody = fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic_%d\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}%s}}\n\n", calls, diagnostics) +
							"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
							"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"synthetic response\"}}\n\n" +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Request-Id": {fmt.Sprintf("req_synthetic_%d", calls)}}, Body: io.NopCloser(strings.NewReader(responseBody)), ContentLength: -1, Request: request}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			for range 2 {
				body := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`)
				headers := http.Header{"X-Session-Id": {"synthetic-cache-diagnosis-" + mode}}
				request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body, Metadata: map[string]any{}}
				options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
				switch mode {
				case "execute":
					if _, errRun := executor.Execute(ctx, auth, request, options); errRun != nil {
						t.Fatal(errRun)
					}
				case "stream":
					response, errRun := executor.ExecuteStream(ctx, auth, request, options)
					if errRun != nil {
						t.Fatal(errRun)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "http":
					raw, errNew := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
					if errNew != nil {
						t.Fatal(errNew)
					}
					raw.Header = headers
					response, errRun := executor.HttpRequest(ctx, auth, raw)
					if errRun != nil {
						t.Fatal(errRun)
					}
					_, errRead := io.Copy(io.Discard, response.Body)
					errClose := response.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatalf("response read=%v, close=%v", errRead, errClose)
					}
				}
			}
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			events := promptDeliveredEvents(t, doer)
			diagnoses := events["tengu_prompt_cache_diagnosis_received"]
			if calls != 2 || len(diagnoses) != 1 || diagnoses[0]["previousMessageId"] != previousMessageID || diagnoses[0]["requestId"] != "req_synthetic_2" || diagnoses[0]["tokensMissed"] != float64(42) {
				t.Fatalf("diagnoses = %v, previous = %q, calls = %d", diagnoses, previousMessageID, calls)
			}
			if len(events["tengu_api_success"]) != 2 || len(events["tengu_api_retry"]) != 0 {
				t.Fatal("diagnosis changed API success/retry behavior")
			}
		})
	}
}
