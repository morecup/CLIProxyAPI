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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func claudeOAuthTestMetadata() map[string]any {
	return map[string]any{
		"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		claudeauth.ClaudeDeviceIDsMetadataKey: []string{
			"0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

func malformedClaudeTreeSignatureForClaudeExecutorTest() string {
	return base64.StdEncoding.EncodeToString([]byte{0x12, 0xFF, 0xFE, 0xFD})
}

func newClaudeHeaderTestRequest(t *testing.T, incoming http.Header) *http.Request {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginReq := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	ginReq.Header = incoming.Clone()
	ginCtx.Request = ginReq

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	return req.WithContext(context.WithValue(req.Context(), "gin", ginCtx))
}

func assertClaudeFingerprint(t *testing.T, headers http.Header, userAgent, pkgVersion, runtimeVersion, osName, arch string) {
	t.Helper()

	if got := headers.Get("User-Agent"); got != userAgent {
		t.Fatalf("User-Agent = %q, want %q", got, userAgent)
	}
	if got := headers.Get("X-Stainless-Package-Version"); got != pkgVersion {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, pkgVersion)
	}
	if got := headers.Get("X-Stainless-Runtime-Version"); got != runtimeVersion {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, runtimeVersion)
	}
	if got := headers.Get("X-Stainless-Os"); got != osName {
		t.Fatalf("X-Stainless-Os = %q, want %q", got, osName)
	}
	if got := headers.Get("X-Stainless-Arch"); got != arch {
		t.Fatalf("X-Stainless-Arch = %q, want %q", got, arch)
	}
}

func TestApplyAnthropicCompatibleHeaders_FastModeBetaIsConditional(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "omitted speed excludes fast mode beta",
			body: `{"model":"claude-opus-5"}`,
			want: "",
		},
		{
			name: "fast speed appends fast mode beta",
			body: `{"model":"claude-opus-5","speed":"fast"}`,
			want: claudeFastModeBeta,
		},
		{
			name: "explicit body beta appends fast mode beta",
			body: `{"model":"claude-opus-5","betas":["fast-mode-2026-02-01"]}`,
			want: claudeFastModeBeta,
		},
	}

	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-fast-mode-beta"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extraBetas, body := extractAndRemoveBetas([]byte(tt.body))
			req := newClaudeHeaderTestRequest(t, nil)
			if errApply := applyClaudeHeaders(req, auth, "key-fast-mode-beta", false, extraBetas, body, nil); errApply != nil {
				t.Fatalf("applyClaudeHeaders() error = %v", errApply)
			}
			if got := req.Header.Get("Anthropic-Beta"); got != tt.want {
				t.Fatalf("Anthropic-Beta = %q, want %q", got, tt.want)
			}
		})
	}
}

func assertClaudeCredentialIdentity(t *testing.T, body []byte, headers http.Header, deviceIDs []string, accountUUID string) {
	t.Helper()
	userID := gjson.GetBytes(body, "metadata.user_id").String()
	deviceID := gjson.Get(userID, "device_id").String()
	inPool := false
	for _, candidate := range deviceIDs {
		if deviceID == candidate {
			inPool = true
			break
		}
	}
	if !inPool {
		t.Fatalf("device_id = %q, want selected credential device pool entry", deviceID)
	}
	if got := gjson.Get(userID, "account_uuid").String(); got != accountUUID {
		t.Fatalf("account_uuid = %q, want selected credential account %q", got, accountUUID)
	}
	sessionID := gjson.Get(userID, "session_id").String()
	if sessionID == "" || sessionID != headers.Get("X-Claude-Code-Session-Id") {
		t.Fatalf("metadata session_id = %q, header session ID = %q", sessionID, headers.Get("X-Claude-Code-Session-Id"))
	}
	resigned, errResign := finalizeAnthropicMessagesBodyCCH(body)
	if errResign != nil {
		t.Fatalf("re-finalize Claude CCH: %v", errResign)
	}
	if !bytes.Equal(resigned, body) {
		t.Fatal("Claude CCH was calculated before final credential metadata rewrite")
	}
}

func TestApplyAnthropicCompatibleHeaders_UsesAPIKeyWithoutDesktopFingerprint(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-header-test"}}
	req := newClaudeHeaderTestRequest(t, nil)
	if errHeaders := applyClaudeHeaders(req, auth, "key-header-test", false, nil, nil, nil); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty for first-party API key", got)
	}
	if got := req.Header.Get("x-api-key"); got != "key-header-test" {
		t.Fatalf("x-api-key = %q, want caller API key", got)
	}
	if got := req.Header.Get("Anthropic-Dangerous-Direct-Browser-Access"); got != "" {
		t.Fatalf("Anthropic-Dangerous-Direct-Browser-Access = %q, want absent", got)
	}
	if got := req.Header.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("Anthropic-Beta = %q, want no synthesized Desktop beta", got)
	}
}

func TestApplyClaudeHeaders_EmptyAPIKey_OmitsAuthHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"auth_kind":           "apikey",
			"base_url":            "https://custom-claude.example.com",
			"header:Custom-Token": "custom-secret",
		},
	}
	req, err := http.NewRequest(http.MethodPost, "https://custom-claude.example.com/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	// Preset preexisting client headers to ensure they get stripped for empty API key
	req.Header.Set("Authorization", "Bearer preexisting-bearer")
	req.Header.Set("x-api-key", "preexisting-key")

	if errHeaders := applyClaudeHeaders(req, auth, "", false, nil, nil, nil); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty for empty API key", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q, want empty for empty API key", got)
	}
	if got := req.Header.Get("Custom-Token"); got != "custom-secret" {
		t.Fatalf("Custom-Token = %q, want custom-secret", got)
	}

	// Also verify PrepareRequest
	req2, _ := http.NewRequest(http.MethodPost, "https://custom-claude.example.com/v1/messages", nil)
	req2.Header.Set("Authorization", "Bearer preexisting-bearer")
	req2.Header.Set("x-api-key", "preexisting-key")
	exec := &ClaudeExecutor{}
	if errPrep := exec.PrepareRequest(req2, auth); errPrep != nil {
		t.Fatalf("PrepareRequest() error = %v", errPrep)
	}
	if got := req2.Header.Get("Authorization"); got != "" {
		t.Fatalf("PrepareRequest Authorization = %q, want empty", got)
	}
	if got := req2.Header.Get("x-api-key"); got != "" {
		t.Fatalf("PrepareRequest x-api-key = %q, want empty", got)
	}
	if got := req2.Header.Get("Custom-Token"); got != "custom-secret" {
		t.Fatalf("PrepareRequest Custom-Token = %q, want custom-secret", got)
	}
}

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestApplyClaudeToolPrefix_WithToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"}],"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"beta"},{"type":"tool_reference","tool_name":"proxy_gamma"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.tool_name").String(); got != "proxy_beta" {
		t.Fatalf("messages.0.content.0.tool_name = %q, want %q", got, "proxy_beta")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != "proxy_gamma" {
		t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, "proxy_gamma")
	}
}

func TestSanitizeClaudeWebSearchDomains(t *testing.T) {
	// Mirrors the litellm payload from issue #2681: a non-empty allowed_domains
	// alongside an empty blocked_domains, which Anthropic rejects as ambiguous.
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["anthropic.com"],"blocked_domains":[],"max_uses":8}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if gjson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("empty blocked_domains should be removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.allowed_domains").Array(); len(got) != 1 || got[0].String() != "anthropic.com" {
		t.Fatalf("non-empty allowed_domains should be preserved: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.max_uses").Int(); got != 8 {
		t.Fatalf("max_uses should be preserved: got %d", got)
	}
}

func TestSanitizeClaudeWebSearchDomains_LeavesNonBuiltinAndNonEmpty(t *testing.T) {
	// Empty arrays on non-web_search tools must be left untouched.
	input := []byte(`{"tools":[{"type":"custom","name":"x","blocked_domains":[]},{"type":"web_search_20250305","name":"web_search","blocked_domains":["evil.com"]}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if !gjson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("non-web_search tool fields should be untouched: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.1.blocked_domains").Array(); len(got) != 1 || got[0].String() != "evil.com" {
		t.Fatalf("non-empty blocked_domains should be preserved: %s", string(out))
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinTools(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"my_custom_tool","input_schema":{"type":"object"}}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name should not be prefixed: tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_my_custom_tool" {
		t.Fatalf("custom tool should be prefixed: tools.1.name = %q, want %q", got, "proxy_my_custom_tool")
	}
}

func TestApplyClaudeToolPrefix_BuiltinToolSkipped(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}},
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_KnownBuiltinInHistoryOnly(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_CustomToolsPrefixed(t *testing.T) {
	body := []byte(`{
		"tools": [{"name": "Read"}, {"name": "Write"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}},
				{"type": "tool_use", "name": "Write", "id": "w1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Write" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Write")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Write" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Write")
	}
}

func TestApplyClaudeToolPrefix_ToolChoiceBuiltin(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search"},
			{"name": "Read"}
		],
		"tool_choice": {"type": "tool", "name": "web_search"}
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "web_search")
	}
}

