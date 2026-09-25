package config

import "testing"

func TestParseConfigBytesPreservesCoolingOverridePresence(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
disable-cooling: true
claude-api-key:
  - api-key: claude-key
    disable-cooling: false
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	override := cfg.ClaudeKey[0].DisableCooling
	if override == nil || *override {
		t.Errorf("claude disable-cooling = %v, want explicit false", override)
	}
}
