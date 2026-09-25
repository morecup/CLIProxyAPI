package translator

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRegistryTranslateRequestAppliesSummaryIntent(t *testing.T) {
	tests := []struct {
		name       string
		from       Format
		to         Format
		input      string
		translated string
		path       string
		want       string
		wantExists bool
	}{
		{
			name:       "chat effort enables Claude summary",
			from:       FormatOpenAI,
			to:         FormatClaude,
			input:      `{"reasoning_effort":"high"}`,
			translated: `{"thinking":{"type":"adaptive"}}`,
			path:       "thinking.display",
			want:       "summarized",
			wantExists: true,
		},
		{
			name:       "responses effort alone leaves Claude display absent",
			from:       FormatOpenAIResponse,
			to:         FormatClaude,
			input:      `{"reasoning":{"effort":"high"}}`,
			translated: `{"thinking":{"type":"adaptive"}}`,
			path:       "thinking.display",
		},
		{
			name:       "responses summary enables Claude summary",
			from:       FormatOpenAIResponse,
			to:         FormatClaude,
			input:      `{"reasoning":{"effort":"high","summary":"auto"}}`,
			translated: `{"thinking":{"type":"adaptive"}}`,
			path:       "thinking.display",
			want:       "summarized",
			wantExists: true,
		},
		{
			name:       "chat extension disables Claude summary",
			from:       FormatOpenAI,
			to:         FormatClaude,
			input:      `{"reasoning_effort":"high","thinking":{"include_thoughts":false}}`,
			translated: `{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`,
			path:       "thinking.display",
			want:       "omitted",
			wantExists: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			registry.Register(test.from, test.to, func(_ string, _ []byte, _ bool) []byte {
				return []byte(test.translated)
			}, ResponseTransform{})
			out := registry.TranslateRequest(test.from, test.to, "model", []byte(test.input), false)
			result := gjson.GetBytes(out, test.path)
			if result.Exists() != test.wantExists {
				t.Fatalf("%s exists = %v, want %v; body=%s", test.path, result.Exists(), test.wantExists, out)
			}
			if test.wantExists && result.String() != test.want {
				t.Fatalf("%s = %q, want %q; body=%s", test.path, result.String(), test.want, out)
			}
		})
	}
}

func TestRegistryTranslateRequestActivatesClaudeForEnabledSummary(t *testing.T) {
	registry := NewRegistry()
	registry.Register(FormatOpenAIResponse, FormatClaude, func(_ string, _ []byte, _ bool) []byte {
		return []byte(`{"model":"claude-opus-5","max_tokens":32000}`)
	}, ResponseTransform{})
	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatClaude,
		"claude-opus-5",
		[]byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`),
		false,
	)
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.display").String(); got != "summarized" {
		t.Fatalf("thinking.display = %q, want summarized; body=%s", got, out)
	}
}

func TestRegistryTranslateRequestDoesNotActivateClaudeForDisabledSummary(t *testing.T) {
	registry := NewRegistry()
	registry.Register(FormatOpenAIResponse, FormatClaude, func(_ string, _ []byte, _ bool) []byte {
		return []byte(`{"model":"claude-opus-5","max_tokens":32000}`)
	}, ResponseTransform{})
	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatClaude,
		"claude-opus-5",
		[]byte(`{"reasoning":{"summary":null},"input":"hi"}`),
		false,
	)
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("disabled summary activated Claude thinking: %s", out)
	}
}

func TestRegistryTranslateRequestPreservesNativeClaudeMissingDisplay(t *testing.T) {
	registry := NewRegistry()
	body := []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`)
	out := registry.TranslateRequest(FormatClaude, FormatClaude, "claude-opus-5", body, true)
	if gjson.GetBytes(out, "thinking.display").Exists() {
		t.Fatalf("native Claude request without display gained one: %s", out)
	}
}