func TestApplyClaudeToolPrefix_KnownFallbackBuiltinsRemainUnprefixed(t *testing.T) {
	for _, builtin := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		t.Run(builtin, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{
				"tools":[{"name":"Read"}],
				"tool_choice":{"type":"tool","name":%q},
				"messages":[{"role":"assistant","content":[{"type":"tool_use","name":%q,"id":"toolu_1","input":{}},{"type":"tool_reference","tool_name":%q},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":%q}]}]}]
			}`, builtin, builtin, builtin, builtin))
			out := applyClaudeToolPrefix(input, "proxy_")

			if got := gjson.GetBytes(out, "tool_choice.name").String(); got != builtin {
				t.Fatalf("tool_choice.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != builtin {
				t.Fatalf("messages.0.content.0.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.2.content.0.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.2.content.0.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
				t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
			}
		})
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromResponse_WithToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_reference","tool_name":"proxy_alpha"},{"type":"tool_reference","tool_name":"bravo"}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.tool_name").String(); got != "alpha" {
		t.Fatalf("content.0.tool_name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.tool_name").String(); got != "bravo" {
		t.Fatalf("content.1.tool_name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestStripClaudeToolPrefixFromStreamLine_WithToolReference(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_reference","tool_name":"proxy_beta"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.tool_name").String(); got != "beta" {
		t.Fatalf("content_block.tool_name = %q, want %q", got, "beta")
	}
}

func TestApplyClaudeToolPrefix_PreservesNestedMCPToolReference(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"mcp__nia__manage_resource"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want MCP name preserved", got)
	}
}

func TestClaudeExecutor_ExecuteStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "gAAAAABopenai-encrypted-content") || strings.Contains(string(seenBody), "codex reasoning") {
		t.Fatalf("invalid thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer", got)
	}
}

func TestClaudeExecutor_ExecuteStripsForeignToolUseSignaturesBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{
					"type":"tool_use",
					"id":"toolu_1",
					"name":"lookup",
					"input":{"q":"x"},
					"signature":"skip_thought_signature_validator",
					"thought_signature":"skip_thought_signature_validator",
					"extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}}
				}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	toolUse := gjson.GetBytes(seenBody, "messages.0.content.0")
	if !toolUse.Get("type").Exists() || toolUse.Get("type").String() != "tool_use" {
		t.Fatalf("tool_use block was not preserved: %s", string(seenBody))
	}
	for _, path := range []string{"signature", "thought_signature", "extra_content"} {
		if toolUse.Get(path).Exists() {
			t.Fatalf("foreign tool_use signature field %s was forwarded: %s", path, string(seenBody))
		}
	}
}

func TestShouldSanitizeClaudeMessagesForUpstream_OnlyClaudeFamily(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{model: "claude-sonnet-4-5", want: true},
		{model: "claude-3-5-sonnet-20241022", want: true},
		{model: "kimi-k2.5", want: false},
		{model: "mimo-v2", want: false},
		{model: "gemini-3.5-flash", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := shouldSanitizeClaudeMessagesForUpstream(tc.model)
			if got != tc.want {
				t.Errorf("shouldSanitizeClaudeMessagesForUpstream(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestSanitizeClaudeMessagesForClaudeUpstream_BypassesUnknownModelSignatureMatrix(t *testing.T) {
	rawSignature := "skip_thought_signature_validator"
	body := []byte(`{
		"model": "kimi-k2.5",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "keep", "signature": "` + rawSignature + `"},
					{"type": "text", "text": "hello"},
					{"type": "tool_use", "id": "call_123", "name": "get_weather", "input": {}, "signature": "` + rawSignature + `"}
				]
			}
		]
	}`)

	output := sanitizeClaudeMessagesForClaudeUpstreamWithDebug(context.Background(), body, "kimi-k2.5")
	parts := gjson.GetBytes(output, "messages.0.content").Array()
	if len(parts) != 3 {
		t.Fatalf("content length = %d, want 3 when sanitizer is bypassed: %s", len(parts), output)
	}
	if got := parts[0].Get("signature").String(); got != rawSignature {
		t.Fatalf("thinking signature = %q, want preserved %q", got, rawSignature)
	}
	if got := parts[2].Get("signature").String(); got != rawSignature {
		t.Fatalf("tool_use signature = %q, want preserved %q", got, rawSignature)
	}
}

func TestClaudeExecutor_ExecuteBypassesSignatureSanitizerForUnknownModel(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"mimo-v2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"keep reasoning","signature":""},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "mimo-v2",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if !strings.Contains(string(seenBody), "keep reasoning") {
		t.Fatalf("unknown-model thinking block should bypass Claude sanitizer: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStripsMalformedEPrefixThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	malformedSignature := malformedClaudeTreeSignatureForClaudeExecutorTest()
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"bad reasoning","signature":"` + malformedSignature + `"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), malformedSignature) || strings.Contains(string(seenBody), "bad reasoning") {
		t.Fatalf("malformed E-prefix thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer", got)
	}
}

func TestClaudeExecutor_ExecuteStripsInvalidBase64ThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"bad reasoning","signature":"E!!!invalid!!!"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "E!!!invalid!!!") || strings.Contains(string(seenBody), "bad reasoning") {
		t.Fatalf("invalid-base64 thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStripsEmptySignatureEmptyTextThinking(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","text":"","signature":""},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("type").String(); got != "text" {
		t.Fatalf("remaining content type = %q, want text: %s", got, string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer: %s", got, string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStreamStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "gAAAAABopenai-encrypted-content") || strings.Contains(string(seenBody), "codex reasoning") {
		t.Fatalf("invalid thinking block was forwarded: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStreamDirectPassthroughEmitsCompleteSSEEvents(t *testing.T) {
	firstData := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`
	secondData := `{"type":"message_stop"}`
	upstreamStream := "event: content_block_delta\n" +
		"data: " + firstData + "\n" +
		"\n" +
		"event: message_stop\n" +
		"data: " + secondData + "\n" +
		"\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamStream))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var payloads []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		payloads = append(payloads, string(chunk.Payload))
	}

	want := []string{
		"event: content_block_delta\n" + "data: " + firstData + "\n\n",
		"event: message_stop\n" + "data: " + secondData + "\n\n",
	}
	if len(payloads) != len(want) {
		t.Fatalf("payload count = %d, want %d: %#v", len(payloads), len(want), payloads)
	}
	for i := range want {
		if payloads[i] != want[i] {
			t.Fatalf("payload[%d] = %q, want %q", i, payloads[i], want[i])
		}
	}
}

// TestClaudeExecutor_ExecuteStreamDecodesCompressedSSE guards the dependency that
// lets CPA advertise the real client's Accept-Encoding on streaming requests:
// once compression is offered the upstream may compress the SSE body, so the
// streaming success path must decode it and still emit event boundaries intact.
func TestClaudeExecutor_ExecuteStreamDecodesCompressedSSE(t *testing.T) {
	firstData := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`
	secondData := `{"type":"message_stop"}`
	upstreamStream := "event: content_block_delta\n" +
		"data: " + firstData + "\n" +
		"\n" +
		"event: message_stop\n" +
		"data: " + secondData + "\n" +
		"\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gzipWriter := gzip.NewWriter(w)
		if _, errWrite := gzipWriter.Write([]byte(upstreamStream)); errWrite != nil {
			t.Errorf("gzip write: %v", errWrite)
		}
		if errClose := gzipWriter.Close(); errClose != nil {
			t.Errorf("gzip close: %v", errClose)
		}
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var payloads []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		payloads = append(payloads, string(chunk.Payload))
	}

	want := []string{
		"event: content_block_delta\n" + "data: " + firstData + "\n\n",
		"event: message_stop\n" + "data: " + secondData + "\n\n",
	}
	if len(payloads) != len(want) {
		t.Fatalf("payload count = %d, want %d: %#v", len(payloads), len(want), payloads)
	}
	for i := range want {
		if payloads[i] != want[i] {
			t.Fatalf("payload[%d] = %q, want %q", i, payloads[i], want[i])
		}
	}
}

func TestClaudeExecutor_CountTokensExcludesInvalidOpenAIThinking(t *testing.T) {
	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	countTokens := func(payload []byte) int64 {
		t.Helper()
		resp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		if err != nil {
			t.Fatalf("CountTokens() error = %v", err)
		}
		return gjson.GetBytes(resp.Payload, "input_tokens").Int()
	}

	withInvalidThinking := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)
	withoutInvalidThinking := []byte(`{
		"messages": [
			{"role":"assistant","content":[{"type":"text","text":"Answer"}]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	if got, want := countTokens(withInvalidThinking), countTokens(withoutInvalidThinking); got != want {
		t.Fatalf("count with invalid thinking = %d, want sanitized count %d", got, want)
	}
}

func TestShouldUseClaudeUpstreamTokenCount(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		baseURL string
		want    bool
	}{
		{name: "official OAuth", apiKey: "sk-ant-oat-official", baseURL: "https://api.anthropic.com", want: true},
		{name: "official API key", apiKey: "key-official", baseURL: "https://api.anthropic.com:443", want: true},
		{name: "custom OAuth", apiKey: "sk-ant-oat-custom", baseURL: "https://gateway.example"},
		{name: "custom API key", apiKey: "key-custom", baseURL: "https://gateway.example"},
		{name: "lookalike host", apiKey: "sk-ant-oat-lookalike", baseURL: "https://api.anthropic.com.example"},
		{name: "insecure official host", apiKey: "sk-ant-oat-http", baseURL: "http://api.anthropic.com"},
		{name: "missing credential", baseURL: "https://api.anthropic.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldUseClaudeUpstreamTokenCount(test.apiKey, test.baseURL); got != test.want {
				t.Fatalf("shouldUseClaudeUpstreamTokenCount() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestClaudeExecutor_CountTokensCountsLocallyWithoutUpstreamRequest(t *testing.T) {
	payload := []byte(`{
		"system":"client system instructions",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)
	const expectedCount int64 = 7

	testCases := []struct {
		name   string
		apiKey string
	}{
		{name: "custom API key", apiKey: "key-123"},
		{name: "custom OAuth", apiKey: "sk-ant-oat-custom"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected upstream count_tokens request: %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			executor := newAnthropicCompatibleTestExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":  testCase.apiKey,
				"base_url": server.URL,
			}}
			resp, errCount := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
			if errCount != nil {
				t.Fatalf("CountTokens() error = %v", errCount)
			}
			if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != expectedCount {
				t.Fatalf("input_tokens = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
			}
		})
	}

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	resp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatGemini,
	})
	if err != nil {
		t.Fatalf("CountTokens() Gemini response error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "totalTokens").Int(); got != expectedCount {
		t.Fatalf("Gemini totalTokens = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "promptTokensDetails.0.tokenCount").Int(); got != expectedCount {
		t.Fatalf("Gemini prompt token detail = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
	}
}

func TestClaudeExecutor_CountTokensRejectsInvalidRequests(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
	}{
		{name: "invalid JSON", payload: `not-json`},
		{name: "non-object", payload: `[]`},
		{name: "missing messages", payload: `{}`},
		{name: "empty messages", payload: `{"messages":[]}`},
		{name: "non-array messages", payload: `{"messages":"invalid"}`},
		{name: "invalid role", payload: `{"messages":[{"role":"system","content":"hello"}]}`},
		{name: "invalid content", payload: `{"messages":[{"role":"user","content":42}]}`},
		{name: "non-object content block", payload: `{"messages":[{"role":"user","content":[42]}]}`},
		{name: "untyped content block", payload: `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`},
	}

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: []byte(testCase.payload),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			assertStatusErr(t, err, http.StatusBadRequest)
			requestErr, ok := err.(cliproxyexecutor.RequestScopedError)
			if !ok || !requestErr.IsRequestScoped() {
				t.Fatalf("error %T is not request-scoped", err)
			}
		})
	}
}

func TestClaudeExecutor_DefaultDoesNotInjectUserID(t *testing.T) {
	var userIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userIDs = append(userIDs, gjson.GetBytes(body, "metadata.user_id").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for i := 0; i < 2; i++ {
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute call %d error: %v", i, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] != "" || userIDs[1] != "" {
		t.Fatalf("default API-key requests must preserve caller metadata without injecting user_id, got %q and %q", userIDs[0], userIDs[1])
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsEmptyClaudeStream(t *testing.T) {
	_, err := executeOpenAIChatCompletionThroughClaude(t, "")
	if err == nil {
		t.Fatal("Execute error = nil, want empty stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "empty stream response") {
		t.Fatalf("Execute error = %q, want empty stream response", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsClaudeErrorEvent(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want upstream error event")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("Execute error = %q, want upstream overloaded", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsIncompleteClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "ended before message completion") {
		t.Fatalf("Execute error = %q, want incomplete stream error", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamConvertsValidClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "msg_123" {
		t.Fatalf("response id = %q, want msg_123; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "model").String(); got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("response model = %q, want claude-3-5-sonnet-20241022", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3", got)
	}
}

func TestClaudeExecutor_ExecuteTransportMatchesResponseFormat(t *testing.T) {
	const model = "claude-3-5-sonnet-20241022"
	streamResponse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	jsonResponse := `{"id":"msg_123","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}`

	tests := []struct {
		name           string
		sourceFormat   sdktranslator.Format
		responseFormat sdktranslator.Format
		wantStream     bool
	}{
		{name: "OpenAI to OpenAI uses SSE", sourceFormat: sdktranslator.FormatOpenAI, responseFormat: sdktranslator.FormatOpenAI, wantStream: true},
		{name: "OpenAI to Claude uses JSON", sourceFormat: sdktranslator.FormatOpenAI, responseFormat: sdktranslator.FormatClaude, wantStream: false},
		{name: "Claude to OpenAI uses SSE", sourceFormat: sdktranslator.FormatClaude, responseFormat: sdktranslator.FormatOpenAI, wantStream: true},
		{name: "Claude to Claude uses JSON", sourceFormat: sdktranslator.FormatClaude, responseFormat: sdktranslator.FormatClaude, wantStream: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenBody []byte
			var seenHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenBody, _ = io.ReadAll(r.Body)
				seenHeaders = r.Header.Clone()
				if tt.wantStream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(streamResponse))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(jsonResponse))
			}))
			defer server.Close()

			executor := newAnthropicCompatibleTestExecutor(&config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}},
						Params: map[string]any{"stream": !tt.wantStream},
					}},
				},
			})
			attributes := map[string]string{
				"api_key":  "key-123",
				"base_url": server.URL,
			}
			auth := &cliproxyauth.Auth{Attributes: attributes}
			payload := []byte(`{"model":"claude-3-5-sonnet-20241022","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   model,
				Payload: payload,
			}, cliproxyexecutor.Options{
				SourceFormat:   tt.sourceFormat,
				ResponseFormat: tt.responseFormat,
				Headers: http.Header{
					"Anthropic-Beta": []string{"client-beta"},
				},
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			stream := gjson.GetBytes(seenBody, "stream")
			if !stream.Exists() || stream.Bool() != tt.wantStream {
				t.Fatalf("upstream stream = %s, want %t; body=%s", stream.Raw, tt.wantStream, string(seenBody))
			}
			wantAccept := "application/json"
			wantEncoding := "gzip, deflate, br, zstd"
			if tt.wantStream {
				wantAccept = "text/event-stream"
				wantEncoding = "identity"
			}
			if got := seenHeaders.Get("Accept"); got != wantAccept {
				t.Fatalf("Accept = %q, want %q", got, wantAccept)
			}
			if got := seenHeaders.Get("Accept-Encoding"); got != wantEncoding {
				t.Fatalf("Accept-Encoding = %q, want %q", got, wantEncoding)
			}
			if got := seenHeaders.Get("Anthropic-Beta"); !strings.Contains(got, "client-beta") {
				t.Fatalf("Anthropic-Beta = %q, want client beta preserved", got)
			}
		})
	}
}

func executeOpenAIChatCompletionThroughClaude(t *testing.T, upstreamBody string) (cliproxyexecutor.Response, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)

	return executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
}

func assertStatusErr(t *testing.T, err error, want int) {
	t.Helper()

	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", err)
	}
	if got := status.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func TestStripClaudeToolPrefixFromResponse_NestedToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"proxy_mcp__nia__manage_resource"}]}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")
	got := gjson.GetBytes(out, "content.0.content.0.tool_name").String()
	if got != "mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "mcp__nia__manage_resource")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReferenceWithStringContent(t *testing.T) {
	// tool_result.content can be a string - should not be processed
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"plain string result"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content").String()
	if got != "plain string result" {
		t.Fatalf("string content should remain unchanged = %q", got)
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"web_search"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "web_search" {
		t.Fatalf("built-in tool_reference should not be prefixed, got %q", got)
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange(t *testing.T) {
	// Payload where no TTL normalization is needed (all blocks use 1h with no
	// preceding 5m block). The text intentionally contains HTML chars (<, >, &)
	// that json.Marshal would escape to \u003c etc., altering byte identity.
	payload := []byte(`{"tools":[{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"<system-reminder>foo & bar</system-reminder>","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeCacheControlTTL(payload)

	if !bytes.Equal(out, payload) {
		t.Fatalf("normalizeCacheControlTTL altered bytes when no change was needed.\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !gjson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !gjson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestClaudeExecutor_ExecuteSanitizesSignaturesBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 16,
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"drop this","signature":""},
				{"type":"text","text":"I will run git status."},
				{"type":"tool_use","id":"Bash-1","name":"Bash","input":{"command":"git status"},"signature":"bad","thoughtSignature":"bad2","model":"claude-opus-4-1"}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"Bash-1","content":"ok"}]}
		]
	}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       false,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	parts := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("messages.0.content length = %d, want 2; body=%s", len(parts), seenBody)
	}
	if parts[0].Get("type").String() != "text" {
		t.Fatalf("first remaining part = %s, want text", parts[0].Raw)
	}
	toolUse := parts[1]
	if toolUse.Get("type").String() != "tool_use" {
		t.Fatalf("second remaining part = %s, want tool_use", toolUse.Raw)
	}
	for _, path := range []string{"signature", "thoughtSignature", "model"} {
		if toolUse.Get(path).Exists() {
			t.Fatalf("tool_use.%s should be removed before upstream: %s", path, seenBody)
		}
	}
}

