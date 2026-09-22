package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestCompactionInstructionMatchesNativeFactory(t *testing.T) {
	data, err := os.ReadFile("testdata/compaction-instruction-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Custom, SHA256 string
			Bytes          int
		}
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Cases) != 9 {
		t.Fatal("invalid native instruction vectors")
	}
	bundle, err := BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range fixture.Cases {
		instruction, errGenerate := bundle.CompactionInstruction(tc.Custom)
		sum := sha256.Sum256([]byte(instruction))
		if errGenerate != nil || len(instruction) != tc.Bytes || hex.EncodeToString(sum[:]) != tc.SHA256 {
			t.Fatalf("native instruction vector %d differs", index)
		}
	}
	for _, unknown := range []*Bundle{nil, {}, {DesktopVersion: "1.40609.0.0", CodeVersion: "unknown"}, {DesktopVersion: "unknown", CodeVersion: "2.1.247"}} {
		if text, errUnknown := unknown.CompactionInstruction(""); errUnknown == nil || text != "" {
			t.Fatal("unknown version inherited a compact instruction")
		}
	}
}
