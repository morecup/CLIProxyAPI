package executor

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

type claudeDesktopParentPromptContextKey struct{}

type claudeDesktopSessionContextKey struct{}

// ClaudeDesktopSessionBinding is internal execution ownership, not an HTTP
// session hint. A resolved Desktop UUID must not be hashed a second time when
// dispatching a side query. Consumers must verify every scope field.
type ClaudeDesktopSessionBinding struct {
	AccountID, ProfileID, Egress, SessionID string
}

func WithClaudeDesktopSessionBinding(ctx context.Context, binding ClaudeDesktopSessionBinding) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeDesktopSessionContextKey{}, binding)
}

func ClaudeDesktopSessionBindingFromContext(ctx context.Context) (ClaudeDesktopSessionBinding, bool) {
	if ctx == nil {
		return ClaudeDesktopSessionBinding{}, false
	}
	binding, ok := ctx.Value(claudeDesktopSessionContextKey{}).(ClaudeDesktopSessionBinding)
	return binding, ok
}

// WithClaudeDesktopParentPromptID associates a helper with the human prompt
// that scheduled it. It does not set the helper's request identity or headers.
// The Desktop runtime still verifies the account/session-local parent.
func WithClaudeDesktopParentPromptID(ctx context.Context, promptID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	id, errParse := uuid.Parse(strings.TrimSpace(promptID))
	value := ""
	if errParse == nil && id != uuid.Nil {
		value = id.String()
	}
	return context.WithValue(ctx, claudeDesktopParentPromptContextKey{}, value)
}

func ClaudeDesktopParentPromptIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(claudeDesktopParentPromptContextKey{}).(string)
	return value
}