func TestClaudeExecutor_Execute_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_ExecuteStream_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func testClaudeExecutorInvalidCompressedErrorBody(
	t *testing.T,
	invoke func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error,
) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not-a-valid-gzip-stream"))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	err := invoke(executor, auth, payload)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to decode error response body") {
		t.Fatalf("expected decode failure message, got: %v", err)
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); !ok || statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected status code 400, got: %v", err)
	}
}

func TestEnsureModelMaxTokens_UsesRegisteredMaxCompletionTokens(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-max-completion-tokens-client"
	modelID := "test-claude-max-completion-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-max-completion-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d", got, 4096)
	}
}

func TestEnsureModelMaxTokens_DefaultsMissingValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:          modelID,
		Type:        "claude",
		OwnedBy:     "anthropic",
		Object:      "model",
		Created:     time.Now().Unix(),
		UserDefined: true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-default-max-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != defaultModelMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, defaultModelMaxTokens)
	}
}

func TestEnsureModelMaxTokens_PreservesExplicitValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-preserve-max-tokens-model","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
}

func TestEnsureModelMaxTokens_SkipsUnregisteredModel(t *testing.T) {
	input := []byte(`{"model":"test-claude-unregistered-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, "test-claude-unregistered-model")

	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset, got %s", gjson.GetBytes(out, "max_tokens").Raw)
	}
}

// TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding verifies that streaming
// requests use Accept-Encoding: identity so the upstream cannot respond with a
// compressed SSE body that would silently break the line scanner.
func TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "identity")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/event-stream")
	}
}

// TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding verifies that non-streaming
// requests keep the full accept-encoding to allow response compression (which
// decodeResponseBody handles correctly).
func TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotEncoding != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "gzip, deflate, br, zstd")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
}

// TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded verifies that a streaming
// HTTP 200 response with Content-Encoding: gzip is correctly decompressed before
// the line scanner runs, so SSE chunks are not silently dropped.
func TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected at least one chunk from gzip-encoded SSE body, got none (body was not decompressed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("expected SSE content in chunks, got: %q", combined.String())
	}
}

func TestDecodeResponseBodyStackedRepeatedHeaders(t *testing.T) {
	payload := []byte("stacked Claude response")
	var gzipOutput bytes.Buffer
	gzipWriter := gzip.NewWriter(&gzipOutput)
	if _, errWrite := gzipWriter.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := gzipWriter.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	var brotliOutput bytes.Buffer
	brotliWriter := brotli.NewWriter(&brotliOutput)
	if _, errWrite := brotliWriter.Write(gzipOutput.Bytes()); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := brotliWriter.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	header := make(http.Header)
	header.Add("Content-Encoding", "gzip")
	header.Add("Content-Encoding", "br")
	decoded, errDecode := decodeResponseBody(io.NopCloser(bytes.NewReader(brotliOutput.Bytes())), claudeResponseContentEncoding(header))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	defer decoded.Close()
	got, errRead := io.ReadAll(decoded)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded body = %q, want %q", got, payload)
	}
}

// TestDecodeResponseBody_MagicByteGzipNoHeader verifies that decodeResponseBody
// detects gzip-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteGzipNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(plaintext))
	_ = gz.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_MagicByteZstdNoHeader verifies that decodeResponseBody
// detects zstd-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteZstdNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	_, _ = enc.Write([]byte(plaintext))
	_ = enc.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_PlainTextNoHeader verifies that decodeResponseBody returns
// plain text untouched when Content-Encoding is absent and no magic bytes match.
func TestDecodeResponseBody_PlainTextNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"
	rc := io.NopCloser(strings.NewReader(plaintext))
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader verifies the full
// pipeline: when the upstream returns a gzip-compressed SSE body WITHOUT setting
// Content-Encoding (a misbehaving upstream), the magic-byte sniff in
// decodeResponseBody still decompresses it, so chunks reach the caller.
func TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected chunks from gzip body without Content-Encoding header, got none (magic-byte sniff failed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("unexpected chunk content: %q", combined.String())
	}
}

// TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader verifies that the
// error path (4xx) correctly decompresses a gzip body even when the upstream omits
// the Content-Encoding header.  This closes the gap left by PR #1771, which only
// fixed header-declared compression on the error path.
func TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

// TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader verifies
// the same for the streaming executor: 4xx gzip body without Content-Encoding is
// decoded and the error message is readable.
func TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"stream test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "stream test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

func TestAnthropicCompatibleStreamHonorsAcceptEncodingOverride(t *testing.T) {
	var gotEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                "key-123",
		"base_url":               server.URL,
		"header:Accept-Encoding": "gzip, deflate, br, zstd",
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want caller-owned override", gotEncoding)
	}
}

// assertClaudeMidConversationSystemMessage checks a forwarded caller system prompt.
// wantTTL is "" for the native default marker and "1h" once
// upgradeClaudeCacheControlTTL has run, which only happens for OAuth credentials.
func assertClaudeMidConversationSystemMessage(t *testing.T, body []byte, messageIndex int, wantText, wantTTL string) {
	t.Helper()
	messagePath := fmt.Sprintf("messages.%d", messageIndex)
	if got := gjson.GetBytes(body, messagePath+".role").String(); got != "system" {
		t.Fatalf("%s.role = %q, want system", messagePath, got)
	}
	content := gjson.GetBytes(body, messagePath+".content").Array()
	if len(content) != 1 {
		t.Fatalf("%s.content has %d blocks, want 1", messagePath, len(content))
	}
	if got := content[0].Get("text").String(); got != wantText {
		t.Fatalf("%s.content.0.text lost caller prompt: got len %d, want len %d", messagePath, len(got), len(wantText))
	}
	if got := content[0].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("%s.content.0.cache_control.type = %q, want ephemeral", messagePath, got)
	}
	if got := content[0].Get("cache_control.ttl").String(); got != wantTTL {
		t.Fatalf("%s.content.0.cache_control.ttl = %q, want %q: %s", messagePath, got, wantTTL, content[0].Raw)
	}
}

func TestClaudeBillingFingerprintUsesLatestUserText(t *testing.T) {
	const prompt = "CPA_OFFICIAL_BASEURL_CLI_SYSTEM_EMPTY_b82d4e"
	payload := []byte(`{"system":"must not seed the build hash","messages":[{"role":"user","content":"old"},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"text","text":"<system-reminder>date</system-reminder>"},{"type":"text","text":"` + prompt + `"}]}]}`)
	if got := claudeBillingFingerprintMessageText(payload); got != prompt {
		t.Fatalf("claudeBillingFingerprintMessageText() = %q, want %q", got, prompt)
	}
	if got := computeFingerprint(prompt, "2.1.247"); got != "0e5" {
		t.Fatalf("computeFingerprint() = %q, want Desktop profile suffix 0e5", got)
	}
}

func TestClaudeUsesLegacySystemReminder(t *testing.T) {
	tests := map[string]bool{
		"claude-opus-4-6":          true,
		"claude-opus-4-7":          true,
		"claude-sonnet-5":          false,
		"prefix/claude-sonnet-4-6": true,
		"claude-3-5-haiku-latest":  true,
		"claude-opus-5":            false,
		"prefix/claude-opus-4-8":   false,
		"claude-fable-5":           false,
		"claude-future-6":          false,
		"":                         false,
	}
	for model, want := range tests {
		t.Run(model, func(t *testing.T) {
			payload := []byte(`{"model":` + fmt.Sprintf("%q", model) + `}`)
			if got := claudeUsesLegacySystemReminder(payload); got != want {
				t.Fatalf("claudeUsesLegacySystemReminder(%q) = %v, want %v", model, got, want)
			}
		})
	}
}

// Test case 5: Special characters survive the mid-conversation system move.
func TestClaudeExecutor_CustomBaseURLPreservesBodyByDefault(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}

	if strings.Contains(string(seenBody), "x-anthropic-billing-header:") || strings.Contains(string(seenBody), "cch=") {
		t.Fatalf("default custom BaseURL request must not inject billing/CCH: %s", seenBody)
	}
}

func TestClaudeExecutor_CustomBaseURLAPIKeyDoesNotEnableCCHSigning(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:  "key-123",
			BaseURL: server.URL,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	const messageText = "please keep literal cch=00000 in this message"
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"please keep literal cch=00000 in this message"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if got := gjson.GetBytes(seenBody, "messages.0.content.0.text").String(); got != messageText {
		t.Fatalf("message text = %q, want %q", got, messageText)
	}
	if strings.Contains(string(seenBody), "x-anthropic-billing-header:") {
		t.Fatalf("default custom BaseURL request must not inject a billing header: %s", seenBody)
	}
}

func TestAnthropicCompatibleCustomBaseOAuthLikeTokenDoesNotGenerateCCH(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-custom-cch",
			"base_url": server.URL,
		},
		Metadata: claudeOAuthTestMetadata(),
	}
	payload := []byte(`{"model":"claude-opus-4-6","system":"keep original system","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, ok := claudeBillingCCHDigitsOffset(seenBody); ok {
		t.Fatalf("anthropic-compatible generated a Desktop CCH: %s", seenBody)
	}
	if got := gjson.GetBytes(seenBody, "system.0.text").String(); got != "keep original system" {
		t.Fatalf("system text = %q, want caller-owned system text", got)
	}
}

