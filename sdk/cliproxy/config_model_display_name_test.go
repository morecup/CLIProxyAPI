package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildConfigModelsDisplayName(t *testing.T) {
	model := buildClaudeConfigModels(&config.ClaudeKey{Models: []config.ClaudeModel{{
		Name: "claude-upstream", Alias: "claude-catalog", DisplayName: "Claude Catalog Name",
	}}})[0]
	if model.DisplayName != "Claude Catalog Name" {
		t.Fatalf("DisplayName = %q, want %q", model.DisplayName, "Claude Catalog Name")
	}
}

func TestBuildConfigModelsDisplayNameFallback(t *testing.T) {
	model := buildClaudeConfigModels(&config.ClaudeKey{Models: []config.ClaudeModel{{
		Name: "claude-upstream", Alias: "claude-catalog",
	}}})[0]
	if model.DisplayName != "claude-upstream" {
		t.Fatalf("DisplayName = %q, want upstream model name", model.DisplayName)
	}
}
