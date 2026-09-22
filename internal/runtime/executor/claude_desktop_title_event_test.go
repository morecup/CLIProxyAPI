package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeDesktopTitleCompletionAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "execute-negative", "stream-negative", "http-negative"} {
		t.Run(mode, func(t *testing.T) {
			negative := strings.HasSuffix(mode, "-negative")
			titleOutput := `{"title":"PRIVATE_GENERATED_TITLE"}`
			if negative {
				titleOutput = `{"title":" "}`
			}
			executor := NewClaudeExecutor(&config.Config{})
			auth := newClaudeDesktopRawRequestTestAuth(t)
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: executor.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
			t.Cleanup(manager.Close)
			executor.desktopTelemetry = manager
			calls := 0
			titleHeaders := http.Header{}
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if request.URL.Path != "/v1/messages" {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":1}`)), Request: request}, nil
				}
				calls++
				model := gjson.GetBytes(body, "model").String()
				text := "synthetic main reply"
				if calls == 2 {
					titleHeaders = request.Header.Clone()
					if model != "claude-haiku-4-5-20251001" || strings.Contains(gjson.GetBytes(body, "system").Raw, "cc_prompt_id=") {
						t.Fatal("title inherited main request dimensions")
					}
					text = titleOutput
				}
				encoded, _ := json.Marshal(text)
				response := fmt.Sprintf(`{"id":"msg_synthetic_%d","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":3}}`, calls, model, encoded)
				if gjson.GetBytes(body, "stream").Bool() {
					response = fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic_%d\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n", calls, model) +
						"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
						"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + string(encoded) + "}}\n\n" +
						"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
						"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
						"data: {\"type\":\"message_stop\"}\n\n"
				}
				responseHeaders := http.Header{"Request-Id": {fmt.Sprintf("req_synthetic_%d", calls)}}
				if gjson.GetBytes(body, "stream").Bool() {
					responseHeaders.Set("Content-Type", "text/event-stream")
				}
				return &http.Response{StatusCode: 200, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(response)), Request: request, ContentLength: -1}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			headers := http.Header{"X-Session-Id": {"title-event-" + mode}}
			parentID := uuid.NewString()
			mainMetadata := map[string]any{claudeDesktopPromptIDMetadataKey: parentID}
			if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","max_tokens":32,"messages":[{"role":"user","content":"synthetic main"}]}`)}, cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: mainMetadata}); err != nil {
				t.Fatal(err)
			}
			if mainMetadata[claudeDesktopPromptIDMetadataKey] != parentID {
				t.Fatal("main prompt ID changed unexpectedly")
			}
			body := syntheticClaudeDesktopTitleBody(t, executor, auth)
			request := cliproxyexecutor.Request{Model: "claude-haiku-4-5-20251001", Payload: body}
			options := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FromString("claude"), Metadata: map[string]any{helps.ClaudeDesktopParentPromptIDMetadataKey: parentID}}
			switch strings.TrimSuffix(mode, "-negative") {
			case "execute":
				if response, err := executor.Execute(ctx, auth, request, options); err != nil {
					t.Fatal(err)
				} else if !gjson.ValidBytes(response.Payload) || gjson.GetBytes(response.Payload, "type").String() != "message" || gjson.GetBytes(response.Payload, "content.0.text").String() != titleOutput {
					t.Fatal("native Execute did not collect the forced upstream stream")
				} else if response.Headers.Get("Content-Type") != "application/json" {
					t.Fatal("collected native response retained its SSE content type")
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
				ctx = cliproxyexecutor.WithClaudeDesktopParentPromptID(ctx, parentID)
				raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				raw.Header = headers
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
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := promptDeliveredEvents(t, doer)
			if len(events["tengu_session_title_generated"]) != 1 || events["tengu_session_title_generated"][0]["cc_prompt_id"] != parentID || events["tengu_session_title_generated"][0]["success"] != !negative {
				t.Fatalf("title events=%v", events["tengu_session_title_generated"])
			}
			if len(events["tengu_turn_end"]) != 1 {
				t.Fatal("title generated another main turn end")
			}
			for name, values := range titleHeaders {
				if strings.Contains(strings.ToLower(name), "parent_prompt") || strings.Contains(strings.Join(values, ","), parentID) {
					t.Fatal("parent ownership leaked into upstream headers")
				}
			}
			sequence := []string{}
			for _, delivery := range doer.Requests() {
				var batch struct {
					Events []struct {
						Data struct {
							Name     string `json:"event_name"`
							Model    string `json:"model"`
							Metadata string `json:"additional_metadata"`
						} `json:"event_data"`
					} `json:"events"`
				}
				if json.Unmarshal(delivery.Body, &batch) != nil {
					continue
				}
				for _, event := range batch.Events {
					decoded, _ := base64.StdEncoding.DecodeString(event.Data.Metadata)
					if strings.Contains(string(decoded), "PRIVATE_GENERATED_TITLE") {
						t.Fatal("generated title exported")
					}
					if event.Data.Name == "tengu_api_success" && event.Data.Model == "claude-haiku-4-5-20251001" {
						if gjson.GetBytes(decoded, "cc_prompt_id").String() != parentID {
							t.Fatal("title success lost parent")
						}
						sequence = append(sequence, "api")
					}
					if event.Data.Name == "tengu_session_title_generated" {
						if event.Data.Model != "claude-opus-5" {
							t.Fatal("title lifecycle used helper model")
						}
						sequence = append(sequence, "title")
					}
				}
			}
			if strings.Join(sequence, ",") != "api,title" {
				t.Fatalf("completion order=%v", sequence)
			}
		})
	}
}

func syntheticClaudeDesktopTitleBody(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) []byte {
	t.Helper()
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":32000,"stream":true,"tools":[],"thinking":{"type":"disabled"},"output_config":{"format":{"type":"json_schema"}},"system":[{"type":"text","text":"Return a short title."}],"messages":[{"role":"user","content":"synthetic title input"}]}`)
	plan, errPlan := executor.planClaudeDesktopRequest(body, claudeprofile.RoleTitle)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	facts := claudeDesktopRuntimeFacts{LogicalModel: "claude-haiku-4-5-20251001"}
	values, errValues := executor.claudeDesktopArtifactValues(facts, plan)
	if errValues != nil {
		t.Fatal(errValues)
	}
	billing := generateClaudeDesktopBillingHeader(claudeDesktopCCHSigningEnabled(claudeCredsToken(auth), "https://api.anthropic.com/v1/messages?beta=true"), executor.desktopProfile.CodeVersion, body, plan, facts)
	system, errSystem := executor.renderClaudeDesktopSystem(plan, values, billing)
	if errSystem != nil {
		t.Fatal(errSystem)
	}
	body, errSystem = sjson.SetRawBytes(body, "system", system)
	if errSystem != nil {
		t.Fatal(errSystem)
	}
	return body
}