func TestNormalizeClaudeSamplingForUpstream_RemovesTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

func TestNormalizeClaudeSamplingForUpstream_RemovesTemperatureWithThinkingEnabled(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"thinking":{"type":"enabled","budget_tokens":2048}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

func TestNormalizeClaudeSamplingForUpstream_RemovesTopPAndTopKForThinking(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"top_p":0.9,"top_k":40,"thinking":{"type":"adaptive"}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if gjson.GetBytes(out, "top_p").Exists() {
		t.Fatalf("top_p should be removed when thinking is active")
	}
	if gjson.GetBytes(out, "top_k").Exists() {
		t.Fatalf("top_k should be removed when thinking is active")
	}
}

func TestNormalizeClaudeSamplingForUpstream_NoThinkingRemovesTemperatureAndTopP(t *testing.T) {
	payload := []byte(`{"temperature":0,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if gjson.GetBytes(out, "top_p").Exists() {
		t.Fatalf("top_p should be removed")
	}
	if got := gjson.GetBytes(out, "top_k").Int(); got != 40 {
		t.Fatalf("top_k = %v, want 40", got)
	}
}

func TestNormalizeClaudeSamplingForUpstream_AfterForcedToolChoiceRemovesTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tool_choice":{"type":"any"}}`)
	out := disableThinkingIfToolChoiceForced(payload)
	out = normalizeClaudeSamplingForUpstream(out, false)

	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed when tool_choice forces tool use")
	}
	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

// The measured structured Haiku helper sends "temperature":1, and
// claudeCodeHelperShapeStructured keys on exactly that value. Stripping it would
// make CPA emit a shape no native client produces, so a confirmed native caller
// must keep it.
func TestNormalizeClaudeSamplingForUpstreamNativeKeepsMeasuredHelperTemperature(t *testing.T) {
	// Top-level key order and values mirror the measured structured helper.
	payload := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":[{"type":"text","text":"helper probe"}]}],"system":[{"type":"text","text":"Return a short title."}],"tools":[],"metadata":{"user_id":"u"},"max_tokens":32000,"thinking":{"type":"disabled"},"temperature":1,"output_config":{"format":{"type":"json_schema"}},"stream":true}`)
	if got := gjson.GetBytes(payload, "temperature"); !got.Exists() || got.Num != 1 {
		t.Fatalf("measured helper fixture should carry temperature=1, got %q", got.Raw)
	}

	out := normalizeClaudeSamplingForUpstream(payload, true)

	if got := gjson.GetBytes(out, "temperature"); !got.Exists() || got.Num != 1 {
		t.Fatalf("confirmed native must preserve the measured temperature, got %q", got.Raw)
	}
}

// Anthropic's real constraints, verified against the live API: with thinking
// active temperature must be 1, top_p must be >= 0.95 and top_k must be unset;
// otherwise temperature and top_p cannot both be specified. Preserving the
// native wire must never forward a combination that would 400.
func TestNormalizeClaudeSamplingForUpstreamNativeDropsOnlyRejectedCombinations(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		keep    map[string]float64
		dropped []string
	}{
		{
			name:    "thinking off keeps every accepted knob",
			payload: `{"temperature":0.5,"top_k":40}`,
			keep:    map[string]float64{"temperature": 0.5, "top_k": 40},
		},
		{
			name:    "thinking off drops top_p when temperature is also set",
			payload: `{"temperature":0.5,"top_p":0.9}`,
			keep:    map[string]float64{"temperature": 0.5},
			dropped: []string{"top_p"},
		},
		{
			name:    "thinking off keeps a lone top_p",
			payload: `{"top_p":0.9}`,
			keep:    map[string]float64{"top_p": 0.9},
		},
		{
			name:    "thinking disabled is not thinking",
			payload: `{"temperature":1,"thinking":{"type":"disabled"}}`,
			keep:    map[string]float64{"temperature": 1},
		},
		{
			name:    "thinking enabled keeps temperature 1",
			payload: `{"temperature":1,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			keep:    map[string]float64{"temperature": 1},
		},
		{
			name:    "thinking enabled drops temperature that is not 1",
			payload: `{"temperature":0.5,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			dropped: []string{"temperature"},
		},
		{
			name:    "thinking enabled keeps top_p at or above 0.95",
			payload: `{"top_p":0.99,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			keep:    map[string]float64{"top_p": 0.99},
		},
		{
			name:    "thinking enabled drops top_p below 0.95",
			payload: `{"top_p":0.9,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			dropped: []string{"top_p"},
		},
		{
			name:    "thinking enabled always drops top_k",
			payload: `{"top_k":40,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			dropped: []string{"top_k"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizeClaudeSamplingForUpstream([]byte(tc.payload), true)

			for field, want := range tc.keep {
				got := gjson.GetBytes(out, field)
				if !got.Exists() || got.Num != want {
					t.Fatalf("%s = %q, want %v preserved", field, got.Raw, want)
				}
			}
			for _, field := range tc.dropped {
				if got := gjson.GetBytes(out, field); got.Exists() {
					t.Fatalf("%s = %q, want dropped because Anthropic rejects it", field, got.Raw)
				}
			}
		})
	}
}

func TestRemapOAuthToolNames_AllClientNamesUseMCPAliases(t *testing.T) {
	for _, original := range []string{"Bash", "bash", "Glob", "glob"} {
		t.Run(original, func(t *testing.T) {
			body := []byte(`{"tools":[{"name":` + fmt.Sprintf("%q", original) + `,"description":"Run a client tool","input_schema":{"type":"object"}}]}`)
			out, reverseMap := remapOAuthToolNames(body)
			alias := gjson.GetBytes(out, "tools.0.name").String()
			if !helps.IsClaudeMCPToolName(alias) {
				t.Fatalf("tools.0.name = %q, want MCP alias", alias)
			}
			if reverseMap[alias] != original {
				t.Fatalf("reverseMap = %v, want %q -> %q", reverseMap, alias, original)
			}
			resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":` + fmt.Sprintf("%q", alias) + `,"input":{}}]}`)
			reversed, errReverse := reverseRemapOAuthToolNames(resp, reverseMap)
			if errReverse != nil {
				t.Fatalf("reverseRemapOAuthToolNames() error = %v", errReverse)
			}
			if got := gjson.GetBytes(reversed, "content.0.name").String(); got != original {
				t.Fatalf("content.0.name = %q, want %q", got, original)
			}
		})
	}
}

