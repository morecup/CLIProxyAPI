package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelDisplayNameConfigDecoding(t *testing.T) {
	const yamlConfig = `claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        display-name: Claude Name
`
	const jsonConfig = `{"claude-api-key":[{"models":[{"name":"claude-upstream","alias":"claude-alias","display-name":"Claude Name"}]}]}`

	for _, tt := range []struct {
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
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if errDecode := tt.decode(&cfg); errDecode != nil {
				t.Fatalf("decode config: %v", errDecode)
			}
			if got := cfg.ClaudeKey[0].Models[0].DisplayName; got != "Claude Name" {
				t.Fatalf("Claude display name = %q", got)
			}
		})
	}
}
