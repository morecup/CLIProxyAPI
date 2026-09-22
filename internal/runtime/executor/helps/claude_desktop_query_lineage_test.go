package helps

import (
	"encoding/json"
	"math"
	"testing"
)

func TestClaudeDesktopQueryLineageRequiresOneCompleteSource(t *testing.T) {
	const chain = "8b900000-0000-4000-8000-000000000001"
	for _, value := range []any{0, int32(1), int64(2), float64(3), json.Number("4")} {
		got, depth := ClaudeDesktopQueryLineage(map[string]any{ClaudeDesktopQueryChainIDMetadataKey: chain, ClaudeDesktopQueryDepthMetadataKey: value})
		if got != chain || depth == nil {
			t.Fatalf("valid lineage %v = %q, %v", value, got, depth)
		}
	}
	for _, value := range []any{nil, "1", -1, int64(math.MaxInt32) + 1, 1.5, math.NaN(), math.Inf(1), json.Number("1e2")} {
		got, depth := ClaudeDesktopQueryLineage(map[string]any{ClaudeDesktopQueryChainIDMetadataKey: chain, ClaudeDesktopQueryDepthMetadataKey: value})
		if got != "" || depth != nil {
			t.Fatalf("invalid lineage %v = %q, %v", value, got, depth)
		}
	}
	got, depth := ClaudeDesktopQueryLineage(
		map[string]any{ClaudeDesktopQueryChainIDMetadataKey: chain},
		map[string]any{ClaudeDesktopQueryDepthMetadataKey: 2},
	)
	if got != "" || depth != nil {
		t.Fatal("mixed independent metadata sources into a lineage")
	}
	got, depth = ClaudeDesktopQueryLineage(
		map[string]any{ClaudeDesktopQueryChainIDMetadataKey: "invalid", ClaudeDesktopQueryDepthMetadataKey: 9},
		map[string]any{ClaudeDesktopQueryChainIDMetadataKey: chain, ClaudeDesktopQueryDepthMetadataKey: 2},
	)
	if got != chain || depth == nil || *depth != 2 {
		t.Fatal("did not use the next complete valid source")
	}
}
