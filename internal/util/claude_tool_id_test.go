package util

import "testing"

func TestSanitizeClaudeToolIDFallback(t *testing.T) {
	if got := SanitizeClaudeToolID(""); got == "" {
		t.Fatal("SanitizeClaudeToolID(\"\") = empty, want generated fallback")
	}
	if got := SanitizeClaudeToolID("call_abc-123"); got != "call_abc-123" {
		t.Fatalf("SanitizeClaudeToolID() = %q, want unchanged", got)
	}
	if got := SanitizeClaudeToolID("call abc.123"); got != "call_abc_123" {
		t.Fatalf("SanitizeClaudeToolID() = %q, want sanitized", got)
	}
}
