package helps

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClaudeDesktopParentPromptIsExplicitAndValidated(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const contextParent = "22222222-2222-4222-8222-222222222222"
	if got := ClaudeDesktopParentPromptID(nil, map[string]any{"claude_desktop_prompt_id": parent}); got != "" {
		t.Fatal("helper ID inferred as parent")
	}
	for _, value := range []any{nil, "PRIVATE_TEXT", "00000000-0000-0000-0000-000000000000", 42} {
		if got := ClaudeDesktopParentPromptID(nil, map[string]any{ClaudeDesktopParentPromptIDMetadataKey: value}); got != "" {
			t.Fatalf("invalid parent accepted: %T", value)
		}
	}
	metadata := map[string]any{ClaudeDesktopParentPromptIDMetadataKey: parent}
	if got := ClaudeDesktopParentPromptID(nil, metadata); got != parent {
		t.Fatal("explicit metadata lost")
	}
	ctx := cliproxyexecutor.WithClaudeDesktopParentPromptID(context.Background(), contextParent)
	if got := ClaudeDesktopParentPromptID(ctx, metadata); got != contextParent {
		t.Fatal("context ownership lost")
	}
	if metadata[ClaudeDesktopParentPromptIDMetadataKey] != parent {
		t.Fatal("metadata mutated")
	}
}
