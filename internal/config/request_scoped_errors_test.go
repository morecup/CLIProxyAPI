package config

import (
	"testing"
)

func TestParseConfigRequestScopedErrors(t *testing.T) {
	const yamlConfig = `
claude-api-key:
  - api-key: claude-key-1
    request-scoped-errors:
      - status: 400
        match:
          - "maximum_context_length"
          - "context_length_exceeded"
        match-regexr:
          - "maximum_context_length$"
          - "^context_length_exceeded"
        action: stop
  - api-key: claude-key-2
    request-scoped-errors:
      - status: 500
        match:
          - "rate_limit_exceeded"
        action: continue-and-cooldown
`

	cfg, errParse := ParseConfigBytes([]byte(yamlConfig))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if len(cfg.ClaudeKey) != 2 || len(cfg.ClaudeKey[0].RequestScopedErrors) != 1 {
		t.Fatalf("claude[0].request-scoped-errors len = %d, want 1", len(cfg.ClaudeKey[0].RequestScopedErrors))
	}
	rule := cfg.ClaudeKey[0].RequestScopedErrors[0]
	if rule.Status != 400 || len(rule.Match) != 2 || len(rule.MatchRegexr) != 2 || rule.Action != "stop" {
		t.Fatalf("unexpected claude rule: %+v", rule)
	}

	if len(cfg.ClaudeKey[1].RequestScopedErrors) != 1 {
		t.Fatalf("claude[1].request-scoped-errors len = %d, want 1", len(cfg.ClaudeKey[1].RequestScopedErrors))
	}
	cooldownRule := cfg.ClaudeKey[1].RequestScopedErrors[0]
	if cooldownRule.Status != 500 || cooldownRule.Action != "continue-and-cooldown" {
		t.Fatalf("unexpected claude cooldown rule: %+v", cooldownRule)
	}
}
