package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
	"github.com/tidwall/gjson"
)

func opus55ModelInfo() *registry.ModelInfo {
	return &registry.ModelInfo{
		ID:                  "claude-opus-5-5",
		Type:                "claude",
		MaxCompletionTokens: 128000,
		Thinking: &registry.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         []string{"low", "medium", "high", "xhigh", "max"},
		},
	}
}

func TestOpus55ThinkingUsesAdaptiveOnly(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		body       string
		wantEffort string
	}{
		{name: "enabled without budget becomes adaptive auto", model: "claude-opus-5-5", body: `{"thinking":{"type":"enabled"}}`},
		{name: "disabled becomes low", model: "claude-opus-5-5", body: `{"thinking":{"type":"disabled"}}`, wantEffort: "low"},
		{name: "zero budget becomes low", model: "claude-opus-5-5", body: `{"thinking":{"type":"enabled","budget_tokens":0}}`, wantEffort: "low"},
		{name: "none suffix becomes low", model: "claude-opus-5-5(none)", body: `{}`, wantEffort: "low"},
		{name: "auto suffix keeps upstream default", model: "claude-opus-5-5(auto)", body: `{}`},
		{name: "positive budget becomes effort", model: "claude-opus-5-5", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, wantEffort: "medium"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := thinking.ApplyThinkingWithModelInfo(
				[]byte(test.body), []byte(test.body), test.model,
				"claude", "claude", "claude", opus55ModelInfo(),
			)
			if err != nil {
				t.Fatalf("ApplyThinkingWithModelInfo() error = %v", err)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
				t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, out)
			}
			if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
				t.Fatalf("adaptive-only model retained budget_tokens: %s", out)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != test.wantEffort {
				t.Fatalf("output_config.effort = %q, want %q; body=%s", got, test.wantEffort, out)
			}
		})
	}
}

func TestOpus55ThinkingPreservesEverySupportedEffort(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort + `"}}`)
			out, err := thinking.ApplyThinkingWithModelInfo(body, body, "claude-opus-5-5", "claude", "claude", "claude", opus55ModelInfo())
			if err != nil {
				t.Fatalf("ApplyThinkingWithModelInfo() error = %v", err)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
				t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, out)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != effort {
				t.Fatalf("output_config.effort = %q, want %q; body=%s", got, effort, out)
			}
		})
	}
}
