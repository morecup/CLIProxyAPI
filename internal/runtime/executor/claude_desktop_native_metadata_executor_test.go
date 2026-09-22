package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeAccountNativeMetadataATISAcrossEntries(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "http", "http-stream"} {
		for _, variant := range []string{"positive", "negative", "gate-disabled"} {
			t.Run(mode+"/"+variant, func(t *testing.T) {
				auth := newClaudeAccountRuntimeTestAuth(t, uuid.NewString(), uuid.NewString(), uuid.NewString())
				prepareH73RuntimeAuth(auth)
				gate := variant != "gate-disabled"
				var exposureMu sync.Mutex
				var exposures []json.RawMessage
				var evalPaths, evalBodies, evalHeaders []string
				protocols := map[string]string{"sdk-event-logging": "http/1.1", "desktop-event-logging": "http/2"}
				wireBundle, errBundle := claudeprofile.Load("")
				if errBundle != nil {
					t.Fatal(errBundle)
				}
				for _, delivery := range wireBundle.AuxiliaryTelemetry.All() {
					protocols[delivery.EndpointRole] = delivery.Protocol
				}
				account := NewClaudeAccountExecutorWithOptions(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}}, ClaudeAccountExecutorOptions{
					StartupDoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
						return claudeAccountStartupDoerFunc(func(request *http.Request) (*http.Response, error) {
							payload := []byte(`{}`)
							if role == "startup-sdk-eval" {
								body, _ := io.ReadAll(request.Body)
								exposureMu.Lock()
								evalPaths = append(evalPaths, request.URL.Path)
								evalBodies = append(evalBodies, string(body))
								evalHeaders = append(evalHeaders, request.Header.Get("Authorization"))
								exposureMu.Unlock()
								if request.Header.Get("X-Session-Id") != "" || request.Header.Get("X-Cc-Atis") != "" {
									t.Error("feature evaluation inherited main request headers")
								}
								if strings.HasPrefix(request.URL.Path, "/api/eval-authed/") {
									return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
								}
								payload, _ = json.Marshal(map[string]any{"features": map[string]any{
									"tengu_kestrel_moor":          map[string]any{"value": gate, "source": "experiment", "experiment": map[string]any{"key": "synthetic-atis-experiment"}, "experimentResult": map[string]any{"variationId": 0}},
									"tengu_gb_eval_authed_enable": map[string]any{"value": true, "source": "experiment", "experiment": map[string]any{"key": "synthetic-eval-experiment"}, "experimentResult": map[string]any{"variationId": 1}},
								}})
							}
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload))}, nil
						}), nil
					},
					TelemetryEndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
						return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
							if role == "sdk-event-logging" {
								body, _ := io.ReadAll(request.Body)
								var envelope struct {
									Events []json.RawMessage `json:"events"`
								}
								_ = json.Unmarshal(body, &envelope)
								exposureMu.Lock()
								for _, event := range envelope.Events {
									if gjson.GetBytes(event, "event_type").String() == "GrowthbookExperimentEvent" {
										exposures = append(exposures, event)
									}
								}
								exposureMu.Unlock()
								return &http.Response{StatusCode: 204, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
							}
							if protocols[role] == "http/1.1" {
								return &http.Response{StatusCode: 204, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
							}
							return &http.Response{StatusCode: 204, Proto: "HTTP/2.0", ProtoMajor: 2, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
						})
					},
				})
				t.Cleanup(account.Close)
				if err := account.Provision(auth); err != nil {
					t.Fatal(err)
				}
				runtime := accountRuntimeForAuth(t, account, auth.ID)
				if err := runtime.executor.Activate(auth); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					status := runtime.executor.StartupStatus()
					if status.Completed+status.Failed+status.Skipped == status.EndpointCount {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("synthetic startup worker did not settle")
					}
					time.Sleep(time.Millisecond)
				}
				headers := http.Header{"X-Session-Id": {uuid.NewString()}, "X-Cc-Atis": {"CALLER-PIN-MUST-NOT-WIN"}}
				session := ""
				accountScope, _ := json.Marshal([]string{auth.ID, account.bundle.ProfileID, auth.ProxyURL})
				scope := string(accountScope)
				pin := "v1.Opaque-PIN.a.b.Signature"
				if variant != "positive" {
					pin = ""
				}
				calls := 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					status, reply := 200, `{}`
					responseHeaders := http.Header{}
					switch request.URL.Path {
					case "/api/claude_cli/bootstrap":
						bootstrapPin := pin
						if request.URL.Query().Get("model") == "claude-opus-5" {
							bootstrapPin = "UPDATED-BOOTSTRAP-PIN"
						}
						payload, _ := json.Marshal(map[string]any{"client_data": map[string]any{"atis": bootstrapPin}, "oauth_account": map[string]any{"account_uuid": auth.Metadata["account_uuid"], "organization_uuid": auth.Metadata["organization_uuid"]}})
						reply = string(payload)
					case "/v1/messages/count_tokens":
						reply = `{"input_tokens":10}`
					case "/v1/messages":
						observeDefaultSDKSession(t, request, &session)
						calls++
						want := pin
						if variant == "gate-disabled" && calls == 2 {
							want = "UPDATED-BOOTSTRAP-PIN"
						}
						if request.Header.Get("X-Cc-Atis") != want {
							t.Error("default header differs from native gate/latch or inherited caller pin")
						}
						before := runtime.executor.desktopPrompts.NativeContent(scope, session)
						if before.ATISLatch == nil || *before.ATISLatch != pin {
							t.Error("default request bound its header instead of the independent conversation latch")
						}
						body, _ := io.ReadAll(request.Body)
						reply = `{"id":"msg_metadata","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"synthetic reply"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
						if gjson.GetBytes(body, "stream").Bool() {
							reply = reactiveSummaryStream("msg_metadata", []string{"synthetic reply"})
							responseHeaders.Set("Content-Type", "text/event-stream")
						}
					}
					return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(reply)), ContentLength: -1, Request: request}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				for _, model := range []string{"claude-sonnet-5", "claude-opus-5"} {
					body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "synthetic input " + model}}, "stream": mode == "http-stream"})
					request := cliproxyexecutor.Request{Model: model, Payload: body}
					opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
					switch mode {
					case "execute":
						if _, err := account.Execute(ctx, auth, request, opts); err != nil {
							t.Fatal(err)
						}
					case "stream":
						stream, err := account.ExecuteStream(ctx, auth, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					default:
						raw, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
						raw.Header = headers.Clone()
						response, err := account.HttpRequest(ctx, auth, raw)
						if err != nil {
							t.Fatal(err)
						}
						_, err = io.ReadAll(response.Body)
						errClose := response.Body.Close()
						if err != nil || errClose != nil || response.StatusCode != 200 {
							t.Fatal("default raw response failed", err, errClose)
						}
					}
				}
				if calls != 2 {
					t.Fatal("default two-model scenario did not execute both calls")
				}
				rotated := auth.Clone()
				rotated.Attributes[cliproxyauth.AttributeAPIKey] = "sk-ant-oat01-synthetic-rotated-eval-token"
				if err := runtime.executor.Activate(rotated); err != nil {
					t.Fatal(err)
				}
				deadline = time.Now().Add(3 * time.Second)
				for runtime.executor.StartupStatus().FeatureRefreshCompleted != 1 {
					if time.Now().After(deadline) {
						t.Fatalf("default authenticated fallback did not finish: %+v", runtime.executor.StartupStatus())
					}
					time.Sleep(time.Millisecond)
				}
				if err := runtime.executor.desktopTelemetry.Flush(context.Background()); err != nil {
					t.Fatal(err)
				}
				exposureMu.Lock()
				actualExposures := append([]json.RawMessage(nil), exposures...)
				if len(evalPaths) != 3 || !strings.HasPrefix(evalPaths[0], "/api/eval/") || !strings.HasPrefix(evalPaths[1], "/api/eval-authed/") || !strings.HasPrefix(evalPaths[2], "/api/eval/") || evalBodies[1] != evalBodies[2] || evalHeaders[1] != "Bearer sk-ant-oat01-synthetic-rotated-eval-token" || evalHeaders[1] != evalHeaders[2] {
					t.Error("default evaluation did not consume its real gate or preserve fallback ownership")
				}
				exposureMu.Unlock()
				if len(actualExposures) != 2 {
					t.Fatalf("default real feature reads emitted %d exposures, want two", len(actualExposures))
				}
				event := actualExposures[0]
				if gjson.GetBytes(event, "event_data.session_id").String() != session || gjson.GetBytes(event, "event_data.experiment_id").String() != "synthetic-atis-experiment" || gjson.GetBytes(event, "event_data.model").Exists() || gjson.GetBytes(event, "event_data.event_name").Exists() {
					t.Fatal("default exposure inherited model fields or lost SDK session ownership")
				}
				evalEvent := actualExposures[1]
				if gjson.GetBytes(evalEvent, "event_data.experiment_id").String() != "synthetic-eval-experiment" || gjson.GetBytes(evalEvent, "event_data.session_id").String() != session || gjson.GetBytes(evalEvent, "event_data.model").Exists() || gjson.GetBytes(evalEvent, "event_data.event_name").Exists() {
					t.Fatal("evaluation gate exposure lost its adopted SDK host or inherited model fields")
				}
				tracker := &runtime.executor.desktopPrompts
				content := tracker.NativeContent(scope, session)
				assertDefaultNativeContent(t, content, tracker.SDKHistory(scope, session), session)
				if err := tracker.FlushNativeTranscript(); err != nil {
					t.Fatal(err)
				}
				identity, _ := json.Marshal([]string{scope, session})
				digest := sha256.Sum256(identity)
				index, err := helps.NewClaudeDesktopTranscriptStore(runtime.executor.desktopDurableStatePath, "").LoadTranscript(hex.EncodeToString(digest[:]))
				if err != nil || len(index.UUIDs) != 4 {
					t.Fatal("default metadata damaged durable message UUID indexing", err)
				}
				encoded, err := os.ReadFile(index.Path)
				if err != nil {
					t.Fatal(err)
				}
				var rows []map[string]json.RawMessage
				for _, line := range bytes.Split(bytes.TrimSpace(encoded), []byte{'\n'}) {
					var envelope struct {
						Protector  string
						Ciphertext []byte
					}
					if json.Unmarshal(line, &envelope) != nil {
						t.Fatal("invalid protected envelope")
					}
					plaintext, err := claudedesktop.UnprotectRuntimePayload(index.Path, envelope.Protector, envelope.Ciphertext)
					if err != nil {
						t.Fatal(err)
					}
					var frame struct{ Lines []byte }
					if json.Unmarshal(plaintext, &frame) != nil {
						t.Fatal("invalid protected frame")
					}
					for _, item := range bytes.Split(bytes.TrimSpace(frame.Lines), []byte{'\n'}) {
						var row map[string]json.RawMessage
						if json.Unmarshal(item, &row) != nil {
							t.Fatal("invalid native JSONL row")
						}
						rows = append(rows, row)
					}
				}
				if len(rows) != 5 || string(rows[0]["type"]) != `"user"` || string(rows[1]["type"]) != `"atis-latch"` || string(rows[2]["type"]) != `"assistant"` {
					t.Fatal("default native input/latch/assistant insertion order differs")
				}
				var actualPin string
				_ = json.Unmarshal(rows[1]["atis"], &actualPin)
				if actualPin != pin || len(rows[1]) != 3 {
					t.Fatal("default transcript copied an effective header instead of native latch metadata")
				}
				for i, row := range rows {
					if i != 1 && string(row["userType"]) != `"external"` {
						t.Fatal("default durable transcript omitted mandatory native metadata")
					}
				}
				firstSession := session
				firstHost := runtime.executor.desktopATIS.featureHosts.LookupSession(firstSession)
				if firstHost != runtime.executor.desktopATIS.featureHosts.Warm() || firstHost.SessionID() != firstSession {
					t.Fatal("ordinary main did not adopt its existing warm SDK host")
				}
				headers = http.Header{"X-Session-Id": {uuid.NewString()}}
				session = ""
				for i := 0; i < 2; i++ {
					invokeDefaultHostEntry(t, account, ctx, rotated, mode, headers)
				}
				deadline = time.Now().Add(3 * time.Second)
				for runtime.executor.StartupStatus().FeatureRefreshCompleted != 2 {
					if time.Now().After(deadline) {
						t.Fatalf("second SDK host did not finish its own evaluation: %+v", runtime.executor.StartupStatus())
					}
					time.Sleep(time.Millisecond)
				}
				secondHost := runtime.executor.desktopATIS.featureHosts.LookupSession(session)
				if secondHost == nil || secondHost == firstHost || secondHost.SessionID() != session || firstHost.SessionID() != firstSession {
					t.Fatal("separate main roots shared or relabeled the first SDK host")
				}
				if err := runtime.executor.desktopTelemetry.Flush(context.Background()); err != nil {
					t.Fatal(err)
				}
				exposureMu.Lock()
				bySession := map[string]map[string]int{}
				for _, event := range exposures {
					owner, experiment := gjson.GetBytes(event, "event_data.session_id").String(), gjson.GetBytes(event, "event_data.experiment_id").String()
					if bySession[owner] == nil {
						bySession[owner] = map[string]int{}
					}
					bySession[owner][experiment]++
				}
				for _, owner := range []string{firstSession, session} {
					if bySession[owner]["synthetic-atis-experiment"] != 1 || bySession[owner]["synthetic-eval-experiment"] != 1 {
						t.Error("real per-host reads were lost or duplicated", bySession)
					}
				}
				if len(exposures) != 4 || len(evalBodies) != 5 || gjson.Get(evalBodies[0], "attributes.sessionId").String() != firstSession || gjson.Get(evalBodies[1], "attributes.sessionId").String() != firstSession || gjson.Get(evalBodies[3], "attributes.sessionId").String() != session || evalBodies[3] != evalBodies[4] || session == firstSession || session == helps.ClaudeAgentSessionUUIDForRequest(headers, nil, nil, false) {
					t.Error("default host evaluation and exposure identities do not share their real query owner")
				}
				exposureMu.Unlock()
			})
		}
	}
}

func invokeDefaultHostEntry(t *testing.T, account *ClaudeAccountExecutor, ctx context.Context, auth *cliproxyauth.Auth, mode string, headers http.Header) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "messages": []any{map[string]any{"role": "user", "content": "synthetic independent host input"}}, "stream": mode == "http-stream"})
	request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body}
	opts := cliproxyexecutor.Options{Headers: headers, SourceFormat: sdktranslator.FormatClaude}
	switch mode {
	case "execute":
		if _, err := account.Execute(ctx, auth, request, opts); err != nil {
			t.Fatal(err)
		}
	case "stream":
		stream, err := account.ExecuteStream(ctx, auth, request, opts)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	default:
		raw, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
		raw.Header = headers.Clone()
		response, err := account.HttpRequest(ctx, auth, raw)
		if err != nil {
			t.Fatal(err)
		}
		_, errRead := io.Copy(io.Discard, response.Body)
		errClose := response.Body.Close()
		if errRead != nil || errClose != nil || response.StatusCode != 200 {
			t.Fatal("independent host response failed", errRead, errClose)
		}
	}
}