func TestRemapOAuthToolNames_AllClientToolsAsMCP(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":2},
			{"name":"bash","description":"client shell tool","input_schema":{"type":"object"}},
			{"name":"Read","description":"client read tool","input_schema":{"type":"object"}},
			{"name":"mcp__context7__query-docs","description":"existing MCP tool","input_schema":{"type":"object"}},
			{"name":"search_web","description":"unknown one","input_schema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}},
			{"name":"Search_Web","description":"case-distinct unknown","input_schema":{"type":"object"}},
			{"name":"search_web","description":"repeated declaration","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"search_web"},
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_unknown","name":"search_web","input":{"q":"go"}},
				{"type":"tool_reference","tool_name":"Search_Web"}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_unknown","content":[{"type":"tool_reference","tool_name":"search_web"}]}
			]}
		]
	}`)

	out, reverseMap := remapOAuthToolNamesWithOptions(body, claudeMCPAliasOptions{secret: "credential-secret"})

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("typed builtin = %q, want unchanged", got)
	}
	bashAlias := gjson.GetBytes(out, "tools.1.name").String()
	readAlias := gjson.GetBytes(out, "tools.2.name").String()
	if !helps.IsClaudeMCPToolName(bashAlias) || !helps.IsClaudeMCPToolName(readAlias) {
		t.Fatalf("former vetted names did not receive MCP aliases: bash=%q Read=%q", bashAlias, readAlias)
	}
	if got := gjson.GetBytes(out, "tools.1.description").String(); got != "client shell tool" {
		t.Fatalf("bash description = %q, want preserved", got)
	}
	if got := gjson.GetBytes(out, "tools.1.input_schema.type").String(); got != "object" {
		t.Fatalf("bash schema changed: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.3.name").String(); got != "mcp__context7__query-docs" {
		t.Fatalf("existing MCP tool = %q, want unchanged", got)
	}

	searchAlias := gjson.GetBytes(out, "tools.4.name").String()
	caseAlias := gjson.GetBytes(out, "tools.5.name").String()
	if !helps.IsClaudeMCPToolName(searchAlias) || !helps.IsClaudeMCPToolName(caseAlias) {
		t.Fatalf("generated aliases are invalid: %q, %q", searchAlias, caseAlias)
	}
	if searchAlias == caseAlias {
		t.Fatalf("case-distinct names share alias %q", searchAlias)
	}
	if got := gjson.GetBytes(out, "tools.6.name").String(); got != searchAlias {
		t.Fatalf("repeated declaration alias = %q, want %q", got, searchAlias)
	}
	if !strings.HasSuffix(searchAlias, "_search_web") || !strings.HasSuffix(caseAlias, "_Search_Web") {
		t.Fatalf("generated aliases lost semantic suffixes: %q, %q", searchAlias, caseAlias)
	}
	if len(searchAlias) > 64 || len(caseAlias) > 64 {
		t.Fatalf("generated aliases exceed 64 characters: %q, %q", searchAlias, caseAlias)
	}
	if got := gjson.GetBytes(out, "tools.4.description").String(); got != "unknown one" {
		t.Fatalf("description = %q, want preserved", got)
	}
	if got := gjson.GetBytes(out, "tools.4.input_schema.required.0").String(); got != "q" {
		t.Fatalf("input schema was not preserved: %s", out)
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != searchAlias {
		t.Fatalf("tool_choice.name = %q, want %q", got, searchAlias)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != searchAlias {
		t.Fatalf("historical tool_use.name = %q, want %q", got, searchAlias)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.id").String(); got != "toolu_unknown" {
		t.Fatalf("tool_use.id = %q, want unchanged", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != caseAlias {
		t.Fatalf("tool_reference.tool_name = %q, want %q", got, caseAlias)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.content.0.tool_name").String(); got != searchAlias {
		t.Fatalf("nested tool_reference.tool_name = %q, want %q", got, searchAlias)
	}
	if reverseMap[searchAlias] != "search_web" || reverseMap[caseAlias] != "Search_Web" ||
		reverseMap[bashAlias] != "bash" || reverseMap[readAlias] != "Read" {
		t.Fatalf("reverseMap = %v, want exact client names", reverseMap)
	}

	response := []byte(fmt.Sprintf(`{"content":[
		{"type":"tool_use","id":"toolu_unknown","name":%q,"input":{}},
		{"type":"tool_reference","tool_name":%q},
		{"type":"tool_result","tool_use_id":"toolu_unknown","content":[{"type":"tool_reference","tool_name":%q}]}
	]}`, searchAlias, caseAlias, searchAlias))
	restored, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNames() error = %v", errReverse)
	}
	if got := gjson.GetBytes(restored, "content.0.name").String(); got != "search_web" {
		t.Fatalf("restored tool_use.name = %q, want search_web", got)
	}
	if got := gjson.GetBytes(restored, "content.1.tool_name").String(); got != "Search_Web" {
		t.Fatalf("restored tool_reference.tool_name = %q, want Search_Web", got)
	}
	if got := gjson.GetBytes(restored, "content.2.content.0.tool_name").String(); got != "search_web" {
		t.Fatalf("restored nested tool_reference = %q, want search_web", got)
	}

	streamLine := []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_unknown","name":%q,"input":{}}}`, searchAlias))
	restoredLine, errReverse := reverseRemapOAuthToolNamesFromStreamLine(streamLine, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() error = %v", errReverse)
	}
	if got := gjson.GetBytes(helps.JSONPayload(restoredLine), "content_block.name").String(); got != "search_web" {
		t.Fatalf("restored stream name = %q, want search_web: %s", got, restoredLine)
	}
}

func TestRemapOAuthToolNames_TypedCustomUsesMCPAlias(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"custom","name":"client_custom","description":"keep","input_schema":{"type":"object","properties":{"value":{"type":"string"}}}},
			{"type":"web_search_20250305","name":"web_search","max_uses":2},
			{"type":"client_extension_v1","name":"client_extension","description":"extension","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"client_custom"},
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_custom","name":"client_custom","input":{}}]}]
	}`)
	out, reverseMap := remapOAuthToolNamesWithOptions(body, claudeMCPAliasOptions{secret: "caller-secret"})

	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) {
		t.Fatalf("typed custom alias = %q, want MCP name", alias)
	}
	if gjson.GetBytes(out, "tools.0.type").Exists() {
		t.Fatalf("typed custom type was not normalized away: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.description").String(); got != "keep" {
		t.Fatalf("typed custom description = %q, want preserved", got)
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "web_search" {
		t.Fatalf("server builtin name = %q, want unchanged", got)
	}
	extensionAlias := gjson.GetBytes(out, "tools.2.name").String()
	if !helps.IsClaudeMCPToolName(extensionAlias) || gjson.GetBytes(out, "tools.2.type").Exists() {
		t.Fatalf("unknown typed client tool was not normalized: %s", out)
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != alias {
		t.Fatalf("tool_choice.name = %q, want %q", got, alias)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != alias {
		t.Fatalf("historical tool_use.name = %q, want %q", got, alias)
	}
	if reverseMap[alias] != "client_custom" || reverseMap[extensionAlias] != "client_extension" {
		t.Fatalf("reverseMap = %v, want exact typed client names", reverseMap)
	}
}

func TestRemapOAuthToolNames_MCPAliasAvoidsClientCollision(t *testing.T) {
	const secret = "credential-secret"
	initialCandidate := helps.ClaudeMCPToolAlias(secret, "fetch_url", 0)
	body := []byte(fmt.Sprintf(`{"tools":[
		{"name":%q,"input_schema":{"type":"object"}},
		{"name":"fetch_url","input_schema":{"type":"object"}}
	]}`, initialCandidate))

	out, reverseMap := remapOAuthToolNamesWithOptions(body, claudeMCPAliasOptions{secret: secret})
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != initialCandidate {
		t.Fatalf("existing MCP tool = %q, want %q", got, initialCandidate)
	}
	alias := gjson.GetBytes(out, "tools.1.name").String()
	if alias == initialCandidate {
		t.Fatalf("generated alias collided with client MCP name %q", alias)
	}
	if reverseMap[alias] != "fetch_url" {
		t.Fatalf("reverseMap = %v, want %q -> fetch_url", reverseMap, alias)
	}
}

func TestRemapOAuthToolNames_MCPAliasIsMandatory(t *testing.T) {
	body := []byte(`{"tools":[{"name":"search_web","input_schema":{"type":"object"}}]}`)
	out, reverseMap := remapOAuthToolNames(body)
	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) {
		t.Fatalf("tools.0.name = %q, want mandatory MCP alias", alias)
	}
	if reverseMap[alias] != "search_web" {
		t.Fatalf("reverseMap = %v, want alias -> search_web", reverseMap)
	}
}

func TestRemapOAuthToolNames_SemanticAliasRestoresLongOriginal(t *testing.T) {
	original := "Read.file/with a very long semantic name and Unicode 网页内容 that exceeds the wire limit"
	body := []byte(`{"tools":[{"name":` + fmt.Sprintf("%q", original) + `,"input_schema":{"type":"object"}}]}`)
	options := claudeMCPAliasOptions{secret: "stable-caller"}

	out, reverseMap := remapOAuthToolNamesWithOptions(body, options)
	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) || len(alias) > 64 {
		t.Fatalf("semantic alias is invalid or too long: len=%d name=%q", len(alias), alias)
	}
	if !strings.Contains(alias, "_Read_file_with_a_very_long") {
		t.Fatalf("semantic alias %q does not expose the truncated original meaning", alias)
	}
	if reverseMap[alias] != original {
		t.Fatalf("reverseMap lost exact original: got %q, want %q", reverseMap[alias], original)
	}

	second, _ := remapOAuthToolNamesWithOptions(body, options)
	if got := gjson.GetBytes(second, "tools.0.name").String(); got != alias {
		t.Fatalf("semantic alias is not stable across requests: %q != %q", got, alias)
	}
	response := []byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":` + fmt.Sprintf("%q", alias) + `,"input":{}}]}`)
	restored, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNames() error = %v", errReverse)
	}
	if got := gjson.GetBytes(restored, "content.0.name").String(); got != original {
		t.Fatalf("restored tool name = %q, want exact original %q", got, original)
	}
}

