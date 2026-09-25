package thinking

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractSummaryConfig(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		body       string
		wantMode   SummaryMode
		wantDetail string
	}{
		{name: "chat effort enables", format: "openai", body: `{"reasoning_effort":"high"}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "chat none disables", format: "openai", body: `{"reasoning_effort":"none"}`, wantMode: SummaryDisabled},
		{name: "chat missing unspecified", format: "openai", body: `{}`, wantMode: SummaryUnspecified},
		{name: "chat null effort unspecified", format: "openai", body: `{"reasoning_effort":null}`, wantMode: SummaryUnspecified},
		{name: "chat non-string effort unspecified", format: "openai", body: `{"reasoning_effort":17}`, wantMode: SummaryUnspecified},
		{name: "chat include_thoughts false overrides effort", format: "openai", body: `{"reasoning_effort":"high","thinking":{"include_thoughts":false}}`, wantMode: SummaryDisabled},
		{name: "chat include_thoughts true", format: "openai", body: `{"thinking":{"include_thoughts":true}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "chat reasoning summary auto", format: "openai", body: `{"reasoning":{"summary":"auto"}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "responses effort alone unspecified", format: "openai-response", body: `{"reasoning":{"effort":"high"}}`, wantMode: SummaryUnspecified},
		{name: "responses summary auto", format: "openai-response", body: `{"reasoning":{"effort":"high","summary":"auto"}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "responses summary concise", format: "openai-response", body: `{"reasoning":{"summary":"concise"}}`, wantMode: SummaryEnabled, wantDetail: "concise"},
		{name: "responses summary null", format: "openai-response", body: `{"reasoning":{"summary":null}}`, wantMode: SummaryDisabled},
		{name: "responses boolean summary invalid", format: "openai-response", body: `{"reasoning":{"summary":true}}`, wantMode: SummaryUnspecified},
		{name: "responses deprecated generate summary", format: "openai-response", body: `{"reasoning":{"generate_summary":"detailed"}}`, wantMode: SummaryEnabled, wantDetail: "detailed"},
		{name: "claude summarized", format: "claude", body: `{"thinking":{"type":"adaptive","display":"summarized"}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "claude omitted", format: "claude", body: `{"thinking":{"type":"enabled","budget_tokens":2048,"display":"omitted"}}`, wantMode: SummaryDisabled},
		{name: "claude display without type is invalid", format: "claude", body: `{"thinking":{"display":"summarized"}}`, wantMode: SummaryUnspecified},
		{name: "claude display with auto type is invalid", format: "claude", body: `{"thinking":{"type":"auto","display":"summarized"}}`, wantMode: SummaryUnspecified},
		// ApplySummaryConfig runs before ApplyThinking fills budget_tokens, so an
		// absent budget must not be read as inactive thinking.
		{name: "claude enabled display without budget is valid", format: "claude", body: `{"thinking":{"type":"enabled","display":"summarized"}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "claude enabled display with zero budget is invalid", format: "claude", body: `{"thinking":{"type":"enabled","budget_tokens":0,"display":"summarized"}}`, wantMode: SummaryUnspecified},
		{name: "claude auto compatibility budget summarized", format: "claude", body: `{"thinking":{"type":"enabled","budget_tokens":-1,"display":"summarized"}}`, wantMode: SummaryEnabled, wantDetail: "auto"},
		{name: "claude auto compatibility budget omitted", format: "claude", body: `{"thinking":{"type":"enabled","budget_tokens":-1,"display":"omitted"}}`, wantMode: SummaryDisabled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ExtractSummaryConfig([]byte(test.body), test.format)
			if got.Mode != test.wantMode || got.Detail != test.wantDetail {
				t.Fatalf("ExtractSummaryConfig() = %+v, want mode=%v detail=%q", got, test.wantMode, test.wantDetail)
			}
		})
	}
}

func TestExtractExplicitSummaryConfigDoesNotUseChatEffort(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high"}`)
	if got := ExtractExplicitSummaryConfig(body, "openai"); got.Mode != SummaryUnspecified {
		t.Fatalf("ExtractExplicitSummaryConfig() = %+v, want unspecified", got)
	}

	body = []byte(`{"reasoning_effort":"high","thinking":{"include_thoughts":false}}`)
	if got := ExtractExplicitSummaryConfig(body, "openai"); got.Mode != SummaryDisabled {
		t.Fatalf("ExtractExplicitSummaryConfig() = %+v, want disabled", got)
	}
}

func TestApplySummaryConfig(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
		config SummaryConfig
		path   string
		want   string
	}{
		{name: "claude enabled", format: "claude", body: `{"thinking":{"type":"adaptive"}}`, config: SummaryConfig{Mode: SummaryEnabled}, path: "thinking.display", want: "summarized"},
		{name: "claude disabled", format: "claude", body: `{"thinking":{"type":"enabled","budget_tokens":2048}}`, config: SummaryConfig{Mode: SummaryDisabled}, path: "thinking.display", want: "omitted"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			if body == "" {
				body = `{}`
			}
			out := ApplySummaryConfig([]byte(body), test.format, test.config)
			if got := gjson.GetBytes(out, test.path).String(); got != test.want {
				t.Fatalf("%s = %q, want %q; body=%s", test.path, got, test.want, out)
			}
		})
	}
}

