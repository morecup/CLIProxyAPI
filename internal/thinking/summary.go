package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SummaryMode represents whether the client explicitly requested reasoning summaries.
type SummaryMode int

const (
	SummaryUnspecified SummaryMode = iota
	SummaryDisabled
	SummaryEnabled
)

// SummaryConfig is the provider-neutral reasoning-summary visibility intent.
// Detail preserves protocols that distinguish auto, concise, and detailed summaries.
type SummaryConfig struct {
	Mode   SummaryMode
	Detail string
}

// ExtractSummaryConfig reads protocol-specific summary visibility intent.
//
// OpenAI Chat is the one protocol where effort implies summaries: chat
// completions has no summary field of its own, and clients that send
// reasoning_effort have always received reasoning summaries here, so treating a
// non-none effort as an explicit request preserves that contract. Every other
// protocol carries a dedicated summary field, so effort alone means nothing.
func ExtractSummaryConfig(body []byte, format string) SummaryConfig {
	normalized := strings.ToLower(strings.TrimSpace(format))
	// Check the format first so unsupported targets skip whole-body validation.
	if !summaryFormatSupported(normalized) || len(body) == 0 || !gjson.ValidBytes(body) {
		return SummaryConfig{}
	}

	switch normalized {
	case "openai":
		if config, ok := extractOpenAIExplicitSummaryConfig(body); ok {
			return config
		}
		if effort := gjson.GetBytes(body, "reasoning_effort"); effort.Type == gjson.String {
			value := strings.ToLower(strings.TrimSpace(effort.String()))
			if value == "" {
				return SummaryConfig{}
			}
			if value == "none" {
				return SummaryConfig{Mode: SummaryDisabled}
			}
			return SummaryConfig{Mode: SummaryEnabled, Detail: "auto"}
		}
	case "openai-response":
		if config, ok := responsesSummaryConfig(body, "reasoning.summary"); ok {
			return config
		}
		if config, ok := responsesSummaryConfig(body, "reasoning.generate_summary"); ok {
			return config
		}
	case "claude":
		// Anthropic only accepts display alongside active adaptive/manual thinking.
		if !claudeThinkingAcceptsDisplay(body) {
			return SummaryConfig{}
		}
		if config, ok := claudeSummaryConfig(body, "thinking.display"); ok {
			return config
		}
	}

	return SummaryConfig{}
}

// ExtractExplicitSummaryConfig reads only explicit visibility controls from a
// provider payload. Unlike ExtractSummaryConfig, OpenAI Chat reasoning_effort
// is not treated as a summary proxy. This lets executor post-processing tell
// whether a request normalizer retained or removed the translated target field.
func ExtractExplicitSummaryConfig(body []byte, format string) SummaryConfig {
	normalized := strings.ToLower(strings.TrimSpace(format))
	if normalized != "openai" {
		return ExtractSummaryConfig(body, normalized)
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return SummaryConfig{}
	}
	config, _ := extractOpenAIExplicitSummaryConfig(body)
	return config
}

// ApplySummaryConfig writes canonical summary intent in the target protocol.
func ApplySummaryConfig(body []byte, format string, config SummaryConfig) []byte {
	return ApplySummaryConfigForModel(body, format, "", config)
}

// ApplySummaryConfigForModel writes canonical summary intent in the target
// protocol and uses target model capabilities when a valid target request must
// activate thinking before it can request summaries.
func ApplySummaryConfigForModel(body []byte, format, model string, config SummaryConfig) []byte {
	return applySummaryConfigForModel(body, format, model, nil, config)
}

// applySummaryConfigForModel uses the resolved model definition when execution
// selected a configured API-key model whose capability is not globally visible.
func applySummaryConfigForModel(body []byte, format, model string, modelInfo *registry.ModelInfo, config SummaryConfig) []byte {
	return applySummaryConfigForProvider(body, format, model, "", modelInfo, config)
}

// applySummaryConfigForProvider writes canonical summary intent into the
// Claude upstream request body.
func applySummaryConfigForProvider(body []byte, format, model, provider string, modelInfo *registry.ModelInfo, config SummaryConfig) []byte {
	normalized := strings.ToLower(strings.TrimSpace(format))
	if config.Mode == SummaryUnspecified || !summaryFormatSupported(normalized) || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	enabled := config.Mode == SummaryEnabled
	switch normalized {
	case "claude":
		// Anthropic documents display as invalid with thinking.type=disabled and
		// requires it alongside adaptive or enabled thinking. Model defaults differ:
		// Opus 5 and Sonnet 5 default to adaptive thinking; Fable/Mythos 5 are always
		// on. Opus 4.8/4.7/4.6, Sonnet 4.6, and the 4.5 models default to thinking
		// off. The newest models also default display to omitted. Keeping a missing
		// thinking block absent therefore preserves both kinds of model default;
		// absence does not mean every Claude model runs without thinking. Only an
		// enabled summary may activate a valid target thinking mode so that summarized
		// text can be returned. A disabled summary only adds omitted to an
		// already-active target mode.
		//
		// Anthropic docs:
		// https://platform.claude.com/docs/en/build-with-claude/thinking
		// https://platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting#supported-models
		if enabled && !gjson.GetBytes(body, "thinking.type").Exists() {
			body = enableClaudeThinkingForSummary(body, model, modelInfo)
		}
		if !claudeThinkingAcceptsDisplay(body) {
			return body
		}
		value := "omitted"
		if enabled {
			value = "summarized"
		}
		body, _ = sjson.SetBytes(body, "thinking.display", value)
	}
	return body
}

