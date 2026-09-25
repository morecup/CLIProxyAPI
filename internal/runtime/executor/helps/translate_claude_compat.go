package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	openaichatclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/chat-completions"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/responses"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TranslateRequestWithAPIKeyModelCompatibility translates an inbound request into the
// target provider format. For API-key models marked is-compat, assistant reasoning
// content with empty signatures is preserved for compatible upstreams.
// Only Claude upstream targets remain in this build.
func TranslateRequestWithAPIKeyModelCompatibility(from, to sdktranslator.Format, model string, payload []byte, stream, isCompat bool) []byte {
	if isCompat {
		var translated []byte
		switch {
		case from == sdktranslator.FormatOpenAI && to == sdktranslator.FormatClaude:
			translated = openaichatclaude.ConvertOpenAIRequestToClaudeWithCompat(model, payload, stream)
		case from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude:
			translated = responsesclaude.ConvertOpenAIResponsesRequestToClaudeWithCompat(model, payload, stream)
		}
		if translated != nil {
			summaryConfig := thinking.ExtractSummaryConfig(payload, from.String())
			return thinking.ApplySummaryConfigForModel(translated, to.String(), model, summaryConfig)
		}
	}
	return sdktranslator.TranslateRequest(from, to, model, payload, stream)
}
