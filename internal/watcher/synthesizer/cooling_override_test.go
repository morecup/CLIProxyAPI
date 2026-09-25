package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func boolPointer(value bool) *bool {
	return &value
}

func TestConfigSynthesizerPreservesExplicitFalseCoolingOverrides(t *testing.T) {
	disableCooling := false
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{
		APIKey:         "claude-key",
		DisableCooling: &disableCooling,
	}}}

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Unix(100, 0).UTC(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	disabled, present := auths[0].DisableCoolingOverride()
	if !present || disabled {
		t.Fatalf("DisableCoolingOverride() = %t, %t, want false, true", disabled, present)
	}
}
