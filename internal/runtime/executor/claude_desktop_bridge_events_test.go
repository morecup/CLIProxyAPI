package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestClaudeDesktopBridgeTeardownDeliveryAcrossActualEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit-stop-%t", mode, stop), func(t *testing.T) {
				var mu sync.Mutex
				type observed struct {
					account string
					data    json.RawMessage
				}
				var events []observed
				protocols := map[string]string{"sdk-event-logging": "http/1.1", "desktop-event-logging": "http/2"}
				bundle, err := claudeprofile.BuiltinV140609()
				if err != nil {
					t.Fatal(err)
				}
				for _, delivery := range bundle.AuxiliaryTelemetry.All() {
					protocols[delivery.EndpointRole] = delivery.Protocol
				}
				e, auths := newExecutionSessionAccountTest(t, func(_ string, role string, auth *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
					return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						if role == "sdk-event-logging" {
							for _, event := range gjson.GetBytes(body, "events").Array() {
								if event.Get("event_data.event_name").String() == "tengu_bridge_repl_teardown" {
									mu.Lock()
									events = append(events, observed{auth.ID, json.RawMessage(event.Get("event_data").Raw)})
									mu.Unlock()
								}
							}
						}
						response := &http.Response{StatusCode: 204, Proto: "HTTP/2.0", ProtoMajor: 2, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
						if protocols[role] == "http/1.1" {
							response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
						}
						return response, nil
					})
				})
				for index, auth := range auths {
					auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
					inner := accountRuntimeForAuth(t, e, auth.ID).executor
					inner.desktopControlPlane.Close()
					inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile, DisableLoops: true,
						DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
							return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
								body, status := `{}`, 200
								switch {
								case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
									body = fmt.Sprintf(`{"session":{"id":"cse_event_%d"}}`, index)
								case strings.HasSuffix(request.URL.Path, "/bridge"):
									body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
								case strings.HasSuffix(request.URL.Path, "/archive"):
									status = 204
								}
								return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
							}), nil
						}})
				}
				owners := make(map[string]claudeDesktopQueryContext)
				prompts := make(map[string]string)
				promptPattern := regexp.MustCompile(`cc_prompt_id=([a-f0-9-]{36})`)
				for _, auth := range auths {
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
						if request.URL.Path == "/v1/messages" {
							owners[auth.ID] = request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
							reader, err := request.GetBody()
							if err != nil {
								return nil, err
							}
							body, err := io.ReadAll(reader)
							_ = reader.Close()
							if err != nil {
								return nil, err
							}
							if match := promptPattern.FindSubmatch(body); len(match) == 2 {
								prompts[auth.ID] = string(match[1])
							}
						}
						return executionSessionTestResponse(t, request), nil
					})))
					if err := invokeQueryLifetimeEntry(ctx, e, auth, http.Header{"X-Session-Id": {uuid.NewString()}}, mode, nil, desktopExecutionMetadata("bridge-event")); err != nil {
						t.Fatal(err)
					}
				}
				for _, auth := range auths {
					inner := accountRuntimeForAuth(t, e, auth.ID).executor
					owner := owners[auth.ID]
					if stop {
						operation := cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: owner.desktopSessionID, ExpectedQueryID: owner.host.ID()}
						if _, err := e.StopDesktopSession(t.Context(), auth.ID, operation); err != nil {
							t.Fatal(err)
						}
						if _, err := e.StopDesktopSession(t.Context(), auth.ID, operation); err != nil {
							t.Fatal(err)
						}
						if err := inner.desktopTelemetry.Flush(t.Context()); err != nil {
							t.Fatal(err)
						}
					} else {
						inner.Close()
						inner.Close()
					}
				}
				mu.Lock()
				result := append([]observed(nil), events...)
				mu.Unlock()
				if len(result) != len(auths) {
					t.Fatal("teardown was dropped at application close or duplicated", len(result))
				}
				seen := make(map[string]bool)
				for _, event := range result {
					owner := owners[event.account]
					if seen[event.account] || gjson.GetBytes(event.data, "session_id").String() != owner.session || gjson.GetBytes(event.data, "model").String() != "claude-opus-5" {
						t.Fatal("wrong or duplicate owner", event.account)
					}
					seen[event.account] = true
					meta, err := base64.StdEncoding.DecodeString(gjson.GetBytes(event.data, "additional_metadata").String())
					if err != nil {
						t.Fatal(err)
					}
					if prompts[event.account] == "" || gjson.GetBytes(meta, "cc_prompt_id").String() != prompts[event.account] || gjson.GetBytes(meta, "archive_http_status").Int() != 204 || !gjson.GetBytes(meta, "archive_ok").Bool() {
						t.Fatal("lost request/archive facts", string(meta))
					}
				}
			})
		}
	}
}
