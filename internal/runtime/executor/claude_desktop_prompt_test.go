package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopPromptContinuesAcrossAllExecutorEntryPoints(t *testing.T) {
	for _, modes := range [][2]string{{"execute", "stream"}, {"stream", "http"}, {"http", "execute"}} {
		t.Run(modes[0]+"_to_"+modes[1], func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			wireSessionID := ""
			type preparationShape struct{ messages, systemMessages, blocks, staticLength, dynamicLength int }
			var wireShapes []preparationShape
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				responseBody := `{"input_tokens":1}`
				if request.URL.Path == "/v1/messages" {
					calls++
					wireSessionID = gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()
					shape := preparationShape{messages: len(gjson.GetBytes(body, "messages").Array()), blocks: len(gjson.GetBytes(body, "system").Array())}
					for _, message := range gjson.GetBytes(body, "messages").Array() {
						if message.Get("role").String() == "system" {
							shape.systemMessages++
						}
					}
					shape.staticLength = len(utf16.Encode([]rune(gjson.GetBytes(body, "system.2.text").String())))
					shape.dynamicLength = len(utf16.Encode([]rune(gjson.GetBytes(body, "system.3.text").String())))
					wireShapes = append(wireShapes, shape)
					stop := "end_turn"
					content := `{"type":"text","text":"synthetic done"}`
					if calls == 1 {
						stop = "tool_use"
						content = `{"type":"tool_use","id":"tool_synthetic","name":"Read","input":{"file_path":"synthetic"}}`
					}
					responseBody = fmt.Sprintf(`{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-sonnet-5","content":[%s],"stop_reason":%q,"usage":{"input_tokens":5,"output_tokens":3}}`, content, stop)
					if gjson.GetBytes(body, "stream").Bool() {
						// Native stream starts clear text/input; the value arrives in deltas.
						delta := map[string]any{"type": "text_delta", "text": "synthetic done"}
						if calls == 1 {
							delta = map[string]any{"type": "input_json_delta", "partial_json": `{"file_path":"synthetic"}`}
						}
						deltaJSON, errMarshal := json.Marshal(delta)
						if errMarshal != nil {
							return nil, errMarshal
						}
						responseBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
							fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":%s}\n\n", content) +
							fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":%s}\n\n", deltaJSON) +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							fmt.Sprintf("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":3}}\n\n", stop) +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Request-Id": {fmt.Sprintf("req_synthetic_%d", calls)}}, Body: io.NopCloser(strings.NewReader(responseBody)), ContentLength: -1, Request: request}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			for index, mode := range modes {
				messages := `[{"role":"user","content":"synthetic user prompt"}]`
				if index == 1 {
					messages = `[{"role":"user","content":"synthetic user prompt"},{"role":"assistant","content":[{"type":"tool_use","id":"tool_synthetic","name":"Read","input":{"file_path":"synthetic"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_synthetic","content":"synthetic result"}]}]`
				}
				body := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":` + messages + `,"tools":[]}`)
				headers := http.Header{"X-Session-Id": {"synthetic-shared-session"}}
				opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
				req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body, Metadata: map[string]any{}}
				switch mode {
				case "execute":
					if _, errExecute := executor.Execute(ctx, auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
				case "stream":
					response, errStream := executor.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
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
					response, errHTTP := executor.HttpRequest(ctx, auth, raw)
					if errHTTP != nil {
						t.Fatal(errHTTP)
					}
					_, errRead := io.Copy(io.Discard, response.Body)
					errClose := response.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatalf("read=%v close=%v", errRead, errClose)
					}
				}
				if errFlush := manager.Flush(ctx); errFlush != nil {
					t.Fatal(errFlush)
				}
				events := promptDeliveredEvents(t, doer)
				if index == 0 && (len(events["tengu_turn_end"]) != 0 || len(events["desktop_ccd_message_cycle_outcome"]) != 0) {
					t.Fatal("tool request prematurely ended prompt")
				}
			}
			events := promptDeliveredEvents(t, doer)
			scope, _ := json.Marshal([]string{auth.ID, executor.desktopProfile.ProfileID, auth.ProxyURL})
			history := executor.desktopPrompts.SDKHistory(string(scope), wireSessionID)
			if wireSessionID == "" || !history.OwnedMessagesKnown || len(history.Messages) != 4 || len(history.Groups) != 2 {
				t.Fatalf("entrypoints lost native message/group ownership: %+v", history)
			}
			for event, count := range map[string]int{"tengu_input_prompt": 1, "tengu_api_after_normalize": 2, "tengu_api_query": 2, "tengu_api_success": 2, "tengu_turn_end": 1, "desktop_ccd_message_cycle_start": 1, "desktop_ccd_message_cycle_outcome": 1} {
				if len(events[event]) != count {
					t.Fatalf("%s count=%d want=%d", event, len(events[event]), count)
				}
			}
			queries := events["tengu_api_query"]
			input := events["tengu_input_prompt"][0]
			if input["prompt_index"] != float64(1) || input["prompt_length"] != float64(len("synthetic user prompt")) || input["cc_prompt_id"] != queries[0]["cc_prompt_id"] {
				t.Fatalf("input submission was lost or measured after profile rendering: %v", input)
			}
			boundaryIndex := 0
			for index, shape := range wireShapes {
				normalized := events["tengu_api_after_normalize"][index]
				if normalized["postNormalizedMessageCount"] != float64(shape.messages) || normalized["apiSystemMessageCount"] != float64(shape.systemMessages) || normalized["cc_prompt_id"] != queries[index]["cc_prompt_id"] {
					t.Fatalf("preparation facts differ from final upstream request: %v", normalized)
				}
				if shape.blocks == 4 {
					for pass := 0; pass < 2; pass++ {
						if boundaryIndex >= len(events["tengu_sysprompt_boundary_found"]) {
							t.Fatal("system boundary event missing from executor path")
						}
						boundary := events["tengu_sysprompt_boundary_found"][boundaryIndex]
						boundaryIndex++
						if boundary["staticBlockLength"] != float64(shape.staticLength) || boundary["dynamicBlockLength"] != float64(shape.dynamicLength) || boundary["blockCount"] != float64(shape.blocks) {
							t.Fatalf("system boundary differs from final wire: %v", boundary)
						}
					}
				}
			}
			if boundaryIndex != len(events["tengu_sysprompt_boundary_found"]) {
				t.Fatal("extra system boundary events")
			}
			if queries[0]["cc_prompt_id"] != queries[1]["cc_prompt_id"] || queries[0]["queryChainId"] != queries[1]["queryChainId"] || queries[0]["queryDepth"] != float64(0) || queries[1]["queryDepth"] != float64(1) {
				t.Fatalf("query ownership differs: %#v", queries)
			}
			end := events["tengu_turn_end"][0]
			if end["cc_prompt_id"] != queries[0]["cc_prompt_id"] || end["terminal_reason"] != "completed" || end["query_source"] != "sdk" || end["query_source_category"] != "main" || end["is_error"] != false || end["is_subagent"] != false || end["goal_active"] != false || end["duration_ms"].(float64) < 0 {
				t.Fatalf("turn-end payload=%#v", end)
			}
			if len(events["tengu_sdk_result"]) != 1 || len(events["tengu_sdk_ttft"]) != 1 {
				t.Fatal("canonical tool-result yield did not reach SDK result/TTFT")
			}
			result := events["tengu_sdk_result"][0]
			if result["num_turns"] != float64(2) || result["tool_use_count"] != float64(1) || result["builtin_tool_calls"] != float64(1) || result["subtype"] != "success" || result["cc_prompt_id"] != queries[0]["cc_prompt_id"] {
				t.Fatalf("tool-result SDK accounting diverged: %#v", result)
			}
		})
	}
}

