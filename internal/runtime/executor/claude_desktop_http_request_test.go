package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func newClaudeDesktopRawRequestTestAuth(t *testing.T) *cliproxyauth.Auth {
	t.Helper()
	identity := claudedesktop.AccountIdentity{
		AccountUUID:      "11111111-1111-4111-8111-111111111111",
		OrganizationUUID: "22222222-2222-4222-8222-222222222222",
	}
	device := claudedesktop.TrustedDevice{
		DeviceID:    "33333333-3333-4333-8333-333333333333",
		DeviceToken: "test-device-token",
		DisplayName: "test-device",
	}
	authID, errAuthID := claudedesktop.StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := claudedesktop.NewEnrollment(authID, identity, device, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":                              "sk-ant-oat-test-token",
			"refresh_token":                             "test-refresh-token",
			"auth_kind":                                 cliproxyauth.AuthKindOAuth,
			"account_uuid":                              identity.AccountUUID,
			"organization_uuid":                         identity.OrganizationUUID,
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"claude_device_ids":                         []string{claudedesktop.RequestDeviceID(device.DeviceID)},
		},
	}
}

func TestClaudeDesktopHttpRequestUsesProfilePlannerAndTransport(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var capturedBody []byte
	var capturedHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/messages/count_tokens" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    req,
			}, nil
		}
		var errRead error
		capturedBody, errRead = io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		capturedHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"request-id": []string{"req_raw_desktop"}},
			Body:          io.NopCloser(strings.NewReader(`{"id":"msg_raw_desktop","type":"message","usage":{"input_tokens":7}}`)),
			ContentLength: -1,
			Request:       req,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	body := `{"top_p":0.5,"stream":false,"messages":[{"role":"user","content":"hello"}],"model":"claude-sonnet-5","max_tokens":1,"tools":[]}`
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(body))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req.Header.Set("Anthropic-Beta", "caller-unobserved-beta")
	req.Header.Set("X-Unprofiled", "remove-me")
	response, errDo := executor.HttpRequest(ctx, auth, req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	if _, errRead := io.ReadAll(response.Body); errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	if len(capturedBody) == 0 {
		t.Fatal("raw HttpRequest did not reach the Desktop transport")
	}
	if !bytes.HasPrefix(capturedBody, []byte(`{"model":"claude-sonnet-5","messages":`)) {
		t.Fatalf("body does not use captured top-level order: %s", capturedBody)
	}
	for path, want := range map[string]string{
		"thinking.type":        "adaptive",
		"thinking.display":     "updates",
		"output_config.effort": "high",
		"metadata.user_id":     "",
	} {
		got := gjson.GetBytes(capturedBody, path).String()
		if path == "metadata.user_id" {
			if got == "" || !gjson.Valid(got) {
				t.Fatalf("%s was not rendered as the Desktop identity envelope: %q", path, got)
			}
			continue
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	if got := gjson.GetBytes(capturedBody, "max_tokens").Int(); got != 64000 {
		t.Fatalf("max_tokens = %d, want 64000", got)
	}
	if gjson.GetBytes(capturedBody, "top_p").Exists() {
		t.Fatal("unprofiled caller sampling field survived raw HttpRequest")
	}
	if got := len(gjson.GetBytes(capturedBody, "system").Array()); got != 4 {
		t.Fatalf("system blocks = %d, want 4", got)
	}
	if capturedHeaders.Get("X-Unprofiled") != "" {
		t.Fatal("unprofiled caller header survived raw HttpRequest")
	}
	if got := headerValueFold(capturedHeaders, "x-app"); got != "cli" {
		t.Fatalf("x-app = %q, want cli", got)
	}
	if got := headerValueFold(capturedHeaders, "X-Stainless-Timeout"); got != "900" {
		t.Fatalf("X-Stainless-Timeout = %q, want 900", got)
	}
	if got := headerValueFold(capturedHeaders, "anthropic-beta"); got == "" || strings.Contains(got, "caller-unobserved-beta") {
		t.Fatalf("anthropic-beta was not profile-owned: %q", got)
	}
}

func TestClaudeDesktopHttpRequestRestoresCompressedStreamingToolAlias(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	t.Cleanup(executor.Close)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var upstreamAlias string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages/count_tokens" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    request,
			}, nil
		}
		body, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			return nil, errRead
		}
		upstreamAlias = claudeDesktopAliasedToolName(t, body, "Agent")
		stream := "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_alias_stream","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_alias","name":"` + upstreamAlias + `","input":{}}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, errWrite := writer.Write([]byte(stream)); errWrite != nil {
			return nil, errWrite
		}
		if errClose := writer.Close(); errClose != nil {
			return nil, errClose
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}},
			Body:          io.NopCloser(bytes.NewReader(compressed.Bytes())),
			ContentLength: int64(compressed.Len()),
			Request:       request,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"use Agent"}],"max_tokens":64,"stream":true,"tools":[{"name":"Agent","description":"launch agent","input_schema":{"type":"object"}}]}`))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	response, errDo := executor.HttpRequest(ctx, auth, request)
	if errDo != nil {
		t.Fatal(errDo)
	}
	responseBody, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if upstreamAlias == "" || upstreamAlias == "Agent" {
		t.Fatalf("upstream Agent alias = %q", upstreamAlias)
	}
	if bytes.Contains(responseBody, []byte(upstreamAlias)) || !bytes.Contains(responseBody, []byte(`"name":"Agent"`)) {
		t.Fatalf("stream tool alias was not restored: alias=%q body=%s", upstreamAlias, responseBody)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("restored stream Content-Encoding = %q, want empty", got)
	}
	if !response.Uncompressed || response.ContentLength != -1 {
		t.Fatalf("restored stream metadata uncompressed=%t contentLength=%d", response.Uncompressed, response.ContentLength)
	}
}

func TestClaudeDesktopHttpRequestRestoresBrotliNonStreamingToolAlias(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	t.Cleanup(executor.Close)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var upstreamAlias string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/messages/count_tokens" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    request,
			}, nil
		}
		body, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			return nil, errRead
		}
		upstreamAlias = claudeDesktopAliasedToolName(t, body, "Agent")
		payload := []byte(`{"id":"msg_alias_json","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_alias","name":"` + upstreamAlias + `","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
		var compressed bytes.Buffer
		writer := brotli.NewWriter(&compressed)
		if _, errWrite := writer.Write(payload); errWrite != nil {
			return nil, errWrite
		}
		if errClose := writer.Close(); errClose != nil {
			return nil, errClose
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"br"}},
			Body:          io.NopCloser(bytes.NewReader(compressed.Bytes())),
			ContentLength: int64(compressed.Len()),
			Request:       request,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"use Agent"}],"max_tokens":64,"stream":false,"tools":[{"name":"Agent","description":"launch agent","input_schema":{"type":"object"}}]}`))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	response, errDo := executor.HttpRequest(ctx, auth, request)
	if errDo != nil {
		t.Fatal(errDo)
	}
	responseBody, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if upstreamAlias == "" || upstreamAlias == "Agent" {
		t.Fatalf("upstream Agent alias = %q", upstreamAlias)
	}
	if bytes.Contains(responseBody, []byte(upstreamAlias)) || gjson.GetBytes(responseBody, "content.0.name").String() != "Agent" {
		t.Fatalf("JSON tool alias was not restored: alias=%q body=%s", upstreamAlias, responseBody)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("restored JSON Content-Encoding = %q, want empty", got)
	}
	if !response.Uncompressed || response.ContentLength != int64(len(responseBody)) {
		t.Fatalf("restored JSON metadata uncompressed=%t contentLength=%d body=%d", response.Uncompressed, response.ContentLength, len(responseBody))
	}
}

func claudeDesktopAliasedToolName(t *testing.T, body []byte, semantic string) string {
	t.Helper()
	suffix := "_" + semantic
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		name := tool.Get("name").String()
		if strings.HasPrefix(name, "mcp__") && strings.HasSuffix(name, suffix) {
			return name
		}
	}
	t.Fatalf("upstream request did not contain an MCP alias for %q: %s", semantic, body)
	return ""
}

func TestClaudeDesktopVerifiedCodeWireRequestUsesOwnedProfile(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newClaudeDesktopRawRequestTestAuth(t)
	sessionID := "44444444-4444-4444-8444-444444444444"
	executor.desktopContexts = helps.NewClaudeDesktopContextStore(t.TempDir(), "")
	sourceHistory := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"CALLER_HISTORY_OLD_USER"}]},{"role":"assistant","content":[{"type":"text","text":"CALLER_HISTORY_OLD_ASSISTANT","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	adoptedHistory := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"assistant","content":[{"type":"text","text":"PROGRAM_OWNED_HISTORY_SUMMARY"}]}]}`)
	executor.desktopATIS = newClaudeDesktopATISManager(t.TempDir(), executor.desktopProfile.CodeVersion, func(ctx context.Context, selected *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
		return executor.desktopTransports.EndpointClient(ctx, executor.cfg, selected, executor.desktopProfile, claudeDesktopATISEndpointRole)
	})
	executor.desktopATIS.profileID = executor.desktopProfile.ProfileID
	t.Cleanup(executor.Close)

	var bootstrapHeaders http.Header
	var capturedBody []byte
	var capturedHeaders http.Header
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/claude_cli/bootstrap":
			bootstrapHeaders = request.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"client_data":{"experimentKey":"probe","atis":"0123456789abcdef"},"oauth_account":{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"22222222-2222-4222-8222-222222222222"}}`)),
				Request:    request,
			}, nil
		case "/v1/messages":
			var errRead error
			capturedBody, errRead = io.ReadAll(request.Body)
			if errRead != nil {
				return nil, errRead
			}
			capturedHeaders = request.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "request-id": []string{"req_verified_code"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_verified_code\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")),
				Request:    request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
		}
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"CALLER_HISTORY_OLD_USER"}]},{"role":"assistant","content":[{"type":"text","text":"CALLER_HISTORY_OLD_ASSISTANT","cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"role":"user","content":[{"type":"text","text":"LIVE_HISTORY_TAIL"}]}],"system":[{"type":"text","text":"CALLER_SYSTEM_ALPHA"},{"type":"text","text":"CALLER_SYSTEM_BETA"}],"tools":[{"name":"sample","description":"sample","input_schema":{"type":"object"}}],"metadata":{"user_id":"caller-owned"},"max_tokens":123,"thinking":{"type":"enabled","display":"caller-owned"},"context_management":{"edits":[{"type":"caller-owned"}]},"output_config":{"effort":"low"},"diagnostics":{"previous_message_id":"caller-owned"},"stream":true}`)
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	for name, value := range map[string]string{
		"Accept":                                    "application/json",
		"Accept-Encoding":                           "gzip, deflate, br, zstd",
		"Content-Type":                              "application/json",
		"User-Agent":                                "claude-cli/999.999 (caller-owned)",
		"X-Claude-Code-Session-Id":                  sessionID,
		"X-Stainless-Arch":                          "x64",
		"X-Stainless-Lang":                          "js",
		"X-Stainless-OS":                            "Windows",
		"X-Stainless-Package-Version":               "0.112.1",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Runtime-Version":               "v26.3.0",
		"X-Stainless-Timeout":                       "900",
		"anthropic-beta":                            "caller-unobserved-beta",
		"anthropic-client-platform":                 "desktop_app",
		"anthropic-client-version":                  "caller-owned-version",
		"anthropic-dangerous-direct-browser-access": "true",
		"anthropic-version":                         "2023-06-01",
		"x-app":                                     "cli",
		"x-claude-code-request-class":               "main",
		"x-client-request-id":                       "55555555-5555-4555-8555-555555555555",
	} {
		request.Header.Set(name, value)
	}
	request.Header.Set("Authorization", "Bearer caller-token-must-not-survive")
	request.Header.Set("Cookie", "caller-cookie-must-not-survive")
	ownedCallerSessionID := claudeDesktopOwnedSessionUUID(request.Header, body, true)
	if ownedCallerSessionID == "" || ownedCallerSessionID == sessionID || ownedCallerSessionID != claudeDesktopOwnedSessionUUID(request.Header, body, true) {
		t.Fatalf("verified Code session was not mapped to a stable program-owned identity: %q", ownedCallerSessionID)
	}
	ctx, ownedSessionID := executor.bindClaudeDesktopQueryContext(ctx, auth, ownedCallerSessionID, claudeprofile.RoleMain)
	if ownedSessionID == "" || ownedSessionID == sessionID {
		t.Fatalf("verified Code session did not resolve to an owned native identity: %q", ownedSessionID)
	}
	request = request.WithContext(ctx)
	historyLease, _, errHistory := executor.desktopContexts.Resume(t.Context(), auth, executor.desktopProfile.ProfileID, ownedSessionID, sourceHistory)
	if errHistory != nil {
		t.Fatal(errHistory)
	}
	if errHistory = historyLease.SaveAdopted(t.Context(), sourceHistory, adoptedHistory); errHistory != nil {
		t.Fatal(errHistory)
	}

	response, errDo := executor.HttpRequest(ctx, auth, request)
	if errDo != nil {
		t.Fatal(errDo)
	}
	if _, errRead := io.Copy(io.Discard, response.Body); errRead != nil {
		t.Fatal(errRead)
	}
	_ = response.Body.Close()

	if bytes.Equal(capturedBody, body) {
		t.Fatal("verified Desktop Code request bypassed the owned request profile")
	}
	if capturedHeaders.Get("Cookie") != "" || capturedHeaders.Get("Authorization") != "Bearer sk-ant-oat-test-token" {
		t.Fatal("verified Desktop Code request did not replace caller-owned credentials")
	}
	if got := headerValueFold(capturedHeaders, "User-Agent"); got != executor.desktopProfile.Software.UserAgent {
		t.Fatalf("upstream User-Agent = %q, want owned profile %q", got, executor.desktopProfile.Software.UserAgent)
	}
	if got := headerValueFold(capturedHeaders, "anthropic-beta"); got == "" || strings.Contains(got, "caller-unobserved-beta") {
		t.Fatalf("upstream anthropic-beta was not profile-owned: %q", got)
	}
	if got := headerValueFold(capturedHeaders, "anthropic-client-version"); got == "caller-owned-version" {
		t.Fatal("caller-owned anthropic-client-version survived")
	}
	if got := headerValueFold(capturedHeaders, "x-client-request-id"); got == "" || got == "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("x-client-request-id was not program-owned: %q", got)
	}
	if got := headerValueFold(capturedHeaders, "x-claude-code-session-id"); got != ownedSessionID {
		t.Fatalf("x-claude-code-session-id = %q, want program-owned %q", got, ownedSessionID)
	}
	if got := headerValueFold(capturedHeaders, "x-cc-atis"); got != "0123456789abcdef" {
		t.Fatalf("x-cc-atis = %q, want owned assignment", got)
	}
	if got := gjson.GetBytes(capturedBody, "max_tokens").Int(); got != 64000 {
		t.Fatalf("max_tokens = %d, want owned profile 64000", got)
	}
	if got := gjson.GetBytes(capturedBody, "thinking.display").String(); got != "updates" {
		t.Fatalf("thinking.display = %q, want owned profile updates", got)
	}
	if got := gjson.GetBytes(capturedBody, "metadata.user_id").String(); got == "caller-owned" || !gjson.Valid(got) {
		t.Fatalf("metadata.user_id was not program-owned: %q", got)
	}
	system := gjson.GetBytes(capturedBody, "system")
	if len(system.Array()) != 4 || strings.Contains(system.Raw, "CALLER_SYSTEM_ALPHA") || strings.Contains(system.Raw, "CALLER_SYSTEM_BETA") {
		t.Fatalf("top-level system was not rebuilt from the owned bundle: %s", system.Raw)
	}
	if strings.Contains(string(capturedBody), "CALLER_SYSTEM_ALPHA") || strings.Contains(string(capturedBody), "CALLER_SYSTEM_BETA") {
		t.Fatal("caller system survived the program-owned Code request rebuild")
	}
	if strings.Contains(string(capturedBody), "CALLER_HISTORY_OLD_USER") || strings.Contains(string(capturedBody), "CALLER_HISTORY_OLD_ASSISTANT") {
		t.Fatal("an exact previously adopted history prefix survived the owned context rewrite")
	}
	if !strings.Contains(string(capturedBody), "PROGRAM_OWNED_HISTORY_SUMMARY") || !strings.Contains(string(capturedBody), "LIVE_HISTORY_TAIL") {
		t.Fatal("owned history summary or current semantic tail was lost while rebuilding the request")
	}
	if got := headerValueFold(bootstrapHeaders, "User-Agent"); got == "" || strings.Contains(got, "999.999") {
		t.Fatalf("bootstrap User-Agent was not program-owned: %q", got)
	}
}

