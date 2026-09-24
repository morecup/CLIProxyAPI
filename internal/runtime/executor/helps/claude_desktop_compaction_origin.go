package helps

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type claudeDesktopReactiveCompactionContextKey struct{}
type claudeDesktopCompactionOriginContextKey struct{}

// ClaudeDesktopCompactionRequestKind preserves the trigger independently of
// the summary algorithm. Threshold auto may use the reactive engine without a
// preceding API error. A wire hint alone never grants recovery/telemetry ownership.
func ClaudeDesktopCompactionRequestKind(ctx context.Context, headers http.Header) (string, error) {
	if ctx != nil {
		if kind, ok := ctx.Value(claudeDesktopCompactionOriginContextKey{}).(string); ok {
			return kind, nil
		}
		if owned, _ := ctx.Value(claudeDesktopReactiveCompactionContextKey{}).(bool); owned {
			return "reactive", nil
		}
	}
	kind := ""
	for name, values := range headers {
		if !strings.EqualFold(name, "x-cc-compaction-request") && !strings.EqualFold(name, "x-claude-code-compaction") {
			continue
		}
		for _, value := range values {
			value = strings.ToLower(strings.TrimSpace(value))
			switch value {
			case "manual", "auto", "reactive":
			default:
				return "", errors.New("invalid Claude Desktop compaction trigger: expected manual, auto, or reactive")
			}
			if kind != "" && kind != value {
				return "", errors.New("conflicting Claude Desktop compaction trigger headers")
			}
			kind = value
		}
	}
	if kind == "" {
		// An explicitly requested summary with no automatic trigger provenance is
		// manual. Only the owned PTL helper supplies the internal reactive marker.
		kind = "manual"
	}
	return kind, nil
}
