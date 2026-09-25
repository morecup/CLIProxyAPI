package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSetConfigAPIKeyExcludedAll(t *testing.T) {
	gotDisable := setConfigAPIKeyExcludedAll([]string{"claude-sonnet-4-6"}, true)
	if len(gotDisable) != 2 || gotDisable[0] != "claude-sonnet-4-6" || gotDisable[1] != "*" {
		t.Fatalf("unexpected disable list: %#v", gotDisable)
	}
	gotEnable := setConfigAPIKeyExcludedAll([]string{"claude-sonnet-4-6", "*"}, false)
	if len(gotEnable) != 1 || gotEnable[0] != "claude-sonnet-4-6" {
		t.Fatalf("unexpected enable list: %#v", gotEnable)
	}
}

func TestToggleConfigAPIKeyExcludedAllClaude(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "claude-test",
		}},
	}

	idGen := synthesizer.NewStableIDGenerator()
	authID, _ := idGen.Next("claude:apikey", "claude-test", "", "", "", "")
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "claude-test",
			"source":  "config:claude[abc]",
		},
	}

	handled, err := toggleConfigAPIKeyExcludedAll(cfg, auth, true)
	if err != nil || !handled {
		t.Fatalf("toggle claude: handled=%v err=%v", handled, err)
	}
	if len(cfg.ClaudeKey[0].ExcludedModels) != 1 || cfg.ClaudeKey[0].ExcludedModels[0] != "*" {
		t.Fatalf("excluded-models = %#v, want [*]", cfg.ClaudeKey[0].ExcludedModels)
	}

	handled, err = toggleConfigAPIKeyExcludedAll(cfg, auth, false)
	if err != nil || !handled {
		t.Fatalf("toggle claude enable: handled=%v err=%v", handled, err)
	}
	if len(cfg.ClaudeKey[0].ExcludedModels) != 0 {
		t.Fatalf("expected excluded-models cleared, got %#v", cfg.ClaudeKey[0].ExcludedModels)
	}
}
