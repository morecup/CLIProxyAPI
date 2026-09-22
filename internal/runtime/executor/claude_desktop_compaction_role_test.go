package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopCompactionCanonicalInstruction(t *testing.T) {
	root := os.Getenv("CLAUDE_DESKTOP_CAPTURE_CORPUS")
	if root == "" {
		t.Skip("requires an explicitly selected read-only recorder corpus")
	}
	// The capture stays in its original recorder directory. Never copy its
	// payload into fixtures or include it in test diagnostics.
	raw, errRead := os.ReadFile(filepath.Join(root, "h7-v140609-message-opus-compaction-chunk-11", "flow-006302", "request-body.raw.bin"))
	if errRead != nil {
		t.Fatal("canonical request cannot be read")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "a64803070dbc3121ddb55f28bf392b4a45316c5b6b9a3ce92406a447493e15ca" {
		t.Fatal("canonical request hash mismatch")
	}
	executor := newClaudeDesktopTestExecutor(t)
	if executor.classifyClaudeDesktopRequestRole(raw) != claudeprofile.RoleCompaction {
		t.Fatal("real canonical compact request lost its role")
	}
	instruction := claudeDesktopLastUserText(raw)
	if len(instruction) != 6369 {
		t.Fatal("canonical instruction extent changed")
	}
	custom := func(text string) string {
		return instruction[:6195] + "\n\nAdditional Instructions:\n" + text + instruction[6195:]
	}
	for _, fixture := range []struct {
		name, text string
		want       claudeprofile.RequestRole
	}{
		{"default", instruction, claudeprofile.RoleCompaction},
		{"custom", custom("synthetic custom instruction"), claudeprofile.RoleCompaction},
		{"custom unicode", custom("\ufeff中文\n"), claudeprofile.RoleCompaction},
		{"custom JS non-whitespace", custom("\u0085"), claudeprofile.RoleCompaction},
		{"empty custom", custom(""), claudeprofile.RoleMain},
		{"JS whitespace-only custom", custom("\ufeff\u00a0 \t\r\n"), claudeprofile.RoleMain},
		{"quoted", "Explain this instruction: " + instruction, claudeprofile.RoleMain},
		{"partial", instruction[:100], claudeprofile.RoleMain},
		{"altered", "X" + instruction[1:], claudeprofile.RoleMain},
		{"appended", instruction + "\nordinary user question", claudeprofile.RoleMain},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			body, errMarshal := json.Marshal(map[string]any{"model": "claude-opus-5", "max_tokens": 64000, "messages": []map[string]any{{"role": "user", "content": fixture.text}}})
			if errMarshal != nil {
				t.Fatal("cannot encode instruction fixture")
			}
			if executor.classifyClaudeDesktopRequestRole(body) != fixture.want {
				t.Fatal("instruction role mismatch")
			}
		})
	}
	for _, field := range []*string{&executor.desktopProfile.DesktopVersion, &executor.desktopProfile.CodeVersion} {
		old := *field
		*field = "unreviewed-version"
		if executor.classifyClaudeDesktopRequestRole(raw) != claudeprofile.RoleMain {
			t.Fatal("unreviewed version inherited a compact instruction signature")
		}
		*field = old
	}
}

