package helps

import "testing"

func TestClaudeDesktopCompactionInstructionRequiresCompleteSignature(t *testing.T) {
	s := claudeCompactInstructionSignature{defaultBytes: 7, defaultHash: claudeCompactTextHash("default"),
		prefixBytes: len("prefix:"), prefixHash: claudeCompactTextHash("prefix:"), suffixBytes: len(":suffix"), suffixHash: claudeCompactTextHash(":suffix")}
	for _, text := range []string{"default", "prefix:custom:suffix", "prefix: 中文\n :suffix", "prefix:\u0085:suffix"} {
		if !s.matches(text) {
			t.Fatal("complete synthetic instruction did not match")
		}
	}
	for _, text := range []string{"", "defaulT", "default ", "quote default", "prefix:", "prefix::suffix", "prefix: \t\r\n\ufeff\u00a0:suffix", "prefix:custom:suffiX", "prefix:custom:suffix tail", "quote prefix:custom:suffix"} {
		if s.matches(text) {
			t.Fatal("partial, quoted, altered or whitespace-only instruction matched")
		}
	}
	for _, text := range []string{"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools", "REMINDER: Do NOT call any tools. Respond with plain text only"} {
		if IsClaudeDesktopCompactionInstruction("1.40609.0.0", "2.1.247", text) {
			t.Fatal("ordinary user text was classified by marker words")
		}
	}
}
