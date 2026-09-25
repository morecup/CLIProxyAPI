package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildConfigModelsPropagatesMaxContextLength(t *testing.T) {
	const want = 1048576

	model := buildClaudeConfigModels(&config.ClaudeKey{
		Models: []config.ClaudeModel{{
			Name: "claude-upstream", Alias: "claude-alias", MaxContextLength: want,
		}},
	})[0]
	if model == nil {
		t.Fatal("model = nil")
	}
	if model.ContextLength != want {
		t.Errorf("context length = %d, want %d", model.ContextLength, want)
	}
	if model.MaxContextLength != want {
		t.Errorf("max context length = %d, want %d", model.MaxContextLength, want)
	}
}
