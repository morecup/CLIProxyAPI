package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestComputeClaudeModelsHash_Deterministic(t *testing.T) {
	models := []config.ClaudeModel{{Name: "a", Alias: "A"}, {Name: "b"}}
	h1 := ComputeClaudeModelsHash(models)
	h2 := ComputeClaudeModelsHash(models)
	if h1 == "" || h1 != h2 {
		t.Fatalf("expected deterministic hash, got %s / %s", h1, h2)
	}
	if h3 := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "a"}}); h3 == h1 {
		t.Fatalf("expected different hash when models change, got %s", h3)
	}
}

func TestComputeClaudeModelsHash_Empty(t *testing.T) {
	if got := ComputeClaudeModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil models, got %q", got)
	}
	if got := ComputeClaudeModelsHash([]config.ClaudeModel{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
}

func TestComputeClaudeModelsHashPreservesDuplicates(t *testing.T) {
	a := []config.ClaudeModel{
		{Name: "m1", Alias: "a1"},
		{Name: " "},
		{Name: "M1", Alias: "A1"},
	}
	b := []config.ClaudeModel{
		{Name: "m1", Alias: "a1"},
	}
	if h1, h2 := ComputeClaudeModelsHash(a), ComputeClaudeModelsHash(b); h1 == "" || h1 == h2 {
		t.Fatalf("expected duplicate routing entries to change hash, got %q / %q", h1, h2)
	}
}

func TestComputeClaudeModelsHashIncludesDisplayName(t *testing.T) {
	base := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", Alias: "a", DisplayName: "One"}})
	changed := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", Alias: "a", DisplayName: "Two"}})
	if base == "" || base == changed {
		t.Fatalf("display name must change model hash: %q / %q", base, changed)
	}
}

func TestComputeClaudeModelsHashIncludesForceMapping(t *testing.T) {
	if ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m"}}) == ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", ForceMapping: true}}) {
		t.Fatal("Claude force-mapping did not change model hash")
	}
}

func TestComputeClaudeModelsHashIncludesThinking(t *testing.T) {
	low := &registry.ThinkingSupport{Levels: []string{"low"}}
	high := &registry.ThinkingSupport{Levels: []string{"high"}}
	lowHash := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", Thinking: low}})
	highHash := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", Thinking: high}})
	if lowHash == "" || lowHash == highHash {
		t.Fatalf("thinking capability must change model hash: %q / %q", lowHash, highHash)
	}
}

func TestComputeExcludedModelsHash_Normalizes(t *testing.T) {
	hash1 := ComputeExcludedModelsHash([]string{" A ", "b", "a"})
	hash2 := ComputeExcludedModelsHash([]string{"a", " b", "A"})
	if hash1 == "" || hash2 == "" {
		t.Fatal("hash should not be empty for non-empty input")
	}
	if hash1 != hash2 {
		t.Fatalf("hash should be order/space insensitive for same multiset, got %s vs %s", hash1, hash2)
	}
	hash3 := ComputeExcludedModelsHash([]string{"c"})
	if hash1 == hash3 {
		t.Fatal("hash should differ for different normalized sets")
	}
}

func TestComputeExcludedModelsHash_Empty(t *testing.T) {
	if got := ComputeExcludedModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil input, got %q", got)
	}
	if got := ComputeExcludedModelsHash([]string{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
	if got := ComputeExcludedModelsHash([]string{"  ", ""}); got != "" {
		t.Fatalf("expected empty hash for whitespace-only entries, got %q", got)
	}
}
