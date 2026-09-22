package executor

import "github.com/router-for-me/CLIProxyAPI/v7/internal/config"

func newAnthropicCompatibleTestExecutor(cfg *config.Config) *ClaudeExecutor {
	return NewAnthropicCompatibleExecutor(cfg).ClaudeExecutor
}