func TestRegistryTranslateRequestDoesNotMixSummaryIntoFallback(t *testing.T) {
	registry := NewRegistry()
	body := []byte(`{"model":"claude-opus-5","reasoning":{"summary":"auto"},"input":"hi"}`)
	out := registry.TranslateRequest(FormatOpenAIResponse, Format("unknown"), "claude-opus-5", body, false)
	if !bytes.Equal(out, body) {
		t.Fatalf("missing translator changed fallback body: got %s, want %s", out, body)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("missing translator mixed Claude fields into Responses body: %s", out)
	}
}

func TestRegistryTranslateRequestPluginMissDoesNotMixSummary(t *testing.T) {
	registry := NewRegistry()
	hooks := &fakePluginHooks{requestTranslateOK: false}
	registry.SetPluginHooks(hooks)
	body := []byte(`{"model":"claude-opus-5","reasoning":{"summary":"auto"},"input":"hi"}`)
	out := registry.TranslateRequest(FormatOpenAIResponse, Format("unknown"), "claude-opus-5", body, false)
	if !bytes.Equal(out, body) {
		t.Fatalf("plugin translation miss changed fallback body: got %s, want %s", out, body)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("plugin translation miss mixed Claude fields into Responses body: %s", out)
	}
}

func TestRegistryTranslateRequestAppliesSummaryAfterPluginTranslation(t *testing.T) {
	registry := NewRegistry()
	hooks := &fakePluginHooks{
		requestTranslateBody: []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`),
		requestTranslateOK:   true,
	}
	registry.SetPluginHooks(hooks)
	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatClaude,
		"claude-opus-5",
		[]byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`),
		false,
	)
	if got := gjson.GetBytes(out, "thinking.display").String(); got != "summarized" {
		t.Fatalf("plugin-translated request lost canonical summary: %s", out)
	}
}

func TestRegistryTranslateRequestPluginNormalizerOwnsSourceSummaryIntent(t *testing.T) {
	tests := []struct {
		name       string
		normalize  func([]byte) []byte
		wantExists bool
		want       bool
	}{
		{
			name: "removed summary remains absent",
			normalize: func(body []byte) []byte {
				out, _ := sjson.DeleteBytes(body, "reasoning.summary")
				return out
			},
		},
		{
			name: "disabled summary replaces enabled intent",
			normalize: func(body []byte) []byte {
				out, _ := sjson.SetBytes(body, "reasoning.summary", nil)
				return out
			},
			wantExists: true,
			want:       false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			hooks := &fakePluginHooks{
				normalizeRequest:     test.normalize,
				requestTranslateBody: []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`),
				requestTranslateOK:   true,
			}
			registry.SetPluginHooks(hooks)

			out := registry.TranslateRequest(
				FormatOpenAIResponse,
				FormatClaude,
				"claude-opus-5",
				[]byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`),
				false,
			)
			result := gjson.GetBytes(out, "thinking.display")
			if result.Exists() != test.wantExists {
				t.Fatalf("thinking.display exists = %v, want %v; body=%s", result.Exists(), test.wantExists, out)
			}
			if test.wantExists && result.String() == "omitted" != !test.want {
				t.Fatalf("thinking.display = %v, want %v; body=%s", result.String(), test.want, out)
			}
		})
	}
}

func TestRegistryTranslateRequestNormalizerOwnsFinalSummaryField(t *testing.T) {
	registry := NewRegistry()
	registry.Register(FormatOpenAIResponse, FormatClaude, func(_ string, _ []byte, _ bool) []byte {
		return []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"}}`)
	}, ResponseTransform{})
	hooks := &fakePluginHooks{normalizeRequest: func(body []byte) []byte {
		if got := gjson.GetBytes(body, "thinking.display").String(); got != "summarized" {
			t.Fatalf("normalizer did not receive canonical enabled summary: %s", body)
		}
		out, _ := sjson.DeleteBytes(body, "thinking.display")
		return out
	}}
	registry.SetPluginHooks(hooks)

	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatClaude,
		"claude-opus-5",
		[]byte(`{"reasoning":{"effort":"high","summary":"auto"},"input":"hi"}`),
		false,
	)
	if gjson.GetBytes(out, "thinking.display").Exists() {
		t.Fatalf("summary post-processing overrode request normalizer: %s", out)
	}
}
