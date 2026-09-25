package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIKeyWeightValidation(t *testing.T) {
	tests := []struct {
		name   string
		weight string
		valid  bool
	}{
		{name: "negative excludes", weight: "-1", valid: true},
		{name: "maximum", weight: "1000000", valid: true},
		{name: "fraction", weight: "1.5", valid: false},
		{name: "above maximum", weight: "1000001", valid: false},
		{name: "integer overflow", weight: "9223372036854775808", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, errParse := ParseConfigBytes([]byte("claude-api-key:\n  - api-key: key\n    weight: " + test.weight + "\n"))
			if (errParse == nil) != test.valid {
				t.Fatalf("ParseConfigBytes(weight=%s) error = %v, want valid=%v", test.weight, errParse, test.valid)
			}
		})
	}
}

func TestAPIKeyWeightParsingAndZeroPersistence(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`claude-api-key:
  - api-key: key
    weight: 0
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].Weight == nil || *cfg.ClaudeKey[0].Weight != 0 {
		t.Fatalf("parsed weight = %#v, want explicit zero", cfg.ClaudeKey)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(`claude-api-key:
  - api-key: key
`), 0644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
	}
	saved, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if !strings.Contains(string(saved), "weight: 0") {
		t.Fatalf("saved config does not preserve explicit zero weight:\n%s", saved)
	}
}