func TestClaudeDesktopHttpRequestTelemetryObservesFinalWireRequestAndResponse(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	if executor.desktopTelemetry != nil {
		executor.desktopTelemetry.Close()
	}
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	telemetryDoer := &claudeDesktopTelemetryTestDoer{}
	manager := claudetelemetry.NewManager(claudetelemetry.Options{
		StatePath: t.TempDir(),
		Bundle:    bundle,
		DoerFactory: func(string) claudetelemetry.HTTPDoer {
			return telemetryDoer
		},
	})
	t.Cleanup(manager.Close)
	executor.desktopTelemetry = manager
	auth := newClaudeDesktopRawRequestTestAuth(t)

	var capturedBody []byte
	var capturedHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/messages/count_tokens" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    req,
			}, nil
		}
		capturedBody, _ = io.ReadAll(req.Body)
		capturedHeaders = req.Header.Clone()
		responseHeaders := make(http.Header)
		responseHeaders.Set("request-id", "req_final_wire")
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        responseHeaders,
			Body:          io.NopCloser(strings.NewReader(`{"id":"msg_final_wire","type":"message","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`)),
			ContentLength: -1,
			Request:       req,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"FINAL_BODY_SENTINEL"}],"max_tokens":1,"tools":[]}`))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req.Header.Set("Anthropic-Beta", "caller-unobserved-beta")
	req.Header.Set("Cookie", "session=COOKIE_SENTINEL")

	response, errDo := executor.HttpRequest(ctx, auth, req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	// Force an early preparation batch so the assertion cannot accidentally
	// depend on the success event being in the first SDK delivery request.
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	if _, errRead := io.Copy(io.Discard, response.Body); errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}

	var sdkRequest *claudeDesktopTelemetryRecordedRequest
	for _, recorded := range telemetryDoer.Requests() {
		if !strings.HasPrefix(recorded.URL, "https://api.anthropic.com/") {
			continue
		}
		if recorded.Header.Get("Cookie") != "" || strings.Contains(string(recorded.Body), "COOKIE_SENTINEL") || strings.Contains(string(recorded.Body), "FINAL_BODY_SENTINEL") || strings.Contains(string(recorded.Body), "sk-ant-oat-test-token") {
			t.Fatal("SDK telemetry batch leaked caller content or credentials")
		}
		for _, event := range gjson.GetBytes(recorded.Body, "events").Array() {
			if event.Get("event_data.event_name").String() != "tengu_api_success" {
				continue
			}
			if sdkRequest != nil {
				t.Fatal("SDK success event was delivered more than once")
			}
			requestCopy := recorded
			sdkRequest = &requestCopy
		}
	}
	if sdkRequest == nil {
		t.Fatal("SDK success event was not delivered in any batch")
	}
	if sdkRequest.Header.Get("Cookie") != "" || strings.Contains(string(sdkRequest.Body), "COOKIE_SENTINEL") || strings.Contains(string(sdkRequest.Body), "FINAL_BODY_SENTINEL") || strings.Contains(string(sdkRequest.Body), "sk-ant-oat-test-token") {
		t.Fatalf("SDK telemetry leaked caller content or credentials: headers=%v body=%s", sdkRequest.Header, sdkRequest.Body)
	}

	var batch struct {
		Events []struct {
			EventData struct {
				EventName          string `json:"event_name"`
				Model              string `json:"model"`
				Betas              string `json:"betas"`
				AdditionalMetadata string `json:"additional_metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(sdkRequest.Body, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	var success map[string]any
	for _, event := range batch.Events {
		if event.EventData.EventName != "tengu_api_success" {
			continue
		}
		if event.EventData.Model != "claude-sonnet-5" || event.EventData.Betas != headerValueFold(capturedHeaders, "anthropic-beta") || strings.Contains(event.EventData.Betas, "caller-unobserved-beta") {
			t.Fatalf("SDK event did not use final wire model/betas: model=%q betas=%q", event.EventData.Model, event.EventData.Betas)
		}
		metadata, errBase64 := base64.StdEncoding.DecodeString(event.EventData.AdditionalMetadata)
		if errBase64 != nil {
			t.Fatal(errBase64)
		}
		if errMetadata := json.Unmarshal(metadata, &success); errMetadata != nil {
			t.Fatal(errMetadata)
		}
	}
	if success == nil {
		t.Fatal("SDK success event was not emitted")
	}
	for key, want := range map[string]any{
		"requestId":           "req_final_wire",
		"stop_reason":         "end_turn",
		"requestBodyChars":    float64(claudeDesktopJavaScriptStringLength(string(capturedBody))),
		"effort_level":        "high",
		"inputTokens":         float64(11),
		"outputTokens":        float64(2),
		"cachedInputTokens":   float64(4),
		"uncachedInputTokens": float64(3),
	} {
		if got := success[key]; got != want {
			t.Fatalf("SDK final-wire metadata %s = %#v, want %#v", key, got, want)
		}
	}
	if chars, _ := success["inputTextCharLength"].(float64); chars <= float64(len("FINAL_BODY_SENTINEL")) {
		t.Fatalf("inputTextCharLength = %v, want final request aggregate", success["inputTextCharLength"])
	}
}

