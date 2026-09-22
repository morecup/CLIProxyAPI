package executor

import "testing"

func TestClaudeDesktopParentContextDoesNotRetainInvalidIdentifiers(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	ctx := WithClaudeDesktopParentPromptID(nil, " "+id+" ")
	if ClaudeDesktopParentPromptIDFromContext(ctx) != id {
		t.Fatal("valid parent lost")
	}
	for _, value := range []string{"PRIVATE_VALUE", "", "00000000-0000-0000-0000-000000000000"} {
		invalid := WithClaudeDesktopParentPromptID(ctx, value)
		if ClaudeDesktopParentPromptIDFromContext(invalid) != "" {
			t.Fatal("invalid value inherited earlier parent")
		}
	}
	if ClaudeDesktopParentPromptIDFromContext(nil) != "" {
		t.Fatal("nil context has parent")
	}
}
