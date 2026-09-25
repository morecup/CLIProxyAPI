package synthesizer

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:         "sk-ant-api-xxx",
					Prefix:         "main",
					BaseURL:        "https://api.anthropic.com",
					DisableCooling: boolPointer(true),
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "anthropic-compatible" {
		t.Errorf("expected provider anthropic-compatible, got %s", auths[0].Provider)
	}
	if auths[0].Label != "anthropic-compatible-apikey" {
		t.Errorf("expected label anthropic-compatible-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if auths[0].Attributes["config_index"] != "0" {
		t.Errorf("expected config_index 0, got %s", auths[0].Attributes["config_index"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if got := auths[0].Attributes["fingerprint_profile"]; got != "" {
		t.Errorf("fingerprint_profile = %q, want omitted", got)
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_ClaudeKeys_AllowsEmptyAPIKeyWithBaseURL(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:  "",
					BaseURL: "https://custom-claude.example.com",
					Headers: map[string]string{"Custom-Auth": "secret"},
				},
				{
					APIKey:  "   ",
					BaseURL: "https://custom-claude-2.example.com",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths for empty API keys with base URL, got %d", len(auths))
	}
	if auths[0].Attributes["base_url"] != "https://custom-claude.example.com" {
		t.Fatalf("expected base_url=https://custom-claude.example.com, got %s", auths[0].Attributes["base_url"])
	}
	if auths[0].Attributes["header:Custom-Auth"] != "secret" {
		t.Fatalf("expected header:Custom-Auth=secret, got %s", auths[0].Attributes["header:Custom-Auth"])
	}
	if auths[0].Attributes["auth_kind"] != "apikey" {
		t.Fatalf("expected auth_kind=apikey, got %s", auths[0].Attributes["auth_kind"])
	}
	if _, exists := auths[0].Attributes["api_key"]; exists {
		t.Fatalf("expected no api_key attribute for empty key, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_RejectsInvalidWeightsForClaudeKeys(t *testing.T) {
	invalidWeight := config.MaxCredentialWeight + 1
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key", Weight: &invalidWeight}}}
	wantPath := "claude-api-key[0].weight"

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize == nil {
		t.Fatal("Synthesize() accepted an invalid credential weight")
	}
	if auths != nil {
		t.Fatalf("Synthesize() auths = %#v, want nil", auths)
	}
	if !strings.Contains(errSynthesize.Error(), "synthesize config API key auths: "+wantPath) {
		t.Fatalf("Synthesize() error = %q, want contextual path %q", errSynthesize, wantPath)
	}
}

func TestConfigSynthesizer_OmittedWeightRemainsUnset(t *testing.T) {
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key"}}},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if _, exists := auths[0].Attributes[coreauth.AttributeWeight]; exists {
		t.Fatal("omitted weight was added to synthesized attributes")
	}
}

func TestConfigSynthesizer_NormalizesNonPositiveWeightToZero(t *testing.T) {
	weight := -5
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key", Weight: &weight}}},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if gotWeight := auths[0].Attributes[coreauth.AttributeWeight]; gotWeight != "0" {
		t.Fatalf("weight = %q, want 0", gotWeight)
	}
}

func TestConfigSynthesizer_PropagatesWeightsForClaudeKeys(t *testing.T) {
	weight := func(value int) *int { return &value }
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{{APIKey: "claude", Weight: weight(3)}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if gotWeight := auths[0].Attributes[coreauth.AttributeWeight]; gotWeight != "3" {
		t.Fatalf("auth weight = %q, want %q", gotWeight, "3")
	}
}

func TestConfigSynthesizer_RequestRetry(t *testing.T) {
	zero := 0
	positive := 2
	negative := -1
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-zero", RequestRetry: &zero},
				{APIKey: "claude-positive", RequestRetry: &positive},
				{APIKey: "claude-negative", RequestRetry: &negative},
				{APIKey: "claude-unset"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}

	want := map[string]any{
		"claude-zero":     0,
		"claude-positive": 2,
		"claude-negative": nil,
		"claude-unset":    nil,
	}
	got := make(map[string]any, len(auths))
	for _, auth := range auths {
		key := auth.Attributes["api_key"]
		if auth.Metadata == nil {
			got[key] = nil
			continue
		}
		if value, exists := auth.Metadata["request_retry"]; exists {
			got[key] = value
			continue
		}
		got[key] = nil
	}
	for key, expected := range want {
		actual, exists := got[key]
		if !exists {
			t.Fatalf("missing synthesized auth for %s", key)
		}
		if actual != expected {
			t.Fatalf("%s request_retry = %v, want %v", key, actual, expected)
		}
	}
}

func TestConfigSynthesizer_RequestScopedErrors(t *testing.T) {
	synth := NewConfigSynthesizer()
	rules := []config.RequestScopedErrorRule{
		{
			Status: 400,
			Match:  []string{"maximum_context_length"},
			Action: "stop",
		},
	}

	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key", RequestScopedErrors: rules},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}

	for _, auth := range auths {
		if auth.Metadata == nil {
			t.Fatalf("auth %s has nil metadata", auth.ID)
		}
		val, exists := auth.Metadata["request_scoped_errors"]
		if !exists {
			t.Fatalf("auth %s missing request_scoped_errors in metadata", auth.ID)
		}
		extracted, ok := val.([]config.RequestScopedErrorRule)
		if !ok || len(extracted) != 1 || extracted[0].Action != "stop" {
			t.Fatalf("auth %s unexpected request_scoped_errors: %#v", auth.ID, val)
		}
	}
}
