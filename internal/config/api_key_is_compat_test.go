package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAPIKeyModelIsCompatConfigDecoding(t *testing.T) {
	const yamlConfig = `claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        is-compat: true
      - name: claude-native
        alias: claude-native
`

	var cfg Config
	if errDecode := yaml.Unmarshal([]byte(yamlConfig), &cfg); errDecode != nil {
		t.Fatalf("decode error: %v", errDecode)
	}

	if len(cfg.ClaudeKey) != 1 || !cfg.ClaudeKey[0].Models[0].IsCompat {
		t.Fatalf("claude-api-key IsCompat = %+v, want true", cfg.ClaudeKey)
	}
	if cfg.ClaudeKey[0].Models[1].IsCompat {
		t.Fatal("claude-api-key omitted IsCompat = true, want default false")
	}
}
