package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// AnthropicCompatibleExecutor serves generic Anthropic Messages API keys and
// custom gateways. It intentionally shares protocol mechanics with the Claude
// executor while remaining outside the Claude Desktop runtime boundary.
type AnthropicCompatibleExecutor struct {
	*ClaudeExecutor
}

func NewAnthropicCompatibleExecutor(cfg *config.Config) *AnthropicCompatibleExecutor {
	return &AnthropicCompatibleExecutor{ClaudeExecutor: &ClaudeExecutor{
		cfg:        cfg,
		providerID: "anthropic-compatible",
	}}
}

// RequestToFormat keeps the generic provider on the Anthropic Messages wire
// format even though its provider identifier is no longer "claude".
func (e *ClaudeExecutor) RequestToFormat(cliproxyexecutor.Request, cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatClaude
}
