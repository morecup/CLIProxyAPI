package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMaxContextLengthConfigDecoding(t *testing.T) {
	const want = 1048576
	const yamlConfig = `claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        max-context-length: 1048576
`
	const jsonConfig = `{"claude-api-key":[{"models":[{"name":"claude-upstream","alias":"claude-alias","max-context-length":1048576}]}]}`

	for _, testCase := range []struct {
		name   string
		decode func(*Config) error
	}{
		{
			name: "YAML",
			decode: func(cfg *Config) error {
				return yaml.Unmarshal([]byte(yamlConfig), cfg)
			},
		},
		{
			name: "JSON",
			decode: func(cfg *Config) error {
				return json.Unmarshal([]byte(jsonConfig), cfg)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var cfg Config
			if errDecode := testCase.decode(&cfg); errDecode != nil {
				t.Fatalf("decode config: %v", errDecode)
			}

			if got := cfg.ClaudeKey[0].Models[0].MaxContextLength; got != want {
				t.Errorf("claude max-context-length = %d, want %d", got, want)
			}
		})
	}
}