func promptDeliveredEvents(t *testing.T, doer *claudeDesktopTelemetryTestDoer) map[string][]map[string]any {
	t.Helper()
	events := make(map[string][]map[string]any)
	for _, request := range doer.Requests() {
		for _, event := range gjson.GetBytes(request.Body, "events").Array() {
			name := event.Get("event_data.event_name").String()
			if name == "" {
				continue
			}
			metadata := event.Get("event_data.additional_metadata")
			data := []byte(metadata.Raw)
			if metadata.Type == gjson.String {
				var errDecode error
				data, errDecode = base64.StdEncoding.DecodeString(metadata.String())
				if errDecode != nil {
					t.Fatal(errDecode)
				}
			}
			var fields map[string]any
			if metadata.Exists() {
				if errJSON := json.Unmarshal(data, &fields); errJSON != nil {
					t.Fatal(errJSON)
				}
			}
			events[name] = append(events[name], fields)
		}
	}
	return events
}

func TestClaudeDesktopPromptFailureWaitsForConductorDecision(t *testing.T) {
	for _, mode := range []string{"terminal", "successful-retry", "cancelled-backoff", "direct"} {
		t.Run(mode, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, code := `{"input_tokens":1}`, 200
				if request.URL.Path == "/v1/messages" {
					calls++
					body, code = `{"error":{"type":"server_error","message":"synthetic failure"}}`, 502
					if calls == 2 {
						body, code = `{"id":"msg_done","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, 200
					}
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Request-Id": {fmt.Sprintf("req_%d", calls)}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1, Request: request}, nil
			})
			ctx := cliproxyexecutor.WithUpstreamAttemptChain(context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport)), time.Now())
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			release := func() {}
			if mode != "direct" {
				release = cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
			}
			defer release()
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"synthetic-retry-session"}}, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{}}
			req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`), Metadata: map[string]any{}}
			_, errFirst := executor.Execute(cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now()), auth, req, opts)
			if errFirst == nil {
				t.Fatal("expected first attempt failure")
			}
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			if mode != "direct" {
				if len(promptDeliveredEvents(t, doer)["desktop_ccd_message_cycle_outcome"]) != 0 {
					t.Fatal("attempt failure was reported as terminal before retry decision")
				}
				found := false
				for _, endpoint := range manager.Status().DeliveryEndpoints {
					if endpoint.Role == "desktop-event-logging" {
						found = endpoint.Status == "awaiting-prompt-completion" && endpoint.Reason != ""
					}
				}
				if !found {
					t.Fatal("unresolved terminal decision was hidden from management status")
				}
			}
			if mode == "successful-retry" || mode == "cancelled-backoff" {
				var observed *claudeDesktopRetryTelemetryError
				if !errors.As(errFirst, &observed) {
					t.Fatal("retry observer missing")
				}
				observed.RecordScheduledRetry(ctx, 1, 0)
				if mode == "successful-retry" {
					if _, errRetry := executor.Execute(cliproxyexecutor.WithNextUpstreamAttempt(ctx, time.Now()), auth, req, opts); errRetry != nil {
						t.Fatal(errRetry)
					}
				} else {
					cancel()
				}
			}
			release()
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			events := promptDeliveredEvents(t, doer)
			if len(events["tengu_api_after_normalize"]) != 1 {
				t.Fatal("retry/terminal decision repeated request normalization telemetry")
			}
			if len(events["desktop_ccd_message_cycle_start"]) != 1 || len(events["desktop_ccd_message_cycle_outcome"]) != 1 {
				t.Fatalf("cycle counts start=%d outcome=%d", len(events["desktop_ccd_message_cycle_start"]), len(events["desktop_ccd_message_cycle_outcome"]))
			}
			wantEnd := 0
			if mode == "successful-retry" {
				wantEnd = 1
			}
			if len(events["tengu_turn_end"]) != wantEnd {
				t.Fatal("terminal failure fabricated a successful turn end")
			}
			for _, endpoint := range manager.Status().DeliveryEndpoints {
				if endpoint.Role == "desktop-event-logging" && endpoint.Status != "ready" {
					t.Fatalf("terminal decision did not clear pending state: %+v", endpoint)
				}
			}
		})
	}
}
