package modelconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// ComputeClaudeModelsHash returns a stable hash for Claude model aliases.
func ComputeClaudeModelsHash(models []config.ClaudeModel) string {
	keys := modelRoutingKeys(func(out func(key string)) {
		for _, model := range models {
			name := strings.TrimSpace(model.Name)
			alias := strings.TrimSpace(model.Alias)
			if name == "" && alias == "" {
				continue
			}
			out(strings.ToLower(name) + "|" + strings.ToLower(alias) + "|" + strings.TrimSpace(model.DisplayName) + "|" + fmt.Sprintf("force-mapping=%t", model.ForceMapping) + "|" + fmt.Sprintf("is-compat=%t", model.IsCompat) + thinkingHashSuffix(model.Thinking))
		}
	})
	return hashJoined(keys)
}

func thinkingHashSuffix(support *registry.ThinkingSupport) string {
	data, _ := json.Marshal(support)
	return "|thinking=" + string(data)
}

func modelRoutingKeys(collect func(out func(key string))) []string {
	keys := make([]string, 0)
	collect(func(key string) {
		keys = append(keys, key)
	})
	return keys
}

func hashJoined(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}
