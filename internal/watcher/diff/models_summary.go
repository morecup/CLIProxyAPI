package diff

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

type ClaudeModelsSummary struct {
	hash  string
	count int
}

// SummarizeClaudeModels hashes Claude model aliases for change detection.
func SummarizeClaudeModels(models []config.ClaudeModel) ClaudeModelsSummary {
	if len(models) == 0 {
		return ClaudeModelsSummary{}
	}
	keys := normalizeModelPairs(func(out func(key string)) {
		for _, model := range models {
			name := strings.TrimSpace(model.Name)
			alias := strings.TrimSpace(model.Alias)
			if name == "" && alias == "" {
				continue
			}
			isCompat := "false"
			if model.IsCompat {
				isCompat = "true"
			}
			out(strings.ToLower(name) + "|" + strings.ToLower(alias) + "|" + strings.TrimSpace(model.DisplayName) + "|is-compat=" + isCompat + thinkingHashSuffix(model.Thinking))
		}
	})
	return ClaudeModelsSummary{
		hash:  hashJoined(keys),
		count: len(keys),
	}
}

func thinkingHashSuffix(support *registry.ThinkingSupport) string {
	data, _ := json.Marshal(support)
	return "|thinking=" + string(data)
}