func TestClaudeDesktopHttpRequestCountTokensUsesCapturedShape(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var capturedBody []byte
	var capturedHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		capturedHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(`{"input_tokens":3}`)),
			ContentLength: -1,
			Request:       req,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens?beta=true", strings.NewReader(`{"tools":[],"system":"drop","messages":[],"model":"claude-opus-5","stream":true}`))
	response, errDo := executor.HttpRequest(ctx, auth, req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if string(capturedBody) != `{"model":"claude-opus-5","messages":[],"tools":[]}` {
		t.Fatalf("count_tokens body = %s", capturedBody)
	}
	if got := headerValueFold(capturedHeaders, "X-Stainless-Timeout"); got != "" {
		t.Fatalf("count_tokens timeout = %q, want omitted", got)
	}
}

func TestClaudeDesktopHttpRequestReusesIdentityAcrossRequestReplay(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var clientRequestIDs []string
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/messages/count_tokens" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    req,
			}, nil
		}
		clientRequestIDs = append(clientRequestIDs, headerValueFold(req.Header, "x-client-request-id"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_replay","type":"message","usage":{"input_tokens":1}}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"tools":[]}`))
	for range 2 {
		response, errDo := executor.HttpRequest(ctx, auth, req)
		if errDo != nil {
			t.Fatal(errDo)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if len(clientRequestIDs) != 2 || clientRequestIDs[0] == "" || clientRequestIDs[0] != clientRequestIDs[1] {
		t.Fatalf("replayed x-client-request-id values = %#v", clientRequestIDs)
	}
}

func TestClaudeDesktopObservedResponseParsesFinalSSELineWithoutNewline(t *testing.T) {
	observed := &claudeDesktopObservedResponseBody{
		body:      io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_final_line\"}}\n\ndata: {\"type\":\"message_stop\"}")),
		ctx:       t.Context(),
		streaming: true,
	}
	if _, errRead := io.Copy(io.Discard, observed); errRead != nil {
		t.Fatal(errRead)
	}
	if !observed.completed || observed.messageID != "msg_final_line" || !observed.finished {
		t.Fatalf("final SSE state = completed:%t message:%q finished:%t", observed.completed, observed.messageID, observed.finished)
	}
}

type claudeDesktopBlockingBody struct {
	readStarted chan struct{}
	closed      chan struct{}
	once        sync.Once
}

func (b *claudeDesktopBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.readStarted) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *claudeDesktopBlockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestClaudeDesktopObservedResponseCloseUnblocksConcurrentRead(t *testing.T) {
	body := &claudeDesktopBlockingBody{readStarted: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	observed := &claudeDesktopObservedResponseBody{body: body, ctx: ctx, streaming: true}
	readDone := make(chan error, 1)
	go func() {
		_, errRead := observed.Read(make([]byte, 1))
		readDone <- errRead
	}()
	<-body.readStarted
	cancel()
	if errClose := observed.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	select {
	case errRead := <-readDone:
		if !errors.Is(errRead, io.ErrClosedPipe) {
			t.Fatalf("Read error = %v", errRead)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
	if !observed.finished {
		t.Fatal("observer was not finalized")
	}
}

func headerValueFold(headers http.Header, name string) string {
	for candidate, values := range headers {
		if strings.EqualFold(candidate, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
