package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopCompactionOriginAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "code-http"} {
		for _, kind := range []string{"manual", "auto", "reactive", "unmarked"} {
			for _, status := range []int{200, 400} {
				t.Run(fmt.Sprintf("%s/%s/%d", mode, kind, status), func(t *testing.T) {
					e := newClaudeDesktopTestExecutor(t)
					auth := newClaudeDesktopRawRequestTestAuth(t)
					doer := &claudeDesktopTelemetryTestDoer{}
					manager := claudetelemetry.NewManager(claudetelemetry.Options{StatePath: t.TempDir(), Bundle: e.desktopProfile, DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer }})
					t.Cleanup(manager.Close)
					e.desktopTelemetry = manager
					instruction, err := e.desktopProfile.CompactionInstruction("")
					if err != nil {
						t.Fatal(err)
					}
					body, err := json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": []map[string]any{{"role": "user", "content": instruction}}, "stream": true})
					if err != nil {
						t.Fatal(err)
					}
					headers := http.Header{"X-Session-Id": {"synthetic-compaction-origin"}}
					if kind != "unmarked" {
						// Real Code wire headers are not necessarily canonicalized.
						headers["x-cc-compaction-request"] = []string{kind}
						headers["x-claude-code-compaction"] = []string{kind}
					}
					if mode == "code-http" {
						headers.Set("User-Agent", "claude-cli/2.1.280 (external, claude-desktop, agent-sdk/0.3.280)")
						headers.Set("Anthropic-Client-Platform", "desktop_app")
						headers.Set("X-Claude-Code-Request-Class", "compaction")
					}
					want := kind
					if kind == "unmarked" {
						want = "manual"
					}
					calls := 0
					transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
						payload, errRead := io.ReadAll(request.Body)
						if errRead != nil {
							return nil, errRead
						}
						if request.URL.Path != "/v1/messages" {
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":1}`)), Request: request}, nil
						}
						calls++
						for _, name := range []string{"X-CC-Compaction-Request", "X-Claude-Code-Compaction"} {
							if got := helps.HeaderValueCaseInsensitive(request.Header, name); got != want {
								t.Errorf("%s=%q want=%q", name, got, want)
								return nil, fmt.Errorf("compaction origin mismatch")
							}
						}
						if gjson.GetBytes(payload, "model").String() != "claude-opus-5-5" {
							t.Error("compaction changed the selected model")
							return nil, fmt.Errorf("compaction model mismatch")
						}
						reply := reactiveSummaryStream("msg_origin", []string{"<summary>Synthetic compacted history.</summary>"})
						responseHeaders := http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req_origin"}}
						if status == 400 {
							reply = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
							responseHeaders.Set("Content-Type", "application/json")
						}
						return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
					})
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
					req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: body}
					opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
					switch mode {
					case "execute":
						_, err = e.Execute(ctx, auth, req, opts)
					case "stream":
						var result *cliproxyexecutor.StreamResult
						result, err = e.ExecuteStream(ctx, auth, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					default:
						raw, errNew := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(body)))
						if errNew != nil {
							t.Fatal(errNew)
						}
						raw.Header = headers.Clone()
						response, errHTTP := e.HttpRequest(ctx, auth, raw)
						err = errHTTP
						if response != nil {
							_, errRead := io.Copy(io.Discard, response.Body)
							errClose := response.Body.Close()
							if errRead != nil || errClose != nil || response.StatusCode != status {
								t.Fatalf("response status=%d read=%v close=%v", response.StatusCode, errRead, errClose)
							}
						}
					}
					if status == 200 && err != nil {
						t.Fatal(err)
					}
					if status == 400 && (mode == "execute" || mode == "stream") && err == nil {
						t.Fatal("helper PTL was not returned to its caller")
					}
					if calls != 1 {
						t.Fatalf("explicit compaction recursively triggered recovery: calls=%d", calls)
					}
					if errFlush := manager.Flush(t.Context()); errFlush != nil {
						t.Fatal(errFlush)
					}
					for name, events := range promptDeliveredEvents(t, doer) {
						if (strings.HasPrefix(name, "tengu_reactive_compact_") || strings.HasPrefix(name, "tengu_auto_compact_")) && len(events) != 0 {
							t.Fatalf("wire hint fabricated an owned compaction lifecycle: %s", name)
						}
					}
				})
			}
		}
	}
}
