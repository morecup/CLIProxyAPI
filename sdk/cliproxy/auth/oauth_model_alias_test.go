package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveOAuthUpstreamModel_SuffixPreservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aliases map[string][]internalconfig.OAuthModelAlias
		channel string
		input   string
		want    string
	}{
		{
			name: "numeric suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5(8192)",
			want:    "claude-opus-4-5-20251101(8192)",
		},
		{
			name: "level suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(high)",
		},
		{
			name: "no suffix unchanged",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5",
			want:    "claude-opus-4-5-20251101",
		},
		{
			name: "config suffix takes priority",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514(low)", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(low)",
		},
		{
			name: "auto suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5(auto)",
			want:    "claude-opus-4-5-20251101(auto)",
		},
		{
			name: "none suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5(none)",
			want:    "claude-opus-4-5-20251101(none)",
		},
		{
			name: "case insensitive alias lookup with suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "Claude-Opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5(high)",
			want:    "claude-opus-4-5-20251101(high)",
		},
		{
			name: "no alias returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "unknown-model(high)",
			want:    "",
		},
		{
			name: "wrong channel returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"other": {{Name: "other-upstream", Alias: "other-model"}},
			},
			channel: "claude",
			input:   "other-model(high)",
			want:    "",
		},
		{
			name: "empty suffix filtered out",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5()",
			want:    "claude-opus-4-5-20251101",
		},
		{
			name: "incomplete suffix treated as no suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5(high"}},
			},
			channel: "claude",
			input:   "claude-opus-4-5(high",
			want:    "claude-opus-4-5-20251101",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := NewManager(nil, nil, nil)
			mgr.SetConfig(&internalconfig.Config{})
			mgr.SetOAuthModelAlias(tt.aliases)

			auth := createAuthForChannel(tt.channel)
			got := mgr.resolveOAuthUpstreamModel(auth, tt.input)
			if got != tt.want {
				t.Errorf("resolveOAuthUpstreamModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func createAuthForChannel(channel string) *Auth {
	switch channel {
	case "claude":
		return &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	default:
		return &Auth{Provider: channel}
	}
}

func TestOAuthModelAliasChannel_APIKeyAuthUnsupported(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("claude", "api_key"); got != "" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want empty channel for API key auth", got)
	}
}

func TestOAuthModelAliasChannel_Claude(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("claude", "oauth"); got != "claude" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "claude")
	}
}

func TestOAuthModelAliasChannel_PluginProvider(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel(" Sample-Provider ", "oauth"); got != "sample-provider" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "sample-provider")
	}
	if got := OAuthModelAliasChannel("sample-provider", "api_key"); got != "" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want empty channel for API key", got)
	}
}

func TestApplyOAuthModelAlias_SuffixPreservation(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "test-auth-id", Provider: "claude"}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-opus-4-5(8192)")
	if resolvedModel != "claude-opus-4-5-20251101(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-opus-4-5-20251101(8192)")
	}
}

func TestApplyOAuthModelAlias_ForceMappingSameBasePreservesSuffix(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"claude": {{
			Name:         "claude-opus-4-5",
			Alias:        "claude-opus-4-5(8192)",
			ForceMapping: true,
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "test-auth-id", Provider: "claude"}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-opus-4-5(8192)")
	if resolvedModel != "claude-opus-4-5(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-opus-4-5(8192)")
	}
}

func TestApplyOAuthModelAlias_PerAuthForceMappingSameBasePreservesSuffix(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})

	auth := &Auth{
		ID:       "test-auth-id",
		Provider: "claude",
		Attributes: map[string]string{
			"model_aliases": `[{"name":"claude-opus-4-5","alias":"claude-opus-4-5(8192)","force-mapping":true}]`,
		},
	}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-opus-4-5(8192)")
	if resolvedModel != "claude-opus-4-5(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-opus-4-5(8192)")
	}
}

