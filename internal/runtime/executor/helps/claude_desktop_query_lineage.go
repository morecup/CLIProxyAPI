package helps

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/google/uuid"
)

const (
	ClaudeDesktopQueryChainIDMetadataKey = "claude_desktop_query_chain_id"
	ClaudeDesktopQueryDepthMetadataKey   = "claude_desktop_query_depth"
)

// ClaudeDesktopQueryLineage reads an explicit agent-loop position. Both fields
// must belong to the same metadata source; unrelated caller/translator maps
// must not accidentally construct a parent-child relationship.
func ClaudeDesktopQueryLineage(metadata ...map[string]any) (string, *int) {
	for _, values := range metadata {
		chain, _ := values[ClaudeDesktopQueryChainIDMetadataKey].(string)
		chain = strings.TrimSpace(chain)
		parsed, errParse := uuid.Parse(chain)
		if errParse != nil || parsed == uuid.Nil {
			continue
		}
		depth, ok := claudeDesktopQueryDepth(values[ClaudeDesktopQueryDepthMetadataKey])
		if ok {
			return parsed.String(), &depth
		}
	}
	return "", nil
}

func claudeDesktopQueryDepth(value any) (int, bool) {
	var depth int64
	switch typed := value.(type) {
	case int:
		depth = int64(typed)
	case int32:
		depth = int64(typed)
	case int64:
		depth = typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < 0 || typed > math.MaxInt32 || math.Trunc(typed) != typed {
			return 0, false
		}
		depth = int64(typed)
	case json.Number:
		parsed, errParse := typed.Int64()
		if errParse != nil {
			return 0, false
		}
		depth = parsed
	default:
		return 0, false
	}
	if depth < 0 || depth > math.MaxInt32 {
		return 0, false
	}
	return int(depth), true
}
