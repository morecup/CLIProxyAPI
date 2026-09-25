package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildConfigChangeDetailsIncludesClaudeCoolingOverride(t *testing.T) {
	disabled := true
	enabled := false
	tests := []struct {
		name   string
		oldCfg *config.Config
		newCfg *config.Config
		want   string
	}{
		{
			name:   "claude inherit to false",
			oldCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key"}}},
			newCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: &enabled}}},
			want:   "claude[0].disable-cooling: inherit -> false",
		},
		{
			name:   "claude false to true",
			oldCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: &enabled}}},
			newCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: &disabled}}},
			want:   "claude[0].disable-cooling: false -> true",
		},
		{
			name:   "claude true to inherit",
			oldCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: &disabled}}},
			newCfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key"}}},
			want:   "claude[0].disable-cooling: true -> inherit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changes := strings.Join(BuildConfigChangeDetails(tc.oldCfg, tc.newCfg), "\n")
			if !strings.Contains(changes, tc.want) {
				t.Fatalf("changes missing %q:\n%s", tc.want, changes)
			}
		})
	}
}
