package helps

import (
	"context"
	"strings"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const ClaudeDesktopParentPromptIDMetadataKey = "claude_desktop_parent_prompt_id"

// ClaudeDesktopParentPromptID accepts only explicit ownership, never the
// latest prompt in a session or a helper's independently generated prompt ID.
func ClaudeDesktopParentPromptID(ctx context.Context, metadata ...map[string]any) string {
	if value := cliproxyexecutor.ClaudeDesktopParentPromptIDFromContext(ctx); value != "" {
		return value
	}
	for _, values := range metadata {
		value, _ := values[ClaudeDesktopParentPromptIDMetadataKey].(string)
		if id, errParse := uuid.Parse(strings.TrimSpace(value)); errParse == nil && id != uuid.Nil {
			return id.String()
		}
	}
	return ""
}