// summaryFormatSupported reports whether a protocol carries summary visibility
// intent that this package can read or write.
func summaryFormatSupported(format string) bool {
	switch format {
	case "openai", "openai-response", "claude":
		return true
	default:
		return false
	}
}

// claudeThinkingAcceptsDisplay reports whether the body carries an active
// thinking block that can hold a display field.
func claudeThinkingAcceptsDisplay(body []byte) bool {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "adaptive":
		return true
	case "enabled":
		// This runs before ApplyThinking normalizes the request, so a missing
		// budget_tokens is an unfinished body rather than inactive thinking. CPA
		// also accepts -1 as its compatibility representation for auto thinking.
		budget := gjson.GetBytes(body, "thinking.budget_tokens")
		if budget.Type != gjson.Number {
			return true
		}
		value := budget.Int()
		return value == -1 || value > 0
	default:
		return false
	}
}

func extractOpenAIExplicitSummaryConfig(body []byte) (SummaryConfig, bool) {
	// Clients may carry the visibility control through compatibility aliases
	// rather than a dedicated Chat field; accept the documented forms before
	// the reasoning_effort fallback.
	for _, path := range []string{
		"thinking.includeThoughts",
		"thinking.include_thoughts",
		"reasoning.includeThoughts",
		"reasoning.include_thoughts",
	} {
		if config, ok := summaryBoolConfig(body, path); ok {
			return config, true
		}
	}

	for _, path := range []string{
		"reasoning.summary",
		"reasoning.generate_summary",
	} {
		if config, ok := responsesSummaryConfig(body, path); ok {
			return config, true
		}
	}

	return SummaryConfig{}, false
}

func summaryBoolConfig(body []byte, path string) (SummaryConfig, bool) {
	switch value := gjson.GetBytes(body, path); value.Type {
	case gjson.True:
		return SummaryConfig{Mode: SummaryEnabled, Detail: "auto"}, true
	case gjson.False:
		return SummaryConfig{Mode: SummaryDisabled}, true
	default:
		return SummaryConfig{}, false
	}
}

func responsesSummaryConfig(body []byte, path string) (SummaryConfig, bool) {
	value := gjson.GetBytes(body, path)
	if value.Raw == "" {
		return SummaryConfig{}, false
	}
	if value.Type == gjson.Null {
		return SummaryConfig{Mode: SummaryDisabled}, true
	}
	if value.Type != gjson.String {
		return SummaryConfig{}, false
	}

	raw := strings.ToLower(strings.TrimSpace(value.String()))
	switch raw {
	case "auto", "concise", "detailed":
		return SummaryConfig{Mode: SummaryEnabled, Detail: raw}, true
	case "none":
		// Compatibility with clients that expose a none enum; the OpenAI wire
		// representation disables summaries by omitting the field.
		return SummaryConfig{Mode: SummaryDisabled}, true
	default:
		return SummaryConfig{}, false
	}
}

func claudeSummaryConfig(body []byte, path string) (SummaryConfig, bool) {
	value := gjson.GetBytes(body, path)
	if value.Type != gjson.String {
		return SummaryConfig{}, false
	}
	switch strings.ToLower(strings.TrimSpace(value.String())) {
	case "summarized":
		return SummaryConfig{Mode: SummaryEnabled, Detail: "auto"}, true
	case "omitted":
		return SummaryConfig{Mode: SummaryDisabled}, true
	default:
		return SummaryConfig{}, false
	}
}

// stripInferredClaudeSummaryActivation removes a globally inferred adaptive
// mode when the selected API-key model supports only manual extended thinking.
// The exact model-aware summary pass can then activate enabled thinking with a
// valid budget, or leave thinking absent when max_tokens cannot accommodate it.
func stripInferredClaudeSummaryActivation(body []byte, modelInfo *registry.ModelInfo) []byte {
	if modelInfo == nil || modelInfo.Thinking == nil || len(modelInfo.Thinking.Levels) > 0 || modelInfo.Thinking.Min <= 0 {
		return body
	}
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()), "adaptive") {
		return body
	}

	for _, path := range []string{
		"thinking.type",
		"thinking.budget_tokens",
		"thinking.display",
		"output_config.effort",
	} {
		body, _ = sjson.DeleteBytes(body, path)
	}
	for _, path := range []string{"thinking", "output_config"} {
		if object := gjson.GetBytes(body, path); object.Exists() && object.IsObject() && len(object.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, path)
		}
	}
	return body
}

func enableClaudeThinkingForSummary(body []byte, model string, resolvedModelInfo *registry.ModelInfo) []byte {
	modelInfo := resolvedModelInfo
	if modelInfo == nil {
		baseModel := ParseSuffix(model).ModelName
		if baseModel == "" {
			baseModel = ParseSuffix(gjson.GetBytes(body, "model").String()).ModelName
		}
		modelInfo = registry.LookupModelInfo(baseModel, "claude")
	}
	if modelInfo == nil || modelInfo.Thinking == nil {
		return body
	}

	if len(modelInfo.Thinking.Levels) > 0 {
		body, _ = sjson.SetBytes(body, "thinking.type", "adaptive")
		body, _ = sjson.DeleteBytes(body, "thinking.budget_tokens")
		return body
	}

	budget := modelInfo.Thinking.Min
	if budget <= 0 {
		return body
	}
	if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() && maxTokens.Int() <= int64(budget) {
		return body
	}
	body, _ = sjson.SetBytes(body, "thinking.type", "enabled")
	body, _ = sjson.SetBytes(body, "thinking.budget_tokens", budget)
	return body
}

func normalizedSummaryDetail(detail string) string {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "concise":
		return "concise"
	case "detailed":
		return "detailed"
	default:
		return "auto"
	}
}