// Anthropic requires thinking.type, and rejects display on a disabled block, so
// display must never be written unless thinking is already active.
func TestApplySummaryConfig_ClaudeDisplayRequiresActiveThinking(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"thinking":{"type":"disabled"}}`,
	}
	for _, mode := range []SummaryMode{SummaryEnabled, SummaryDisabled} {
		for _, body := range bodies {
			out := ApplySummaryConfig([]byte(body), "claude", SummaryConfig{Mode: mode})
			if gjson.GetBytes(out, "thinking.display").Exists() {
				t.Fatalf("mode %v wrote display without active thinking: %s", mode, out)
			}
			if !bytes.Equal(out, []byte(body)) {
				t.Fatalf("mode %v changed body: got %s, want %s", mode, out, body)
			}
		}
	}
}

func TestApplySummaryConfigForModel_ClaudeEnabledSummaryUsesValidThinkingMode(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		body       string
		wantType   string
		wantBudget int64
	}{
		{name: "adaptive model", model: "claude-opus-5", body: `{"model":"claude-opus-5","max_tokens":32000}`, wantType: "adaptive"},
		{name: "manual model", model: "claude-haiku-4-5-20251001", body: `{"model":"claude-haiku-4-5-20251001","max_tokens":32000}`, wantType: "enabled", wantBudget: 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := ApplySummaryConfigForModel([]byte(test.body), "claude", test.model, SummaryConfig{Mode: SummaryEnabled})
			if got := gjson.GetBytes(out, "thinking.type").String(); got != test.wantType {
				t.Fatalf("thinking.type = %q, want %q; body=%s", got, test.wantType, out)
			}
			if got := gjson.GetBytes(out, "thinking.display").String(); got != "summarized" {
				t.Fatalf("thinking.display = %q, want summarized; body=%s", got, out)
			}
			if test.wantBudget > 0 && gjson.GetBytes(out, "thinking.budget_tokens").Int() != test.wantBudget {
				t.Fatalf("thinking.budget_tokens = %d, want %d; body=%s", gjson.GetBytes(out, "thinking.budget_tokens").Int(), test.wantBudget, out)
			}
		})
	}
}

// Disabling summaries must not make CPA add a Claude thinking block. Absence
// preserves the per-model default: newer models may still think by default,
// while older models remain off.
func TestApplySummaryConfigForModel_ClaudeDisabledSummaryDoesNotEnableThinking(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-haiku-4-5-20251001"} {
		body := []byte(`{"model":"` + model + `","max_tokens":32000}`)
		out := ApplySummaryConfigForModel(body, "claude", model, SummaryConfig{Mode: SummaryDisabled})
		if gjson.GetBytes(out, "thinking").Exists() {
			t.Fatalf("model %s gained thinking for a disabled summary: %s", model, out)
		}
	}
}

func TestApplySummaryConfig_UnspecifiedLeavesBodyUnchanged(t *testing.T) {
	body := []byte(`{"thinking":{"type":"adaptive"}}`)
	if got := ApplySummaryConfig(body, "claude", SummaryConfig{}); !bytes.Equal(got, body) {
		t.Fatalf("unspecified summary changed body: got %s, want %s", got, body)
	}
}