func TestPrepareClaudeDesktopToolNamesForUpstream_PreservesMCPConvention(t *testing.T) {
	body := []byte(`{"tools":[
		{"name":"search_web","input_schema":{"type":"object"}},
		{"name":"mcp__context7__query-docs","input_schema":{"type":"object"}},
		{"name":"bash","input_schema":{"type":"object"}}
	],"tool_choice":{"type":"tool","name":"search_web"}}`)
	out, reverseMap := prepareClaudeDesktopToolNamesForUpstream(body, claudeMCPAliasOptions{secret: "credential-secret"})

	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) || strings.HasPrefix(alias, "proxy_") {
		t.Fatalf("unknown alias = %q, want bare mcp__ name", alias)
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "mcp__context7__query-docs" {
		t.Fatalf("existing MCP name = %q, want unchanged", got)
	}
	bashAlias := gjson.GetBytes(out, "tools.2.name").String()
	if !helps.IsClaudeMCPToolName(bashAlias) || strings.HasPrefix(bashAlias, "proxy_") {
		t.Fatalf("former vetted tool = %q, want bare MCP alias", bashAlias)
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != alias {
		t.Fatalf("tool_choice.name = %q, want %q", got, alias)
	}
	if reverseMap[alias] != "search_web" || reverseMap[bashAlias] != "bash" {
		t.Fatalf("reverseMap = %v, want exact alias restoration", reverseMap)
	}
}

func TestResolveClaudeMCPAliasOptions(t *testing.T) {
	if options := resolveClaudeMCPAliasOptions(context.Background()); options.secret == "" {
		t.Fatal("default caller alias secret is empty")
	}

	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("userApiKey", "downstream-caller-one")
	callerCtx := context.WithValue(context.Background(), "gin", ginCtx)
	firstSecret := resolveClaudeMCPAliasOptions(callerCtx).secret
	secondSecret := resolveClaudeMCPAliasOptions(callerCtx).secret
	if firstSecret == "" || secondSecret != firstSecret {
		t.Fatalf("caller alias secret is unstable: %q != %q", firstSecret, secondSecret)
	}
	otherGinCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	otherGinCtx.Set("userApiKey", "downstream-caller-two")
	otherCtx := context.WithValue(context.Background(), "gin", otherGinCtx)
	if otherSecret := resolveClaudeMCPAliasOptions(otherCtx).secret; otherSecret == firstSecret {
		t.Fatalf("different downstream callers shared alias secret %q", firstSecret)
	}
}

func TestRemapOAuthToolNames_MixedCaseNamesRemainDistinct(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object"}},` +
		`{"name":"bash","input_schema":{"type":"object"}}` +
		`]}`)
	out, reverseMap := remapOAuthToolNames(body)
	upperAlias := gjson.GetBytes(out, "tools.0.name").String()
	lowerAlias := gjson.GetBytes(out, "tools.1.name").String()
	if !helps.IsClaudeMCPToolName(upperAlias) || !helps.IsClaudeMCPToolName(lowerAlias) || upperAlias == lowerAlias {
		t.Fatalf("mixed-case aliases = %q, %q, want distinct MCP names", upperAlias, lowerAlias)
	}
	if reverseMap[upperAlias] != "Bash" || reverseMap[lowerAlias] != "bash" {
		t.Fatalf("reverseMap = %v, want exact mixed-case names", reverseMap)
	}
}

// TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap guards the
// SSE streaming code path against the same mixed-case bug.
func TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	// Bash block was never renamed, must pass through as-is.
	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}`)
	out, errReverse := reverseRemapOAuthToolNamesFromStreamLine(bashLine, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() error = %v", errReverse)
	}
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	// Glob block IS in the reverseMap, must be restored to `glob`.
	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"Glob","input":{}}}`)
	out, errReverse = reverseRemapOAuthToolNamesFromStreamLine(globLine, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() error = %v", errReverse)
	}
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

func TestPrepareClaudeDesktopToolNamesForUpstream_AllCustomToolsWithHistory(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`],"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"glob","input":{}}` +
		`]}]}`)

	out, reverseMap := prepareClaudeDesktopToolNamesForUpstream(body, claudeMCPAliasOptions{secret: "mixed-case-caller"})
	bashAlias := gjson.GetBytes(out, "tools.0.name").String()
	globAlias := gjson.GetBytes(out, "tools.1.name").String()
	if !helps.IsClaudeMCPToolName(bashAlias) || !helps.IsClaudeMCPToolName(globAlias) || bashAlias == globAlias {
		t.Fatalf("tool aliases = %q, %q, want distinct bare MCP names", bashAlias, globAlias)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != bashAlias {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, bashAlias)
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != globAlias {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, globAlias)
	}
	if reverseMap[bashAlias] != "Bash" || reverseMap[globAlias] != "glob" {
		t.Fatalf("reverseMap = %v, want exact client names", reverseMap)
	}
}

func TestPrependClaudeSystemReminders_FollowsToolResultsAndIsIdempotent(t *testing.T) {
	payload := []byte(`{"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"continue"}]}` +
		`]}`)

	texts := []string{"first guidance", "second guidance"}
	first := prependClaudeSystemRemindersToFirstUserMessage(payload, texts)
	second := prependClaudeSystemRemindersToFirstUserMessage(first, texts)
	if !bytes.Equal(first, second) {
		t.Fatalf("caller reminder insertion is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	content := gjson.GetBytes(first, "messages.1.content").Array()
	if len(content) != 4 {
		t.Fatalf("content has %d blocks, want tool_result, two caller reminders, and user text", len(content))
	}
	if got := content[0].Get("type").String(); got != "tool_result" {
		t.Fatalf("content[0].type = %q, want tool_result", got)
	}
	for idx, text := range texts {
		if got := content[idx+1].Get("text").String(); got != claudeCallerSystemReminder(text) {
			t.Fatalf("content[%d].text = %q, want caller reminder %q", idx+1, got, text)
		}
	}
	if got := content[3].Get("text").String(); got != "continue" {
		t.Fatalf("content[3].text = %q, want user text", got)
	}
}

func TestInsertClaudeMidConversationSystemMessages_FollowsToolResultUserTurn(t *testing.T) {
	payload := []byte(`{"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}` +
		`]}`)

	out := insertClaudeMidConversationSystemMessages(payload, []string{"guidance"})
	if got := gjson.GetBytes(out, "messages.#").Int(); got != 3 {
		t.Fatalf("message count = %d, want 3: %s", got, out)
	}
	blocks := gjson.GetBytes(out, "messages.1.content")
	if got := blocks.Get("0.type").String(); got != "tool_result" {
		t.Fatalf("first block type = %q, want tool_result: %s", got, out)
	}
	if got := blocks.Get("0.tool_use_id").String(); got != "toolu_1" {
		t.Fatalf("tool_use_id = %q, want toolu_1: %s", got, out)
	}
	assertClaudeMidConversationSystemMessage(t, out, 2, "guidance", "")
}

func TestInsertClaudeMidConversationSystemMessages_PrecedesExistingAssistantTurn(t *testing.T) {
	payload := []byte(`{"messages":[` +
		`{"role":"user","content":"hello"},` +
		`{"role":"assistant","content":"answer"},` +
		`{"role":"user","content":"continue"}` +
		`]}`)

	out := insertClaudeMidConversationSystemMessages(payload, []string{"guidance"})
	roles := gjson.GetBytes(out, "messages.#.role").Array()
	wantRoles := []string{"user", "system", "assistant", "user"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("message count = %d, want %d: %s", len(roles), len(wantRoles), out)
	}
	for idx, wantRole := range wantRoles {
		if got := roles[idx].String(); got != wantRole {
			t.Fatalf("messages[%d].role = %q, want %q", idx, got, wantRole)
		}
	}
	assertClaudeMidConversationSystemMessage(t, out, 1, "guidance", "")
}

func TestInsertClaudeMidConversationSystemMessages_FollowsConsecutiveUserRun(t *testing.T) {
	payload := []byte(`{"messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"user","content":"second"},` +
		`{"role":"assistant","content":"answer"}` +
		`]}`)

	out := insertClaudeMidConversationSystemMessages(payload, []string{"guidance"})
	roles := gjson.GetBytes(out, "messages.#.role").Array()
	wantRoles := []string{"user", "user", "system", "assistant"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("message count = %d, want %d: %s", len(roles), len(wantRoles), out)
	}
	for idx, wantRole := range wantRoles {
		if got := roles[idx].String(); got != wantRole {
			t.Fatalf("messages[%d].role = %q, want %q", idx, got, wantRole)
		}
	}
	assertClaudeMidConversationSystemMessage(t, out, 2, "guidance", "")
}

func TestInsertClaudeMidConversationSystemMessages_IsIdempotent(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	texts := []string{"first guidance", "second guidance"}
	first := insertClaudeMidConversationSystemMessages(payload, texts)
	second := insertClaudeMidConversationSystemMessages(first, texts)
	if !bytes.Equal(first, second) {
		t.Fatalf("mid-conversation system insertion is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	if got := gjson.GetBytes(first, "messages.#").Int(); got != 3 {
		t.Fatalf("message count = %d, want user and two system messages: %s", got, first)
	}
	assertClaudeMidConversationSystemMessage(t, first, 1, texts[0], "")
	assertClaudeMidConversationSystemMessage(t, first, 2, texts[1], "")
}

// TestApplyClaudeHeaders_StreamTransportNegotiation keeps the compatibility
// provider's body-driven SSE behavior stable.
func TestApplyClaudeHeaders_StreamTransportNegotiation(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-stream-accept"}}
	body := []byte(`{"model":"claude-opus-4-6","stream":true}`)

	directReq := newClaudeHeaderTestRequest(t, http.Header{})
	if errApply := applyClaudeHeaders(directReq, auth, "key-stream-accept", true, nil, body, http.Header{}); errApply != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errApply)
	}
	if got, want := directReq.Header.Get("Accept"), "application/json"; got != want {
		t.Fatalf("streaming Accept = %q, want %q to match the real client", got, want)
	}
	if got, want := directReq.Header.Get("Accept-Encoding"), "gzip, deflate, br, zstd"; got != want {
		t.Fatalf("streaming Accept-Encoding = %q, want %q to match the real client", got, want)
	}

	gatewayReq := httptest.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/messages", nil)
	gatewayReq = gatewayReq.WithContext(directReq.Context())
	if errApply := applyClaudeHeaders(gatewayReq, auth, "key-stream-accept", true, nil, body, http.Header{}); errApply != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errApply)
	}
	if got, want := gatewayReq.Header.Get("Accept"), "text/event-stream"; got != want {
		t.Fatalf("gateway streaming Accept = %q, want %q", got, want)
	}
	if got, want := gatewayReq.Header.Get("Accept-Encoding"), "identity"; got != want {
		t.Fatalf("gateway streaming Accept-Encoding = %q, want %q", got, want)
	}
}

