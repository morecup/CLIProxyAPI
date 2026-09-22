package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeRaceProbeOAuthKey = "sk-ant-oat-beta-policy"

func claudeOAuthAuthForBetaPolicy() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "claude-beta-policy",
		Metadata: map[string]any{"access_token": claudeRaceProbeOAuthKey},
	}
}

// A confirmed native client authenticates to CPA with the user's configured key
// and cannot know CPA will pick an OAuth credential upstream, so its header never
// carries the credential-scoped OAuth and extended-cache betas.
func TestClaudeExecutor_ContextManagementNeverLeaksToOtherUpstreams(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "claude-non-anthropic-upstream",
		Attributes: map[string]string{"api_key": "sk-ant-oat-non-anthropic", "base_url": server.URL},
		Metadata:   claudeOAuthTestMetadata(),
	}
	payload := []byte(`{"model":"claude-opus-5","system":"p","messages":[{"role":"user","content":"hi"}]}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(upstreamBody, "context_management"); got.Exists() {
		t.Fatalf("non-Anthropic upstream received context_management = %s", got.Raw)
	}
}

func TestIsAnthropicUpstreamBase(t *testing.T) {
	cases := map[string]bool{
		"https://api.anthropic.com":      true,
		"https://API.Anthropic.com":      true,
		"https://api.anthropic.com:443":  true,
		"https://api.anthropic.com:8443": false,
		"https://user@api.anthropic.com": false,
		"https://api.kimi.com":           false,
		"http://api.anthropic.com":       false,
		"https://api.anthropic.com.evil": false,
		"https://gateway.example.com":    false,
		"":                               false,
	}
	for base, want := range cases {
		if got := isAnthropicUpstreamBase(base); got != want {
			t.Fatalf("isAnthropicUpstreamBase(%q) = %v, want %v", base, got, want)
		}
	}
}

// Streaming previously never reached the fast-mode derivation, so speed:"fast"
// produced a 400 on every streamed request.
func TestClassifyClaudeUpstreamError_FastModeCreditsIsRequestScoped(t *testing.T) {
	// Anthropic and the Claude Code CLI word this refusal differently; both must
	// be recognised, and neither may be rewritten on the way back to the caller.
	bodies := [][]byte{
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."}}`),
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Fast mode requires usage credits"}}`),
	}
	for _, body := range bodies {
		err := classifyClaudeUpstreamError(http.StatusTooManyRequests, nil, body)

		scoped, ok := err.(cliproxyexecutor.RequestScopedError)
		if !ok || !scoped.IsRequestScoped() {
			t.Fatalf("fast-mode credit refusal = %T, want a request-scoped error: %s", err, body)
		}
		var status cliproxyexecutor.StatusError
		if !errors.As(err, &status) || status.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("status was not preserved for the caller: %v", err)
		}
		// Pass-through must be byte-exact: the upstream body is the caller's
		// only explanation of what to do about it.
		if err.Error() != string(body) {
			t.Fatalf("body was rewritten:\n got  %s\n want %s", err.Error(), body)
		}
	}
}

// A genuine rate limit must keep cooling the credential down and rotating.
func TestClassifyClaudeUpstreamError_RealRateLimitStaysCredentialScoped(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit."}}`),
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This organization has exceeded its usage limit."}}`),
	}
	for _, body := range cases {
		err := classifyClaudeUpstreamError(http.StatusTooManyRequests, nil, body)
		if scoped, ok := err.(cliproxyexecutor.RequestScopedError); ok && scoped.IsRequestScoped() {
			t.Fatalf("genuine rate limit was misclassified as request-scoped: %s", body)
		}
	}
}

func TestClassifyClaudeUpstreamError_OtherStatusesUnaffected(t *testing.T) {
	body := []byte(`{"error":{"message":"Usage credits are required for fast mode."}}`)
	// Only 429 carries the entitlement refusal; a 500 mentioning it is still a
	// credential-scoped failure worth rotating away from.
	err := classifyClaudeUpstreamError(http.StatusInternalServerError, nil, body)
	if scoped, ok := err.(cliproxyexecutor.RequestScopedError); ok && scoped.IsRequestScoped() {
		t.Fatal("non-429 status was misclassified as request-scoped")
	}
}
