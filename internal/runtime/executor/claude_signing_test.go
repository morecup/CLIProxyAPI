package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const claudeDesktopCCHBaseBody = `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=fixture; cc_entrypoint=claude-desktop; cch=00000;"},{"type":"text","text":"system-x"}],"tools":[],"metadata":{"user_id":"meta-x"},"max_tokens":1,"thinking":{"type":"adaptive","display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"high"},"stream":true}`

func TestSignAnthropicMessagesBody_DesktopNormalizationVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "base", body: claudeDesktopCCHBaseBody, want: "7d961"},
		{name: "model value ignored", body: strings.Replace(claudeDesktopCCHBaseBody, `"model":"model-a"`, `"model":"model-b"`, 1), want: "7d961"},
		{name: "max tokens ignored", body: strings.Replace(claudeDesktopCCHBaseBody, `"max_tokens":1`, `"max_tokens":2`, 1), want: "7d961"},
		{name: "message changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"text":"x"`, `"text":"y"`, 1), want: "0eb4e"},
		{name: "system changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"system-x"`, `"system-y"`, 1), want: "20095"},
		{name: "metadata changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"user_id":"meta-x"`, `"user_id":"meta-y"`, 1), want: "24de7"},
		{name: "thinking changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"thinking":{"type":"adaptive","display":"omitted"}`, `"thinking":{"type":"disabled"}`, 1), want: "52951"},
		{name: "context changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`, `"context_management":{"edits":[]}`, 1), want: "8c5d5"},
		{name: "effort changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"effort":"high"`, `"effort":"low"`, 1), want: "97ac4"},
		{name: "stream changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"stream":true`, `"stream":false`, 1), want: "1f735"},
		{name: "tool changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"tools":[]`, `"tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}]`, 1), want: "9ff92"},
		{name: "extra field changes hash", body: strings.Replace(claudeDesktopCCHBaseBody, `"stream":true}`, `"stream":true,"extra_top":"extra"}`, 1), want: "7f9b1"},
		{
			name: "field order remains significant",
			body: `{"stream":true,"output_config":{"effort":"high"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"thinking":{"type":"adaptive","display":"omitted"},"max_tokens":1,"metadata":{"user_id":"meta-x"},"tools":[],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=fixture; cc_entrypoint=claude-desktop; cch=00000;"},{"type":"text","text":"system-x"}],"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"model":"model-a"}`,
			want: "32ebe",
		},
		{name: "nested model value ignored", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","model":"a"}`, 1), want: "e5aad"},
		{name: "nested max tokens member omitted", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":2}`, 1), want: "7d961"},
		{name: "top level fallbacks member omitted", body: strings.Replace(claudeDesktopCCHBaseBody, `"stream":true}`, `"stream":true,"fallbacks":[{"model":"fallback-a"}]}`, 1), want: "7d961"},
		{name: "nested fallbacks member omitted", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","fallbacks":[{"model":"nested-a"}]}`, 1), want: "7d961"},
		{name: "top level fallback credit token omitted", body: strings.Replace(claudeDesktopCCHBaseBody, `"stream":true}`, `"stream":true,"fallback_credit_token":"a"}`, 1), want: "7d961"},
		{name: "nested fallback credit token omitted", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","fallback_credit_token":"a"}`, 1), want: "7d961"},
		{name: "trailing dispatch run keeps Desktop comma", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":999,"fallbacks":[{"model":"fallback-model"}]}`, 1), want: "fa5aa"},
		{name: "model before trailing dispatch run", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","model":"nested-model","max_tokens":999,"fallbacks":[{"model":"fallback-model"}],"fallback_credit_token":"not-a-real-token"}`, 1), want: "79e55"},
		{name: "model splits dispatch runs", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":999,"model":"nested-model","fallbacks":[{"model":"fallback-model"}]}`, 1), want: "e5aad"},
		{name: "ordinary nested member remains", body: strings.Replace(claudeDesktopCCHBaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","plain":"a"}`, 1), want: "b19e9"},
		{name: "billing block only", body: `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=fixture; cc_entrypoint=claude-desktop; cch=00000;"}]}`, want: "d5a5e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signed, err := signAnthropicMessagesBody([]byte(tt.body))
			if err != nil {
				t.Fatalf("signAnthropicMessagesBody() error = %v", err)
			}
			if got := claudeCCHFromBody(t, signed); got != tt.want {
				t.Fatalf("cch = %q, want %q\nbody: %s", got, tt.want, signed)
			}
		})
	}
}

func TestSignAnthropicMessagesBody_PreservesFinalSerializedBytes(t *testing.T) {
	t.Parallel()

	literal := "keep literal cch=00000; in the message"
	body := []byte(strings.Replace(claudeDesktopCCHBaseBody, `"text":"x"`, `"text":"`+literal+`"`, 1))
	signed, err := signAnthropicMessagesBody(body)
	if err != nil {
		t.Fatalf("signAnthropicMessagesBody() error = %v", err)
	}
	if got := gjson.GetBytes(signed, "messages.0.content.0.text").String(); got != literal {
		t.Fatalf("message text = %q, want %q", got, literal)
	}

	cchOffset, ok := claudeBillingCCHDigitsOffset(signed)
	if !ok {
		t.Fatal("signed billing CCH not found")
	}
	unsigned := bytes.Clone(signed)
	copy(unsigned[cchOffset:cchOffset+claudeCCHLength], "00000")
	if !bytes.Equal(unsigned, body) {
		t.Fatalf("signing changed bytes outside CCH\n got: %s\nwant: %s", unsigned, body)
	}
}

func TestFinalizeAnthropicMessagesBodyCCHDoesNotInventDesktopFields(t *testing.T) {
	t.Parallel()

	for _, body := range [][]byte{
		[]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=fixture; cc_entrypoint=claude-desktop;"}]}`),
	} {
		signed, err := finalizeAnthropicMessagesBodyCCH(body)
		if err != nil {
			t.Fatalf("finalizeAnthropicMessagesBodyCCH() error = %v", err)
		}
		if !bytes.Equal(signed, body) {
			t.Fatalf("finalizer invented Desktop fields: got %s, want %s", signed, body)
		}
	}
}

func TestClaudeDesktopCCHSigningEnabled(t *testing.T) {
	t.Parallel()

	const anthropicOrigin = "https://api.anthropic.com/v1/messages?beta=true"
	tests := []struct {
		name   string
		token  string
		origin string
		want   bool
	}{
		{name: "Desktop OAuth on first-party HTTPS", token: "sk-ant-oat-test", origin: anthropicOrigin, want: true},
		{name: "Desktop OAuth on explicit first-party port", token: "sk-ant-oat-test", origin: "https://api.anthropic.com:443/v1/messages", want: true},
		{name: "Desktop OAuth over HTTP", token: "sk-ant-oat-test", origin: "http://api.anthropic.com/v1/messages"},
		{name: "Desktop OAuth on custom gateway", token: "sk-ant-oat-test", origin: "https://gateway.example/v1/messages"},
		{name: "API key on first-party HTTPS", token: "key-test", origin: anthropicOrigin},
		{name: "missing token", origin: anthropicOrigin},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := claudeDesktopCCHSigningEnabled(test.token, test.origin); got != test.want {
				t.Fatalf("claudeDesktopCCHSigningEnabled() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestNormalizeClaudeCCHInput_PreservesRawJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "model string becomes empty", body: `{"model":"claude","keep":1}`, want: `{"model":"","keep":1}`},
		{name: "excluded first member", body: `{"max_tokens":1,"keep":2}`, want: `{"keep":2}`},
		{name: "excluded middle member", body: `{"keep":1,"fallbacks":[{"model":"x"}],"tail":2}`, want: `{"keep":1,"tail":2}`},
		{name: "excluded last member", body: `{"keep":1,"fallback_credit_token":"secret"}`, want: `{"keep":1}`},
		{name: "all members excluded", body: `{"max_tokens":1,"fallbacks":[],"fallback_credit_token":"secret"}`, want: `{}`},
		{name: "adjacent excluded members", body: `{"keep":1,"max_tokens":1,"fallbacks":[],"tail":2}`, want: `{"keep":1,"tail":2}`},
		{name: "Desktop trailing dispatch run", body: `{"keep":1,"max_tokens":1,"fallbacks":[]}`, want: `{"keep":1,}`},
		{name: "nested fields", body: `{"outer":{"model":"x","max_tokens":1,"keep":"y"}}`, want: `{"outer":{"model":"","keep":"y"}}`},
		{name: "escaped key text stays inside string", body: `{"text":"literal \"model\":\"x\" and \"max_tokens\":1"}`, want: `{"text":"literal \"model\":\"x\" and \"max_tokens\":1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeClaudeCCHInput([]byte(tt.body))
			if err != nil {
				t.Fatalf("normalizeClaudeCCHInput() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("normalized body = %s, want %s", got, tt.want)
			}
		})
	}
}

func claudeCCHFromBody(t *testing.T, body []byte) string {
	t.Helper()

	offset, ok := claudeBillingCCHDigitsOffset(body)
	if !ok {
		t.Fatalf("billing CCH not found in body: %s", body)
	}
	return string(body[offset : offset+claudeCCHLength])
}