func TestApplyClaudeHeaders_DefaultPreservesCallerBetas(t *testing.T) {
	incoming := http.Header{"Anthropic-Beta": []string{"caller-only-beta"}}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-caller-betas"}}
	body := []byte(`{"model":"claude-opus-4-6"}`)

	// Default API-key mode preserves caller betas on direct Anthropic.
	directReq := newClaudeHeaderTestRequest(t, incoming)
	if errApply := applyClaudeHeaders(directReq, auth, "key-caller-betas", false, nil, body, incoming); errApply != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errApply)
	}
	if got := directReq.Header.Get("Anthropic-Beta"); got != "caller-only-beta" {
		t.Fatalf("Anthropic-Beta = %q, want caller beta on api.anthropic.com", got)
	}

	// Other Anthropic-compatible upstreams keep caller betas functional.
	gatewayReq := httptest.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/messages", nil)
	gatewayReq = gatewayReq.WithContext(directReq.Context())
	if errApply := applyClaudeHeaders(gatewayReq, auth, "key-caller-betas", false, nil, body, incoming); errApply != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errApply)
	}
	if got := gatewayReq.Header.Get("Anthropic-Beta"); !strings.Contains(got, "caller-only-beta") {
		t.Fatalf("Anthropic-Beta = %q, want caller beta preserved on non-Anthropic upstream", got)
	}
}

func TestInjectClaudeDesktopContextManagement(t *testing.T) {
	const captured = `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`

	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "enabled thinking", payload: `{"model":"claude-opus-5","thinking":{"type":"enabled"}}`},
		{name: "adaptive thinking", payload: `{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, automaticallyInjected := injectClaudeDesktopContextManagement([]byte(test.payload))
			if !automaticallyInjected {
				t.Fatal("automatic context_management injection was not reported")
			}
			if diff := gjson.GetBytes(got, "context_management").Raw; diff != captured {
				t.Fatalf("context_management = %s, want the captured object %s", diff, captured)
			}
		})
	}

	callerOwned := []byte(`{"model":"claude-opus-4-6","context_management":{"edits":[]}}`)
	callerOwnedGot, automaticallyInjected := injectClaudeDesktopContextManagement(callerOwned)
	if automaticallyInjected {
		t.Error("caller context_management was reported as automatically injected")
	}
	if !bytes.Equal(callerOwnedGot, callerOwned) {
		t.Fatalf("caller context_management was modified: %s", callerOwnedGot)
	}

	// Anthropic rejects clear_thinking_20251015 unless thinking is enabled or
	// adaptive, so an omitted thinking field is as ineligible as an explicit
	// disabled one.
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "disabled thinking", payload: `{"model":"claude-opus-5","thinking":{"type":"disabled"}}`},
		{name: "omitted thinking", payload: `{"model":"claude-opus-4-6"}`},
		{name: "unknown thinking", payload: `{"model":"claude-opus-5","thinking":{"type":"unexpected"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ineligible := []byte(test.payload)
			got, automaticallyInjected := injectClaudeDesktopContextManagement(ineligible)
			if automaticallyInjected {
				t.Error("ineligible thinking context_management was reported as automatically injected")
			}
			if !bytes.Equal(got, ineligible) {
				t.Errorf("ineligible payload was modified: %s", got)
			}
			if cm := gjson.GetBytes(got, "context_management"); cm.Exists() {
				t.Errorf("context_management = %s, want absent", cm.Raw)
			}
		})
	}
}

// Anthropic rejects a request carrying the clear_thinking_20251015 strategy
// without enabled/adaptive thinking:
//
//	`clear_thinking_20251015` strategy requires `thinking` to be enabled or adaptive
//
// This walks the real execute.go ordering, where disableThinkingIfToolChoiceForced
// deletes the thinking field between injection and reconciliation.
func TestClaudeDesktopContextManagementNeverOutlivesEligibleThinking(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		wantCM  bool
	}{
		{
			name:    "thinking omitted from the start",
			payload: `{"model":"claude-opus-5","messages":[]}`,
		},
		{
			name:    "forced tool_choice strips thinking after injection",
			payload: `{"model":"claude-opus-5","thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"any"},"messages":[]}`,
		},
		{
			name:    "thinking survives without forced tool_choice",
			payload: `{"model":"claude-opus-5","thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`,
			wantCM:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, injected := injectClaudeDesktopContextManagement([]byte(test.payload))
			state := claudeDesktopContextManagementState{eligible: true, automaticallyInjected: injected}
			body = disableThinkingIfToolChoiceForced(body)
			body = reconcileClaudeDesktopContextManagement(body, state)

			thinkingEligible := gjson.GetBytes(body, "thinking.type").String() == "enabled" ||
				gjson.GetBytes(body, "thinking.type").String() == "adaptive"
			cm := gjson.GetBytes(body, "context_management")
			if cm.Exists() && !thinkingEligible {
				t.Fatalf("context_management = %s survived ineligible thinking; Anthropic would reject this: %s", cm.Raw, body)
			}
			if cm.Exists() != test.wantCM {
				t.Fatalf("context_management present = %v, want %v; body=%s", cm.Exists(), test.wantCM, body)
			}
		})
	}
}

func TestReconcileClaudeDesktopContextManagement(t *testing.T) {
	withAutomatic := func(thinkingType string) string {
		return `{"thinking":{"type":"` + thinkingType + `"},"context_management":` + claudeDesktopContextManagement + `}`
	}

	for _, test := range []struct {
		name    string
		payload string
		state   claudeDesktopContextManagementState
		wantRaw string
	}{
		{
			name:    "removes unchanged automatic object when disabled",
			payload: withAutomatic("disabled"),
			state:   claudeDesktopContextManagementState{eligible: true, automaticallyInjected: true},
		},
		{
			name:    "preserves rule owned automatic object when disabled",
			payload: withAutomatic("disabled"),
			state:   claudeDesktopContextManagementState{eligible: true, automaticallyInjected: true, payloadRuleTouched: true},
			wantRaw: claudeDesktopContextManagement,
		},
		{
			name:    "preserves changed automatic object when disabled",
			payload: `{"thinking":{"type":"disabled"},"context_management":{"edits":[{"type":"custom"}]}}`,
			state:   claudeDesktopContextManagementState{eligible: true, automaticallyInjected: true},
			wantRaw: `{"edits":[{"type":"custom"}]}`,
		},
		{
			name:    "adds automatic object when enabled",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeDesktopContextManagementState{eligible: true},
			wantRaw: claudeDesktopContextManagement,
		},
		{
			name:    "adds automatic object when adaptive",
			payload: `{"thinking":{"type":"adaptive"}}`,
			state:   claudeDesktopContextManagementState{eligible: true},
			wantRaw: claudeDesktopContextManagement,
		},
		{
			name:    "caller ownership prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeDesktopContextManagementState{eligible: true, callerOwned: true},
		},
		{
			name:    "payload rule ownership prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeDesktopContextManagementState{eligible: true, payloadRuleTouched: true},
		},
		{
			name:    "ineligible request prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
		},
		{
			name:    "omitted thinking prevents addition",
			payload: `{}`,
			state:   claudeDesktopContextManagementState{eligible: true},
		},
		{
			name:    "removes automatic object when thinking was stripped entirely",
			payload: `{"context_management":` + claudeDesktopContextManagement + `}`,
			state:   claudeDesktopContextManagementState{eligible: true, automaticallyInjected: true},
		},
		{
			name:    "keeps caller object when thinking was stripped entirely",
			payload: `{"context_management":` + claudeDesktopContextManagement + `}`,
			state:   claudeDesktopContextManagementState{eligible: true, callerOwned: true},
			wantRaw: claudeDesktopContextManagement,
		},
		{
			name:    "unknown thinking prevents addition",
			payload: `{"thinking":{"type":"unexpected"}}`,
			state:   claudeDesktopContextManagementState{eligible: true},
		},
		{
			name:    "invalid thinking prevents addition",
			payload: `{"thinking":{"type":123}}`,
			state:   claudeDesktopContextManagementState{eligible: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := reconcileClaudeDesktopContextManagement([]byte(test.payload), test.state)
			if raw := gjson.GetBytes(got, "context_management").Raw; raw != test.wantRaw {
				t.Fatalf("context_management = %s, want %s; body=%s", raw, test.wantRaw, got)
			}
		})
	}
}

func TestClaudeExecutorPayloadOverrideDisabledThinking(t *testing.T) {
	const model = "claude-opus-5"
	modelRules := []config.PayloadModelRule{{Name: model, Protocol: "claude"}}
	basePayload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	for _, test := range []struct {
		name   string
		stream bool
	}{
		{name: "execute"},
		{name: "execute stream", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: modelRules,
				Params: map[string]any{"thinking.type": "disabled"},
			}}}}
			upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, test.stream)
			if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != "disabled" {
				t.Fatalf("final upstream thinking.type = %q, want disabled; body=%s", got, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
				t.Errorf("final upstream context_management = %s with disabled thinking, want absent", got.Raw)
			}
		})
	}

	t.Run("caller context management is preserved", func(t *testing.T) {
		cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: modelRules,
			Params: map[string]any{"thinking.type": "disabled"},
		}}}}
		payload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[{"type":"caller_owned"}]}}`)
		upstreamBody := executeClaudeContextManagementRequest(t, cfg, payload, false)
		if got := gjson.GetBytes(upstreamBody, "context_management.edits.0.type").String(); got != "caller_owned" {
			t.Fatalf("caller context_management type = %q, want caller_owned; body=%s", got, upstreamBody)
		}
	})

	t.Run("payload override replacement is preserved", func(t *testing.T) {
		cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: modelRules,
			Params: map[string]any{
				"thinking.type":      "disabled",
				"context_management": map[string]any{"edits": []any{map[string]any{"type": "payload_rule"}}},
			},
		}}}}
		upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, false)
		if got := gjson.GetBytes(upstreamBody, "context_management.edits.0.type").String(); got != "payload_rule" {
			t.Fatalf("payload-rule context_management type = %q, want payload_rule; body=%s", got, upstreamBody)
		}
	})

	t.Run("exact automatic value remains payload rule owned", func(t *testing.T) {
		ownershipConfigs := []struct {
			name string
			cfg  *config.Config
		}{
			{
				name: "default",
				cfg: &config.Config{Payload: config.PayloadConfig{
					Default: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"context_management": json.RawMessage(claudeDesktopContextManagement)},
					}},
					Override: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"thinking.type": "disabled"},
					}},
				}},
			},
			{
				name: "raw default",
				cfg: &config.Config{Payload: config.PayloadConfig{
					DefaultRaw: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"context_management": claudeDesktopContextManagement},
					}},
					Override: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"thinking.type": "disabled"},
					}},
				}},
			},
			{
				name: "override",
				cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: modelRules,
					Params: map[string]any{
						"thinking.type":      "disabled",
						"context_management": json.RawMessage(claudeDesktopContextManagement),
					},
				}}}},
			},
			{
				name: "raw override",
				cfg: &config.Config{Payload: config.PayloadConfig{
					Override: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"thinking.type": "disabled"},
					}},
					OverrideRaw: []config.PayloadRule{{
						Models: modelRules,
						Params: map[string]any{"context_management": claudeDesktopContextManagement},
					}},
				}},
			},
		}
		for _, ownership := range ownershipConfigs {
			for _, stream := range []bool{false, true} {
				name := ownership.name + " execute"
				if stream {
					name += " stream"
				}
				t.Run(name, func(t *testing.T) {
					upstreamBody := executeClaudeContextManagementRequest(t, ownership.cfg, basePayload, stream)
					if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != "disabled" {
						t.Fatalf("final upstream thinking.type = %q, want disabled; body=%s", got, upstreamBody)
					}
					if got := gjson.GetBytes(upstreamBody, "context_management").Raw; got != claudeDesktopContextManagement {
						t.Fatalf("%s context_management = %s, want payload-rule-owned %s; body=%s", ownership.name, got, claudeDesktopContextManagement, upstreamBody)
					}
				})
			}
		}
	})

	t.Run("payload filter remains effective", func(t *testing.T) {
		cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
			Models: modelRules,
			Params: []string{"context_management"},
		}}}}
		upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, false)
		if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
			t.Fatalf("filtered context_management = %s, want absent", got.Raw)
		}
	})

	for _, stream := range []bool{false, true} {
		// Anthropic rejects the automatic strategy once forced tool choice has
		// stripped thinking:
		//
		//	`clear_thinking_20251015` strategy requires `thinking` to be enabled or adaptive
		name := "forced tool choice drops automatic context management execute"
		if stream {
			name += " stream"
		}
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}}`)
			upstreamBody := executeClaudeContextManagementRequest(t, &config.Config{}, payload, stream)
			if got := gjson.GetBytes(upstreamBody, "thinking"); got.Exists() {
				t.Fatalf("forced tool choice thinking = %s, want absent", got.Raw)
			}
			if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
				t.Fatalf("forced tool choice context_management = %s, want absent because Anthropic rejects it without thinking", got.Raw)
			}
			if got := gjson.GetBytes(upstreamBody, "tool_choice.type").String(); got != "any" {
				t.Fatalf("forced tool_choice.type = %q, want any", got)
			}
		})
	}
}