func TestClaudeDesktopCompactionMarkerRemainsMainAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http"} {
		t.Run(mode, func(t *testing.T) {
			executor := newClaudeDesktopTestExecutor(t)
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				response := `{"input_tokens":1}`
				if request.URL.Path == "/v1/messages" {
					response = `{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic reply"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					if gjson.GetBytes(body, "stream").Bool() {
						response = "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"type\":\"message\",\"id\":\"msg_synthetic\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
							"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"synthetic reply\"}}\n\n" +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Request-Id": {"req_synthetic"}}, Body: io.NopCloser(strings.NewReader(response)), ContentLength: -1, Request: request}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			body := []byte(`{"model":"claude-opus-5","max_tokens":32,"messages":[{"role":"user","content":"Explain: CRITICAL: Respond with TEXT ONLY. Do NOT call any tools"}]}`)
			headers := http.Header{"X-Session-Id": {"synthetic-marker-" + mode}}
			request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
			options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude")}
			switch mode {
			case "execute":
				if _, err := executor.Execute(ctx, auth, request, options); err != nil {
					t.Fatal(err)
				}
			case "stream":
				result, err := executor.ExecuteStream(ctx, auth, request, options)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
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
				response, err := executor.HttpRequest(ctx, auth, raw)
				if err != nil {
					t.Fatal(err)
				}
				_, errRead := io.Copy(io.Discard, response.Body)
				errClose := response.Body.Close()
				if errRead != nil || errClose != nil {
					t.Fatal("synthetic response drain failed")
				}
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := promptDeliveredEvents(t, doer)
			if len(events["desktop_ccd_message_cycle_start"]) != 1 || len(events["tengu_turn_end"]) != 1 || len(events["tengu_sdk_result"]) != 1 || len(events["tengu_api_success"]) != 1 || events["tengu_api_success"][0]["querySource"] != "sdk" {
				t.Fatalf("ordinary marker lifecycle counts: cycle=%d turn=%d result=%d success=%d; success sources=%v", len(events["desktop_ccd_message_cycle_start"]), len(events["tengu_turn_end"]), len(events["tengu_sdk_result"]), len(events["tengu_api_success"]), compactSuccessSources(events["tengu_api_success"]))
			}
		})
	}
}

func compactSuccessSources(events []map[string]any) []any {
	var sources []any
	for _, event := range events {
		sources = append(sources, event["querySource"])
	}
	return sources
}

func TestClaudeDesktopCompactionAdoptionAcrossEntryPoints(t *testing.T) {
	root := os.Getenv("CLAUDE_DESKTOP_CAPTURE_CORPUS")
	if root == "" {
		t.Skip("requires an explicitly selected read-only recorder corpus")
	}
	// Only the verified compact instruction is used transiently. All history,
	// tool results, summary outputs and identities in this chain are synthetic.
	raw, err := os.ReadFile(filepath.Join(root, "h7-v140609-message-opus-compaction-chunk-11", "flow-006302", "request-body.raw.bin"))
	if err != nil {
		t.Fatal("canonical compact instruction unavailable")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "a64803070dbc3121ddb55f28bf392b4a45316c5b6b9a3ce92406a447493e15ca" {
		t.Fatal("canonical compact request changed")
	}
	instruction := claudeDesktopLastUserText(raw)
	data, err := os.ReadFile("../../claudedesktop/prompt/testdata/sdk-compaction-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Response string                  `json:"response_text"`
		Wrappers []struct{ Text string } `json:"wrappers"`
	}
	if json.Unmarshal(data, &vectors) != nil || len(vectors.Wrappers) != 16 {
		t.Fatal("invalid synthetic compaction fixture")
	}
	encode := func(value any) string {
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			t.Fatal("synthetic chain could not be encoded")
		}
		return string(encoded)
	}
	for _, mode := range []string{"execute", "stream", "http"} {
		t.Run(mode, func(t *testing.T) {
			executor := newClaudeDesktopTestExecutor(t)
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				response := `{"input_tokens":1}`
				if request.URL.Path == "/v1/messages" {
					calls++
					stop, content := "end_turn", `{"type":"text","text":"synthetic done"}`
					if calls == 1 {
						stop, content = "tool_use", `{"type":"tool_use","id":"tool_compact_chain","name":"Read","input":{}}`
					} else if calls == 2 {
						content = encode(map[string]any{"type": "text", "text": vectors.Response})
					}
					response = fmt.Sprintf(`{"type":"message","role":"assistant","id":"msg_compact_chain_%d","model":"claude-opus-5","content":[%s],"stop_reason":%q,"usage":{"input_tokens":1,"output_tokens":1}}`, calls, content, stop)
					if gjson.GetBytes(body, "stream").Bool() {
						blockStart, blockDelta := content, ""
						if gjson.Get(content, "type").String() == "text" {
							// Native streaming clears start.text; real text arrives in deltas.
							blockStart = `{"type":"text","text":""}`
							blockDelta = "data: " + encode(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": gjson.Get(content, "text").String()}}) + "\n\n"
						}
						response = fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_compact_chain_%d\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", calls) +
							fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":%s}\n\n", blockStart) + blockDelta +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							fmt.Sprintf("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":1}}\n\n", stop) +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Request-Id": {fmt.Sprintf("req_compact_chain_%d", calls)}}, Body: io.NopCloser(strings.NewReader(response)), ContentLength: -1, Request: request}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			parent := ""
			for phase := 0; phase < 3; phase++ {
				var content any = "synthetic input before compact"
				if phase == 1 {
					content = []map[string]any{{"type": "tool_result", "tool_use_id": "tool_compact_chain", "content": "synthetic tool result"}, {"type": "text", "text": instruction}}
				} else if phase == 2 {
					content = vectors.Wrappers[12].Text
				}
				body := []byte(encode(map[string]any{"model": "claude-opus-5", "max_tokens": 64000, "messages": []map[string]any{{"role": "user", "content": content}}}))
				requestCtx := ctx
				if parent != "" {
					requestCtx = cliproxyexecutor.WithClaudeDesktopParentPromptID(ctx, parent)
				}
				headers := http.Header{"X-Session-Id": {"synthetic-compaction-" + mode}}
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
				opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude")}
				switch mode {
				case "execute":
					if _, errExecute := executor.Execute(requestCtx, auth, req, opts); errExecute != nil {
						t.Fatal("synthetic compact chain execute failed")
					}
				case "stream":
					result, errStream := executor.ExecuteStream(requestCtx, auth, req, opts)
					if errStream != nil {
						t.Fatal("synthetic compact chain stream failed")
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal("synthetic compact chain chunk failed")
						}
					}
				case "http":
					rawRequest, errNew := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
					if errNew != nil {
						t.Fatal(errNew)
					}
					rawRequest.Header = headers
					response, errHTTP := executor.HttpRequest(requestCtx, auth, rawRequest)
					if errHTTP != nil {
						t.Fatal("synthetic compact chain HTTP failed")
					}
					_, errRead := io.Copy(io.Discard, response.Body)
					errClose := response.Body.Close()
					if errRead != nil || errClose != nil {
						t.Fatal("synthetic compact chain drain failed")
					}
				}
				if errFlush := manager.Flush(ctx); errFlush != nil {
					t.Fatal(errFlush)
				}
				events := promptDeliveredEvents(t, doer)
				if phase == 0 {
					if len(events["tengu_api_query"]) != 1 {
						t.Fatal("first query did not provide a parent identity")
					}
					parent, _ = events["tengu_api_query"][0]["cc_prompt_id"].(string)
					if parent == "" {
						t.Fatal("missing root prompt identity")
					}
				}
				if phase < 2 && len(events["tengu_sdk_result"]) != 0 {
					t.Fatal("helper response completed an unadopted prompt")
				}
			}
			events := promptDeliveredEvents(t, doer)
			for event, count := range map[string]int{"tengu_input_prompt": 1, "tengu_api_query": 3, "tengu_api_success": 3, "tengu_sdk_result": 1, "tengu_sdk_ttft": 1, "tengu_turn_end": 1, "desktop_ccd_message_cycle_start": 1, "desktop_ccd_message_cycle_outcome": 1} {
				if len(events[event]) != count {
					t.Fatalf("%s count=%d want=%d", event, len(events[event]), count)
				}
			}
			for index, source := range []string{"sdk", "compact", "sdk"} {
				if events["tengu_api_success"][index]["querySource"] != source {
					t.Fatal("helper or continuation lost its query role")
				}
			}
			result := events["tengu_sdk_result"][0]
			if result["num_turns"] != float64(3) || result["tool_use_count"] != float64(0) || result["saw_compact"] != true || result["cc_prompt_id"] != parent || calls != 3 {
				t.Fatalf("three-entrypoint compact adoption differs: %v; calls=%d", result, calls)
			}
		})
	}
}
