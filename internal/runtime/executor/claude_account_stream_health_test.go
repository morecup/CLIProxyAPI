package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestClaudeAccountCompletedStreamHealthSurvivesCallerCancellation(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		t.Run(string(format), func(t *testing.T) {
			executor, auths := newExecutionSessionAccountTest(t)
			auth := auths[0]
			const model = "claude-opus-5"
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			manager.SetRetryConfig(0, 0, 0)
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			blocked := make(chan struct{})
			wire := sdkSessionContentResponse(t, `[{"type":"text","text":"synthetic response"}]`, true)
			ctx := context.WithValue(caller, "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				response := executionSessionTestResponse(t, request)
				if request.URL.Path == "/v1/messages" {
					// The upstream has sent its terminal event but keeps the body open.
					// A real SSE client may close as soon as it receives that event.
					response.Body = &queryLifetimeBlockingBody{ctx: request.Context(), prefix: strings.NewReader(wire), blocked: blocked}
				}
				return response, nil
			})))
			body := []byte(`{"model":"claude-opus-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"synthetic health check"}]}`)
			stream, err := manager.ExecuteStream(ctx, []string{auth.Provider}, cliproxyexecutor.Request{Model: model, Payload: body}, cliproxyexecutor.Options{
				Stream: true, SourceFormat: sdktranslator.FormatClaude, ResponseFormat: format, OriginalRequest: body,
			})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			terminalSeen := false
			for !terminalSeen {
				select {
				case chunk, ok := <-stream.Chunks:
					if !ok || chunk.Err != nil {
						t.Fatalf("stream ended before completion: %v", chunk.Err)
					}
					payload := string(chunk.Payload)
					terminalSeen = strings.Contains(payload, `"type":"message_stop"`) || strings.Contains(payload, `"finish_reason":"tool_calls"`) || strings.Contains(payload, `"type":"response.completed"`)
				case <-deadline.C:
					t.Fatal("terminal event was not forwarded before EOF")
				}
			}
			cancel()
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("completed stream became an error: %v", chunk.Err)
				}
			}
			stored, _ := manager.GetByID(auth.ID)
			if stored.Success != 1 || stored.Failed != 0 {
				t.Fatalf("completed stream health = %d/%d, want success=1 failed=0", stored.Success, stored.Failed)
			}
		})
	}
}
