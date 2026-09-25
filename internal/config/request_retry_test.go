package config

import "testing"

func TestParseConfigBytesRequestRetry(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
claude-api-key:
  - api-key: "claude-zero"
    request-retry: 0
  - api-key: "claude-unset"
  - api-key: "claude-neg"
    request-retry: -1
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if len(cfg.ClaudeKey) != 3 {
		t.Fatalf("claude-api-key count = %d, want 3", len(cfg.ClaudeKey))
	}
	if cfg.ClaudeKey[0].RequestRetry == nil || *cfg.ClaudeKey[0].RequestRetry != 0 {
		t.Fatalf("claude[0].request-retry = %v, want 0", cfg.ClaudeKey[0].RequestRetry)
	}
	if cfg.ClaudeKey[1].RequestRetry != nil {
		t.Fatalf("claude[1].request-retry = %v, want unset", cfg.ClaudeKey[1].RequestRetry)
	}
	if cfg.ClaudeKey[2].RequestRetry == nil || *cfg.ClaudeKey[2].RequestRetry != -1 {
		t.Fatalf("claude[2].request-retry = %v, want -1", cfg.ClaudeKey[2].RequestRetry)
	}
}
