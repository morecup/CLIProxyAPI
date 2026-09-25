package test

import (
	"fmt"
	"testing"
	"time"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"

	// Import the Claude provider package to trigger init() registration of its ProviderApplier
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// thinkingTestCase represents a common test case structure for both suffix and body tests.
type thinkingTestCase struct {
	name            string
	from            string
	to              string
	model           string
	inputJSON       string
	expectField     string
	expectValue     string
	expectField2    string
	expectValue2    string
	expectField3    string
	expectValue3    string
	expectAbsent    []string
	includeThoughts string
	expectErr       bool
}

// TestThinkingE2EClaudeMatrix covers the surviving Claude-target thinking
// transformations: OpenAI/OpenAI-Responses/Claude sources into the Claude wire
// format. Data flow: Input JSON -> TranslateRequest -> ApplyThinking -> Validate.
func TestThinkingE2EClaudeMatrix(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	uid := fmt.Sprintf("thinking-e2e-claude-%d", time.Now().UnixNano())

	reg.RegisterClient(uid, "test", getTestModels())
	defer reg.UnregisterClient(uid)

	cases := []thinkingTestCase{}

	runThinkingTests(t, cases)
}

func getTestModels() []*registry.ModelInfo {
	return []*registry.ModelInfo{
		{
			ID:          "claude-budget-model",
			Object:      "model",
			Created:     1700000000,
			OwnedBy:     "test",
			Type:        "claude",
			DisplayName: "Claude Budget Model",
			Thinking:    &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-6-model",
			Object:              "model",
			Created:             1770318000, // 2026-02-05
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Opus",
			Description:         "Premium model combining maximum intelligence with practical performance",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false, Levels: []string{"low", "medium", "high", "max"}},
		},
		{
			ID:                  "claude-sonnet-4-6-model",
			Object:              "model",
			Created:             1771372800, // 2026-02-17
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false, Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:          "user-defined-model",
			Object:      "model",
			Created:     1700000000,
			OwnedBy:     "test",
			Type:        "openai",
			DisplayName: "User Defined Model",
			UserDefined: true,
			Thinking:    nil,
		},
	}
}

// runThinkingTests runs thinking test cases using the real data flow path.
func runThinkingTests(t *testing.T, cases []thinkingTestCase) {
	for _, tc := range cases {
		tc := tc
		testName := fmt.Sprintf("Case%s_%s->%s_%s", tc.name, tc.from, tc.to, tc.model)
		t.Run(testName, func(t *testing.T) {
			suffixResult := thinking.ParseSuffix(tc.model)
			baseModel := suffixResult.ModelName

			body := sdktranslator.TranslateRequest(
				sdktranslator.FromString(tc.from),
				sdktranslator.FromString(tc.to),
				baseModel,
				[]byte(tc.inputJSON),
				true,
			)
			if tc.to == "claude" {
				body, _ = sjson.SetBytes(body, "max_tokens", 200000)
			}

			body, err := thinking.ApplyThinking(body, tc.model, tc.from, tc.to, tc.to)

			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error but got none, body=%s", string(body))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v, body=%s", err, string(body))
			}

			for _, fieldPath := range tc.expectAbsent {
				if gjson.GetBytes(body, fieldPath).Exists() {
					t.Fatalf("expected field %s to be absent, body=%s", fieldPath, string(body))
				}
			}

			if tc.expectField == "" {
				if gjson.GetBytes(body, "thinking").Exists() {
					t.Fatalf("expected no thinking field but found one, body=%s", string(body))
				}
				return
			}

			assertField := func(fieldPath, expected string) {
				val := gjson.GetBytes(body, fieldPath)
				if !val.Exists() {
					t.Fatalf("expected field %s not found, body=%s", fieldPath, string(body))
				}
				actualValue := val.String()
				if val.Type == gjson.Number {
					actualValue = fmt.Sprintf("%d", val.Int())
				}
				if actualValue != expected {
					t.Fatalf("field %s: expected %q, got %q, body=%s", fieldPath, expected, actualValue, string(body))
				}
			}

			assertField(tc.expectField, tc.expectValue)
			if tc.expectField2 != "" {
				assertField(tc.expectField2, tc.expectValue2)
			}

			// Claude adaptive effort is only valid as a pair: native Claude Code
			// 2.1.220 always sends thinking.type="adaptive" alongside
			// output_config.effort. Emitting effort on its own would be a wire
			// shape the real client never produces.
			if tc.to == "claude" && gjson.GetBytes(body, "output_config.effort").Exists() {
				assertField("thinking.type", "adaptive")
			}
			if tc.expectField3 != "" {
				assertField(tc.expectField3, tc.expectValue3)
			}
		})
	}
}
