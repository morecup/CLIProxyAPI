package config

import "testing"

func TestSanitizeOAuthModelAlias_PreservesOptionalFields(t *testing.T) {
	cfg := &Config{
		OAuthModelAlias: map[string][]OAuthModelAlias{
			" ClAuDe ": {
				{Name: " claude-opus-4-5 ", Alias: " opus ", Fork: true, DisplayName: " Opus ", ForceMapping: true},
				{Name: "claude-sonnet-4-5", Alias: "sonnet"},
			},
		},
	}

	cfg.SanitizeOAuthModelAlias()

	aliases := cfg.OAuthModelAlias["claude"]
	if len(aliases) != 2 {
		t.Fatalf("expected 2 sanitized aliases, got %d", len(aliases))
	}
	if aliases[0].Name != "claude-opus-4-5" || aliases[0].Alias != "opus" || !aliases[0].Fork || aliases[0].DisplayName != "Opus" || !aliases[0].ForceMapping {
		t.Fatalf("unexpected sanitized first alias: %+v", aliases[0])
	}
	if aliases[1].Name != "claude-sonnet-4-5" || aliases[1].Alias != "sonnet" || aliases[1].Fork || aliases[1].DisplayName != "" || aliases[1].ForceMapping {
		t.Fatalf("unexpected sanitized second alias: %+v", aliases[1])
	}
}

func TestSanitizeOAuthModelAlias_AllowsMultipleAliasesForSameName(t *testing.T) {
	cfg := &Config{
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"claude": {
				{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101", Fork: true},
				{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101-thinking", Fork: true},
				{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5", Fork: true},
			},
		},
	}

	cfg.SanitizeOAuthModelAlias()

	aliases := cfg.OAuthModelAlias["claude"]
	expected := []OAuthModelAlias{
		{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101", Fork: true},
		{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101-thinking", Fork: true},
		{Name: "claude-opus-4-5-thinking", Alias: "claude-opus-4-5", Fork: true},
	}
	if len(aliases) != len(expected) {
		t.Fatalf("expected %d sanitized aliases, got %d", len(expected), len(aliases))
	}
	for i, exp := range expected {
		if aliases[i].Name != exp.Name || aliases[i].Alias != exp.Alias || aliases[i].Fork != exp.Fork {
			t.Fatalf("expected alias %d to be name=%q alias=%q fork=%v, got name=%q alias=%q fork=%v", i, exp.Name, exp.Alias, exp.Fork, aliases[i].Name, aliases[i].Alias, aliases[i].Fork)
		}
	}
}