func TestApplyOAuthModelAlias_PerAuthOverridesGlobalAlias(t *testing.T) {
	t.Parallel()

	globalAliases := map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-global", Alias: "claude-sonnet-4-5"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(globalAliases)

	auth := &Auth{
		ID:       "claude-auth-id",
		Provider: "claude",
		Attributes: map[string]string{
			"auth_kind":     "oauth",
			"model_aliases": `[{"name":"claude-per-auth","alias":"claude-sonnet-4-5"}]`,
		},
	}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-sonnet-4-5(high)")
	if resolvedModel != "claude-per-auth(high)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-per-auth(high)")
	}
}

func TestApplyOAuthModelAlias_PerAuthAliasSkipsAPIKey(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})

	auth := &Auth{
		ID:       "claude-api-key-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"auth_kind":     "api_key",
			"model_aliases": `[{"name":"claude-per-auth","alias":"claude-sonnet-4-5"}]`,
		},
	}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-sonnet-4-5")
	if resolvedModel != "claude-sonnet-4-5" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-sonnet-4-5")
	}
}

func TestApplyOAuthModelAlias_PluginProvider(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"sample-provider": {{Name: "sample-model-latest", Alias: "sample-latest"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "sample-provider-auth", Provider: "sample-provider", Attributes: map[string]string{"auth_kind": "oauth"}}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "sample-latest")
	if resolvedModel != "sample-model-latest" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "sample-model-latest")
	}
}

func TestApplyOAuthModelAlias_PluginProviderSkipsAPIKey(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"sample-provider": {{Name: "sample-model-latest", Alias: "sample-latest"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "sample-provider-auth", Provider: "sample-provider", Attributes: map[string]string{"auth_kind": "api_key"}}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "sample-latest")
	if resolvedModel != "sample-latest" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "sample-latest")
	}
}
func TestApplyOAuthModelAliasWithResult_ForceMappingUsesConfigAliasNotRequestSuffix(t *testing.T) {
	t.Parallel()
	mgr := NewManager(nil, nil, nil)
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{
			Name: "claude-opus-4-5", Alias: "claude-opus-fast", Fork: true, ForceMapping: true,
		}},
	})
	auth := &Auth{ID: "t", Provider: "claude"}
	res := mgr.applyOAuthModelAliasWithResult(auth, "claude-opus-fast(high)")
	if res.UpstreamModel != "claude-opus-4-5(high)" {
		t.Fatalf("upstream = %q want claude-opus-4-5(high)", res.UpstreamModel)
	}
	if res.OriginalAlias != "claude-opus-fast" {
		t.Fatalf("OriginalAlias = %q want claude-opus-fast", res.OriginalAlias)
	}
}
func TestApplyOAuthModelAliasWithResultPrefersExactSuffixedAlias(t *testing.T) {
	t.Parallel()
	manager := NewManager(nil, nil, nil)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {
			{Name: "base-upstream", Alias: "public", Fork: true},
			{Name: "low-upstream", Alias: "public(low)", Fork: true, ForceMapping: true},
		},
	})
	auth := &Auth{ID: "exact-suffix", Provider: "claude"}
	result := manager.applyOAuthModelAliasWithResult(auth, "public(low)")
	if result.UpstreamModel != "low-upstream(low)" || !result.ForceMapping {
		t.Fatalf("exact suffixed alias result = %+v, want low-upstream(low) with force mapping", result)
	}
}

func TestApplyOAuthModelAliasWithResult_NoForceMappingPreservesRequestedModelInOriginalAlias(t *testing.T) {
	t.Parallel()
	mgr := NewManager(nil, nil, nil)
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{
			Name: "claude-opus-4-5", Alias: "claude-opus-fast", Fork: true, ForceMapping: false,
		}},
	})
	auth := &Auth{ID: "t", Provider: "claude"}
	res := mgr.applyOAuthModelAliasWithResult(auth, "claude-opus-fast(high)")
	if res.ForceMapping {
		t.Fatal("expected ForceMapping false")
	}
	if res.OriginalAlias != "claude-opus-fast(high)" {
		t.Fatalf("OriginalAlias = %q want requested model when force-mapping off", res.OriginalAlias)
	}
}
