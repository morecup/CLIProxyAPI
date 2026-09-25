package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestLookupAPIKeyUpstreamModel(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey:  "k",
				BaseURL: "https://example.com",
				Models: []internalconfig.ClaudeModel{
					{Name: "claude-opus-4-6", Alias: "opus"},
					{Name: "claude-sonnet-4-6(low)", Alias: "sonnet"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": "https://example.com"}})

	tests := []struct {
		name   string
		authID string
		input  string
		want   string
	}{
		// Fast path + suffix preservation
		{"alias with suffix", "a1", "opus(8192)", "claude-opus-4-6(8192)"},
		{"alias without suffix", "a1", "opus", "claude-opus-4-6"},

		// Config suffix takes priority
		{"config suffix priority", "a1", "sonnet(high)", "claude-sonnet-4-6(low)"},
		{"config suffix no user suffix", "a1", "sonnet", "claude-sonnet-4-6(low)"},

		// Case insensitive
		{"uppercase alias", "a1", "OPUS", "claude-opus-4-6"},
		{"mixed case with suffix", "a1", "Opus(4096)", "claude-opus-4-6(4096)"},

		// Direct name lookup
		{"upstream name direct", "a1", "claude-opus-4-6", "claude-opus-4-6"},
		{"upstream name with suffix", "a1", "claude-opus-4-6(8192)", "claude-opus-4-6(8192)"},

		// Cache miss scenarios
		{"non-existent auth", "non-existent", "opus", ""},
		{"unknown alias", "a1", "unknown-alias", ""},
		{"empty auth ID", "", "opus", ""},
		{"empty model", "a1", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input)
			if resolved != tt.want {
				t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
			}
		})
	}
}

func TestAPIKeyModelAlias_ConfigHotReload(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "k",
				Models: []internalconfig.ClaudeModel{{Name: "claude-opus-4-6", Alias: "opus"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k"}})

	// Initial alias
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "opus"); resolved != "claude-opus-4-6" {
		t.Fatalf("before reload: got %q, want %q", resolved, "claude-opus-4-6")
	}

	// Hot reload with new alias
	mgr.SetConfig(&internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "k",
				Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4-6", Alias: "opus"}},
			},
		},
	})

	// New alias should take effect
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "opus"); resolved != "claude-sonnet-4-6" {
		t.Fatalf("after reload: got %q, want %q", resolved, "claude-sonnet-4-6")
	}
}

func TestAPIKeyModelAlias_MultipleProviders(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{APIKey: "claude-key", Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}}},
			{APIKey: "other-key", Models: []internalconfig.ClaudeModel{{Name: "claude-opus-4-6", Alias: "op46"}}},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "claude-auth-2", Provider: "claude", Attributes: map[string]string{"api_key": "other-key"}})

	tests := []struct {
		authID, input, want string
	}{
		{"claude-auth", "cs4", "claude-sonnet-4"},
		{"claude-auth-2", "op46", "claude-opus-4-6"},
	}

	for _, tt := range tests {
		if resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input); resolved != tt.want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
		}
	}
}

func TestApplyAPIKeyModelAlias(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{APIKey: "k", Models: []internalconfig.ClaudeModel{{Name: "claude-opus-4-6", Alias: "opus"}}},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	apiKeyAuth := &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k"}}
	oauthAuth := &Auth{ID: "oauth-auth", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	_, _ = mgr.Register(ctx, apiKeyAuth)

	tests := []struct {
		name       string
		auth       *Auth
		inputModel string
		wantModel  string
	}{
		{
			name:       "api_key auth with alias",
			auth:       apiKeyAuth,
			inputModel: "opus(8192)",
			wantModel:  "claude-opus-4-6(8192)",
		},
		{
			name:       "oauth auth passthrough",
			auth:       oauthAuth,
			inputModel: "some-model",
			wantModel:  "some-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolvedModel := mgr.applyAPIKeyModelAlias(tt.auth, tt.inputModel)

			if resolvedModel != tt.wantModel {
				t.Errorf("model = %q, want %q", resolvedModel, tt.wantModel)
			}
		})
	}
}

func TestResolveAPIKeyModelAliasWithResult_ForceMapping(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "claude-key",
			Models: []internalconfig.ClaudeModel{{
				Name:         "glm-5.2",
				Alias:        "claude-sonnet-latest",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "claude-sonnet-latest")
	if result.UpstreamModel != "glm-5.2" || !result.ForceMapping || result.OriginalAlias != "claude-sonnet-latest" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() = %+v, want upstream glm-5.2 with force mapping", result)
	}

	noRewrite := mgr.resolveAPIKeyModelAliasWithResult(auth, "glm-5.2")
	if noRewrite.UpstreamModel != "glm-5.2" || noRewrite.ForceMapping || noRewrite.OriginalAlias != "" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() direct upstream = %+v, want passthrough without rewrite", noRewrite)
	}
}

func TestResolveAPIKeyModelAliasWithResult_SameBasePreservesSuffix(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "k",
			Models: []internalconfig.ClaudeModel{{
				Name:         "claude-opus-4-6",
				Alias:        "claude-opus-4-6(8192)",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "k"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "claude-opus-4-6(8192)")
	if result.UpstreamModel != "claude-opus-4-6(8192)" || !result.ForceMapping || result.OriginalAlias != "claude-opus-4-6(8192)" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() = %+v, want same-base suffix preserved", result)
	}
}

func TestResolveAPIKeyModelAliasWithResult_ForceMappingUsesConfigAliasNotRequestSuffix(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "claude-key",
			Models: []internalconfig.ClaudeModel{{
				Name:         "glm-5.5",
				Alias:        "claude-sonnet-4-5",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "claude-sonnet-4-5(high)")
	if result.UpstreamModel != "glm-5.5(high)" {
		t.Fatalf("upstream = %q want glm-5.5(high)", result.UpstreamModel)
	}
	if result.OriginalAlias != "claude-sonnet-4-5" {
		t.Fatalf("OriginalAlias = %q want claude-sonnet-4-5", result.OriginalAlias)
	}
}
