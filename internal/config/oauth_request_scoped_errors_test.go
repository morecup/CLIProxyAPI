package config

import (
	"testing"
)

func TestParseConfigOAuthRequestScopedErrors(t *testing.T) {
	const yamlConfig = `
oauth-request-scoped-errors:
  claude:
    - status: 400
      match:
        - "maximum_context_length"
        - "context_length_exceeded"
      match-regexr:
        - "maximum_context_length$"
        - "^context_length_exceeded"
      action: "stop"
    - status: 429
      match:
        - "rate_limit"
      action: "continue-and-cooldown"
`

	cfg, err := ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatalf("ParseConfigFromBytes failed: %v", err)
	}

	if len(cfg.OAuthRequestScopedErrors) != 1 {
		t.Fatalf("cfg.OAuthRequestScopedErrors len = %d, want 1", len(cfg.OAuthRequestScopedErrors))
	}

	claudeRules, ok := cfg.OAuthRequestScopedErrors["claude"]
	if !ok || len(claudeRules) != 2 {
		t.Fatalf("claude rules missing or len != 2: %#v", claudeRules)
	}
	rule := claudeRules[0]
	if rule.Status != 400 || rule.Action != "stop" {
		t.Errorf("unexpected claude rule: %+v", rule)
	}
	if len(rule.Match) != 2 || len(rule.MatchRegexr) != 2 {
		t.Errorf("unexpected claude match len: %+v", rule)
	}
}

func TestSanitizeOAuthRequestScopedErrors(t *testing.T) {
	cfg := &Config{
		OAuthRequestScopedErrors: map[string][]RequestScopedErrorRule{
			" Claude ": {
				{
					Status:      400,
					Match:       []string{"  context_length  ", ""},
					MatchRegexr: []string{"  ^error.*  ", ""},
					Action:      " STOP ",
				},
				{
					Status: 0, // invalid status
					Match:  []string{"foo"},
					Action: "stop",
				},
				{
					Status: 400, // missing match / action
				},
			},
			" empty-channel ": {},
		},
	}

	cfg.SanitizeOAuthRequestScopedErrors()

	if len(cfg.OAuthRequestScopedErrors) != 1 {
		t.Fatalf("expected 1 sanitized channel, got %d", len(cfg.OAuthRequestScopedErrors))
	}

	rules := cfg.OAuthRequestScopedErrors["claude"]
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule for claude, got %d", len(rules))
	}
	if rules[0].Status != 400 || rules[0].Action != "stop" {
		t.Errorf("unexpected sanitized rule: %+v", rules[0])
	}
	if len(rules[0].Match) != 1 || rules[0].Match[0] != "context_length" {
		t.Errorf("unexpected sanitized match: %+v", rules[0].Match)
	}
	if len(rules[0].MatchRegexr) != 1 || rules[0].MatchRegexr[0] != "^error.*" {
		t.Errorf("unexpected sanitized regexr: %+v", rules[0].MatchRegexr)
	}
}
