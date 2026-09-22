package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type claudeCompactInstructionSignature struct {
	defaultBytes, prefixBytes, suffixBytes int
	defaultHash, prefixHash, suffixHash    string
}

// These hashes describe the complete v140609 / 2.1.247 instruction factory,
// including its optional custom-instruction slot. They contain no prompt text.
// A role match is not evidence of parent ownership or summary application.
var claudeCompactV140609 = claudeCompactInstructionSignature{
	defaultBytes: 6369,
	defaultHash:  "63e2bfb4ded280d5057a66d8a034ef352a73960d97d955b990e78f04f0db1fb0",
	prefixBytes:  6222,
	prefixHash:   "197fe90a167c7837699b21eeaffe34574b3a06e7120c6ccf2e1fb40f8b489117",
	suffixBytes:  174,
	suffixHash:   "e8304a513b9da6144d75836dfc5b7bf260a6cfd853ad6b02c19174cabe6e19cf",
}

// IsClaudeDesktopCompactionInstruction recognizes a reviewed whole instruction,
// not marker words that can occur in an ordinary user question. An unknown
// version must obtain its own evidence instead of inheriting these signatures.
func IsClaudeDesktopCompactionInstruction(desktopVersion, codeVersion, text string) bool {
	return desktopVersion == "1.40609.0.0" && codeVersion == "2.1.247" && claudeCompactV140609.matches(text)
}

func (s claudeCompactInstructionSignature) matches(text string) bool {
	if len(text) == s.defaultBytes && claudeCompactTextHash(text) == s.defaultHash {
		return true
	}
	if s.prefixBytes <= 0 || s.suffixBytes <= 0 || len(text) <= s.prefixBytes+s.suffixBytes {
		return false
	}
	if claudeCompactTextHash(text[:s.prefixBytes]) != s.prefixHash || claudeCompactTextHash(text[len(text)-s.suffixBytes:]) != s.suffixHash {
		return false
	}
	// JavaScript trim excludes U+0085 and includes U+FEFF. Go TrimSpace has
	// different membership, so it cannot decide the native optional slot.
	return strings.TrimFunc(text[s.prefixBytes:len(text)-s.suffixBytes], claudeCompactJSWhitespace) != ""
}

func claudeCompactTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func claudeCompactJSWhitespace(r rune) bool {
	return r >= '\t' && r <= '\r' || r == ' ' || r == '\u00a0' || r == '\u1680' ||
		r >= '\u2000' && r <= '\u200a' || r == '\u2028' || r == '\u2029' || r == '\u202f' ||
		r == '\u205f' || r == '\u3000' || r == '\ufeff'
}
