package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigOptionalRejectsClaudeHeaderDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
claude-header-defaults:
  user-agent: "  claude-cli/2.1.70 (external, cli)  "
  package-version: "  0.80.0  "
  runtime-version: "  v24.5.0  "
  os: "  MacOS  "
  arch: "  arm64  "
  timeout: "  900  "
  timezone: "  Pacific/Honolulu  "
  stabilize-device-profile: false
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := LoadConfigOptional(configPath, false)
	if err == nil || !strings.Contains(err.Error(), "claude-header-defaults") {
		t.Fatalf("LoadConfigOptional() error = %v, want migration error", err)
	}
}
