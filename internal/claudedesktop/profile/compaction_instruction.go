package profile

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// Generated from the reviewed installed instruction factory, not a recording
// or user conversation. Runtime-generated custom instructions are not stored.
//
//go:embed v140609.compaction-instruction.json
var nativeCompactionInstructionJSON []byte

type compactionInstructionTemplate struct {
	Standard string `json:"standard"`
	Prefix   string `json:"prefix"`
	Suffix   string `json:"suffix"`
}

var nativeCompactionInstruction = sync.OnceValues(func() (compactionInstructionTemplate, error) {
	var template compactionInstructionTemplate
	if json.Unmarshal(nativeCompactionInstructionJSON, &template) != nil {
		return template, errors.New("invalid Desktop compact instruction template")
	}
	for _, part := range []struct {
		text, sha256 string
		bytes        int
	}{
		{template.Standard, "63e2bfb4ded280d5057a66d8a034ef352a73960d97d955b990e78f04f0db1fb0", 6369},
		{template.Prefix, "197fe90a167c7837699b21eeaffe34574b3a06e7120c6ccf2e1fb40f8b489117", 6222},
		{template.Suffix, "e8304a513b9da6144d75836dfc5b7bf260a6cfd853ad6b02c19174cabe6e19cf", 174},
	} {
		sum := sha256.Sum256([]byte(part.text))
		if len(part.text) != part.bytes || hex.EncodeToString(sum[:]) != part.sha256 {
			return compactionInstructionTemplate{}, errors.New("unreviewed Desktop compact instruction template")
		}
	}
	return template, nil
})

// CompactionInstruction generates the pinned native k1e instruction. Model,
// system, headers and cache policy remain the compact request planner's job.
func (b *Bundle) CompactionInstruction(custom string) (string, error) {
	if b == nil || b.DesktopVersion != "1.40609.0.0" || b.CodeVersion != "2.1.247" {
		return "", errors.New("unreviewed Desktop compact instruction version")
	}
	template, err := nativeCompactionInstruction()
	if err != nil {
		return "", err
	}
	// Native trim only selects the branch; a nonblank custom slot is verbatim.
	if strings.TrimFunc(custom, compactionInstructionWhitespace) == "" {
		return template.Standard, nil
	}
	return template.Prefix + custom + template.Suffix, nil
}

func compactionInstructionWhitespace(r rune) bool {
	return r >= '\t' && r <= '\r' || r == ' ' || r == '\u00a0' || r == '\u1680' ||
		r >= '\u2000' && r <= '\u200a' || r == '\u2028' || r == '\u2029' || r == '\u202f' ||
		r == '\u205f' || r == '\u3000' || r == '\ufeff'
}
