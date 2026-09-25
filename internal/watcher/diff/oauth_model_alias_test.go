package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDiffOAuthModelAliasChanges_IncludesDisplayName(t *testing.T) {
	oldMap := map[string][]config.OAuthModelAlias{
		"claude": {
			{Name: "claude-opus-4-6", Alias: "claude-opus-4-6-thinking", DisplayName: "Opus 4.6"},
		},
	}
	newMap := map[string][]config.OAuthModelAlias{
		"claude": {
			{Name: "claude-opus-4-6", Alias: "claude-opus-4-6-thinking", DisplayName: "Opus 4.6 (Thinking)"},
		},
	}

	changes, affected := DiffOAuthModelAliasChanges(oldMap, newMap)
	expectContains(t, changes, "oauth-model-alias[claude]: updated (1 -> 1 entries)")
	if len(affected) != 1 || affected[0] != "claude" {
		t.Fatalf("expected claude to be affected, got %#v", affected)
	}
}