func TestAnthropicCompatiblePayloadOverrideReenablesThinking(t *testing.T) {
	const model = "claude-opus-5"
	modelRules := []config.PayloadModelRule{{Name: model, Protocol: "claude"}}
	basePayload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)

	for _, test := range []struct {
		name         string
		thinkingType string
		stream       bool
	}{
		{name: "execute enabled", thinkingType: "enabled"},
		{name: "execute adaptive", thinkingType: "adaptive"},
		{name: "execute stream enabled", thinkingType: "enabled", stream: true},
		{name: "execute stream adaptive", thinkingType: "adaptive", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: modelRules,
				Params: map[string]any{"thinking.type": test.thinkingType},
			}}}}
			upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, test.stream)
			if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != test.thinkingType {
				t.Fatalf("final upstream thinking.type = %q, want %q; body=%s", got, test.thinkingType, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
				t.Fatalf("anthropic-compatible injected Desktop context_management = %s; body=%s", got.Raw, upstreamBody)
			}
		})
	}

	for _, stream := range []bool{false, true} {
		nameSuffix := "execute"
		if stream {
			nameSuffix = "execute stream"
		}

		t.Run("caller context management is preserved after re-enabling "+nameSuffix, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: modelRules,
				Params: map[string]any{"thinking.type": "enabled"},
			}}}}
			payload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"context_management":{"edits":[{"type":"caller_owned"}]}}`)
			upstreamBody := executeClaudeContextManagementRequest(t, cfg, payload, stream)
			if got := gjson.GetBytes(upstreamBody, "context_management.edits.0.type").String(); got != "caller_owned" {
				t.Fatalf("caller context_management type = %q, want caller_owned; body=%s", got, upstreamBody)
			}
		})

		t.Run("custom payload rule object is preserved after re-enabling "+nameSuffix, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: modelRules,
				Params: map[string]any{
					"thinking.type":      "adaptive",
					"context_management": map[string]any{"edits": []any{map[string]any{"type": "payload_rule"}}},
				},
			}}}}
			upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, stream)
			if got := gjson.GetBytes(upstreamBody, "context_management.edits.0.type").String(); got != "payload_rule" {
				t.Fatalf("payload-rule context_management type = %q, want payload_rule; body=%s", got, upstreamBody)
			}
		})

		t.Run("context management filter remains authoritative after re-enabling "+nameSuffix, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{
				Override: []config.PayloadRule{{
					Models: modelRules,
					Params: map[string]any{"thinking.type": "enabled"},
				}},
				Filter: []config.PayloadFilterRule{{
					Models: modelRules,
					Params: []string{"context_management"},
				}},
			}}
			upstreamBody := executeClaudeContextManagementRequest(t, cfg, basePayload, stream)
			if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != "enabled" {
				t.Fatalf("final upstream thinking.type = %q, want enabled; body=%s", got, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
				t.Fatalf("filtered context_management = %s after re-enabling, want absent; body=%s", got.Raw, upstreamBody)
			}
		})
	}
}

func executeClaudeContextManagementRequest(t *testing.T, cfg *config.Config, payload []byte, stream bool) []byte {
	t.Helper()

	var upstreamBody []byte
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		contentType := "application/json"
		responseBody := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if stream {
			contentType = "text/event-stream"
			responseBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := newAnthropicCompatibleTestExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-payload-rule"}}
	request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	if stream {
		result, errStream := executor.ExecuteStream(ctx, auth, request, options)
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
		}
		return upstreamBody
	}
	if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	return upstreamBody
}

func TestValidateClaudeCallerSystemBlocksAcceptsTextOnly(t *testing.T) {
	tests := []struct {
		name   string
		system string
	}{
		{name: "string", system: `"S1"`},
		{name: "text blocks", system: `[{"type":"text","text":"S1"},{"type":"text","text":"S2"}]`},
		{name: "absent", system: ``},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"model":"claude-opus-5"}`
			if test.system != "" {
				payload = `{"model":"claude-opus-5","system":` + test.system + `}`
			}
			if err := validateClaudeCallerSystemBlocks(gjson.Get(payload, "system")); err != nil {
				t.Fatalf("validateClaudeCallerSystemBlocks() error = %v, want nil", err)
			}
		})
	}
}

// Anthropic rejects every non-text block in both system slots, verified live on
// 2026-08-03: the top-level field answers "system.<i>.type: Input should be
// 'text'" and a role=system message answers "role 'system' supports text,
// tool_addition, and tool_removal blocks only". Cloaking has no third slot, so
// the request has to fail here instead of losing the caller's instructions.
func TestValidateClaudeCallerSystemBlocksRejectsNonTextBlock(t *testing.T) {
	tests := []struct {
		name      string
		system    string
		wantIndex string
		wantType  string
	}{
		{
			name:      "image",
			system:    `[{"type":"text","text":"S1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`,
			wantIndex: "system.1.type",
			wantType:  `"image"`,
		},
		{
			name:      "responses marker",
			system:    `[{"type":"input_file"}]`,
			wantIndex: "system.0.type",
			wantType:  `"input_file"`,
		},
		{
			name:      "missing type",
			system:    `[{"text":"S1"}]`,
			wantIndex: "system.0.type",
			wantType:  `"unknown"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaudeCallerSystemBlocks(gjson.Parse(test.system))
			if err == nil {
				t.Fatal("validateClaudeCallerSystemBlocks() error = nil, want rejection")
			}
			var statusCoder interface{ StatusCode() int }
			if !errors.As(err, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
				t.Fatalf("error status = %v, want 400", err)
			}
			var scoped interface{ IsRequestScoped() bool }
			if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
				t.Fatalf("error %v must be request scoped so no other credential is tried", err)
			}
			if got := err.Error(); !strings.Contains(got, test.wantIndex) || !strings.Contains(got, test.wantType) {
				t.Fatalf("error = %q, want it to name %s and %s", got, test.wantIndex, test.wantType)
			}
		})
	}
}

func TestAnthropicCompatiblePreservesCallerAgentAndEnvironmentHeaders(t *testing.T) {
	tests := []struct {
		name            string
		incomingHeaders http.Header
		wantHeaders     map[string]string
		wantAbsent      []string
	}{
		{
			name: "preserves canonical agent and parent agent headers",
			incomingHeaders: http.Header{
				"X-Claude-Code-Agent-Id":        {"subagent-001"},
				"X-Claude-Code-Parent-Agent-Id": {"parent-agent-root"},
			},
			wantHeaders: map[string]string{
				"X-Claude-Code-Agent-Id":        "subagent-001",
				"X-Claude-Code-Parent-Agent-Id": "parent-agent-root",
			},
		},
		{
			name: "preserves lowercased agent and environment headers",
			incomingHeaders: http.Header{
				"x-claude-code-agent-id":            {"agent-xyz"},
				"x-claude-remote-container-id":      {"container-123"},
				"x-claude-remote-session-id":        {"remote-sess-456"},
				"x-client-app":                      {"custom-sdk"},
				"x-anthropic-additional-protection": {"true"},
			},
			wantHeaders: map[string]string{
				"X-Claude-Code-Agent-Id":            "agent-xyz",
				"X-Claude-Remote-Container-Id":      "container-123",
				"X-Claude-Remote-Session-Id":        "remote-sess-456",
				"X-Client-App":                      "custom-sdk",
				"X-Anthropic-Additional-Protection": "true",
			},
		},
		{
			name: "does not fabricate agent header when absent",
			incomingHeaders: http.Header{
				"User-Agent": {"test-client"},
			},
			wantAbsent: []string{
				"X-Claude-Code-Agent-Id",
				"X-Claude-Code-Parent-Agent-Id",
				"X-Claude-Remote-Container-Id",
				"X-Claude-Remote-Session-Id",
				"X-Client-App",
				"X-Anthropic-Additional-Protection",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_agent","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer server.Close()

			executor := newAnthropicCompatibleTestExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				ID: "agent-header-test",
				Attributes: map[string]string{
					"api_key":  "sk-ant-test-key",
					"base_url": server.URL,
				},
				Metadata: claudeOAuthTestMetadata(),
			}

			_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-opus-4-6",
				Payload: []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatClaude,
				Headers:      tt.incomingHeaders,
			})
			if errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}

			for wantKey, wantVal := range tt.wantHeaders {
				if got := seenHeaders.Get(wantKey); got != wantVal {
					t.Errorf("header %s = %q, want %q", wantKey, got, wantVal)
				}
			}
			for _, absentKey := range tt.wantAbsent {
				if got := seenHeaders.Get(absentKey); got != "" {
					t.Errorf("header %s = %q, want absent", absentKey, got)
				}
			}
		})
	}
}
