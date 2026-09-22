package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func newClaudeSDKTimingSpan() *claudeDesktopRequestSpan {
	tracker := &claudeprompt.Tracker{}
	request := tracker.Begin(claudeprompt.Input{AccountID: "synthetic-account", SessionID: "synthetic-session",
		ClientRequestID: "synthetic-call", Role: "main", StartedAt: time.Now(),
		Body: []byte(`{"messages":[{"role":"user","content":"synthetic prompt"}]}`)})
	request.ObserveSDKQuery()
	return &claudeDesktopRequestSpan{prompt: request}
}

type claudeSDKCancelledBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b *claudeSDKCancelledBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()
		return n, context.Canceled
	}
	return n, err
}

func (*claudeSDKCancelledBody) Close() error { return nil }

func TestClaudeDesktopSDKCancellationAcrossAllEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		t.Run(mode, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/messages" {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":1}`)), Request: request}, nil
				}
				partial := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_partial\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
					"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
					"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req_cancel"}}, Body: &claudeSDKCancelledBody{Reader: strings.NewReader(partial), cancel: cancel}, ContentLength: -1, Request: request}, nil
			})
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
			ctx = cliproxyexecutor.WithUpstreamAttemptChain(ctx, time.Now())
			release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
			defer release()
			ctx = cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now())
			body := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"synthetic cancel"}]}`)
			headers := http.Header{"X-Session-Id": {"synthetic-cancel-session"}}
			opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
			req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body, Metadata: map[string]any{}}
			switch mode {
			case "execute":
				if _, err := executor.Execute(ctx, auth, req, opts); !errors.Is(err, context.Canceled) {
					t.Fatalf("execute cancellation=%v", err)
				}
			case "stream":
				response, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for range response.Chunks {
				}
			case "http":
				raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				raw.Header = headers
				response, err := executor.HttpRequest(ctx, auth, raw)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("HTTP body cancellation=%v", err)
				}
			}
			release()
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := promptDeliveredEvents(t, doer)
			if len(events["tengu_sdk_result"]) != 1 || len(events["tengu_turn_end"]) != 1 || len(events["tengu_api_success"]) != 0 || len(events["tengu_sdk_ttft"]) != 0 {
				t.Fatalf("cancelled executor event counts=%v", events)
			}
			result := events["tengu_sdk_result"][0]
			if result["subtype"] != "terminated" || result["num_turns"] != float64(2) || result["duration_api_ms"] != float64(0) || result["is_error"] != true {
				t.Fatalf("cancelled executor result=%v", result)
			}
			// Desktop uses the headless SDK interrupt route, not the interactive
			// CLI writer of interrupted_message_id. A new submission in the same
			// session must still emit its next input event after partial output.
			continued := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_continued\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			resumeTransport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				wire, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				responseBody, contentType := continued, "text/event-stream"
				if !gjson.GetBytes(wire, "stream").Bool() {
					responseBody, contentType = `{"id":"msg_continued","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "application/json"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}, "Request-Id": {"req_continued"}}, Body: io.NopCloser(strings.NewReader(responseBody)), ContentLength: -1, Request: request}, nil
			})
			resumeCtx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(resumeTransport))
			resumeBody := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"synthetic cancel"},{"role":"assistant","content":"partial"},{"role":"user","content":"synthetic continue"}]}`)
			resumeReq := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: resumeBody, Metadata: map[string]any{}}
			resumeOpts := cliproxyexecutor.Options{Headers: headers.Clone(), SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
			switch mode {
			case "execute":
				if _, err := executor.Execute(resumeCtx, auth, resumeReq, resumeOpts); err != nil {
					t.Fatal(err)
				}
			case "stream":
				response, err := executor.ExecuteStream(resumeCtx, auth, resumeReq, resumeOpts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			case "http":
				raw, err := http.NewRequestWithContext(resumeCtx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(resumeBody)))
				if err != nil {
					t.Fatal(err)
				}
				raw.Header = headers.Clone()
				response, err := executor.HttpRequest(resumeCtx, auth, raw)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			events = promptDeliveredEvents(t, doer)
			inputs := events["tengu_input_prompt"]
			if len(inputs) != 2 || len(events["tengu_api_success"]) != 1 || len(events["tengu_sdk_result"]) != 2 {
				t.Fatalf("post-cancel input or completion missing: %v", events)
			}
			for i, input := range inputs {
				if input["prompt_index"] != float64(i+1) || input["prompt_source"] != "sdk" {
					t.Fatalf("post-cancel input index/source changed: %v", input)
				}
				if _, present := input["interrupted_message_id"]; present {
					t.Fatal("Desktop SDK input inherited interactive CLI interrupted identity")
				}
			}
			if inputs[0]["cc_prompt_id"] == inputs[1]["cc_prompt_id"] {
				t.Fatal("continued input reused the cancelled prompt")
			}
		})
	}
}

func TestClaudeSDKTimingObservedBeforeResponseCompletion(t *testing.T) {
	span := newClaudeSDKTimingSpan()
	span.ObserveFirstByte(time.Now())
	for _, line := range []string{
		`data: {"type":"message_start","message":{"role":"assistant"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"synthetic"}}`,
	} {
		span.ObserveStreamLine([]byte(line))
	}
	if !span.prompt.Snapshot().SDK.FirstAssistantMessageAt.IsZero() {
		t.Fatal("headers or partial stream created assistant timing")
	}
	span.ObserveStreamLine([]byte(`data: {"type":"content_block_stop","index":0}`))
	first := span.prompt.Snapshot().SDK.FirstAssistantMessageAt
	if first.IsZero() || span.prompt.Snapshot().Complete {
		t.Fatal("closed block must observe assistant without completing the prompt")
	}
	for _, line := range []string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	} {
		span.ObserveStreamLine([]byte(line))
	}
	span.FinishSuccess(context.Background())
	if snapshot := span.prompt.Snapshot(); !snapshot.Complete || snapshot.SDK.FirstAssistantMessageAt != first {
		t.Fatal("completion lost or replaced the earlier assistant observation")
	}
}

func TestClaudeSDKClosedBlockDoesNotMakeTruncationSuccessful(t *testing.T) {
	span := newClaudeSDKTimingSpan()
	for _, line := range []string{
		`data: {"type":"message_start","message":{"role":"assistant"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	} {
		span.ObserveStreamLine([]byte(line))
	}
	span.FinishFailure(context.Background(), "incomplete_stream", errors.New("synthetic truncated response"))
	span.prompt.FinalizeFailure(time.Now())
	if snapshot := span.prompt.Snapshot(); snapshot.Complete || !snapshot.Failed || snapshot.SDK.FirstAssistantMessageAt.IsZero() {
		t.Fatalf("truncated response lost facts or became successful: %+v", snapshot)
	}
}
