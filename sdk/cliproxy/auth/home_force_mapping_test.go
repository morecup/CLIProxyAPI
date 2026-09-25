package auth

import (
	"testing"
)

func TestHomeForceMappingAliasResult(t *testing.T) {
	auth := &Auth{
		Provider: "xai",
		Attributes: map[string]string{
			homeUpstreamModelAttributeKey: "grok-4.5",
			homeForceMappingAttributeKey:  "true",
			homeOriginalAliasAttributeKey: "grok-latest",
		},
	}

	result := homeForceMappingAliasResult(auth, "grok-latest")
	if result.UpstreamModel != "grok-4.5" || !result.ForceMapping || result.OriginalAlias != "grok-latest" {
		t.Fatalf("homeForceMappingAliasResult() = %+v", result)
	}
}

func TestHomeForceMappingAliasResultRequiresSameOriginalAlias(t *testing.T) {
	auth := &Auth{
		Provider: "xai",
		Attributes: map[string]string{
			homeUpstreamModelAttributeKey: "grok-4.5",
			homeForceMappingAttributeKey:  "true",
			homeOriginalAliasAttributeKey: "grok-latest",
		},
	}

	if result := homeForceMappingAliasResult(auth, " GROK-LATEST "); !result.ForceMapping {
		t.Fatalf("homeForceMappingAliasResult() = %+v, want same alias force mapping", result)
	}
	if result := homeForceMappingAliasResult(auth, "grok-latest(high)"); !result.ForceMapping {
		t.Fatalf("homeForceMappingAliasResult() = %+v, want reasoning suffix force mapping", result)
	}
	if result := homeForceMappingAliasResult(auth, "grok-latest(custom)"); result.ForceMapping || result.OriginalAlias != "" {
		t.Fatalf("homeForceMappingAliasResult() = %+v, want no force mapping for a custom suffix", result)
	}
	if result := homeForceMappingAliasResult(auth, "grok-other"); result.ForceMapping || result.OriginalAlias != "" {
		t.Fatalf("homeForceMappingAliasResult() = %+v, want no force mapping for a different alias", result)
	}
}

func TestHomeForceMappingAliasResultRequiresExplicitFlag(t *testing.T) {
	auth := &Auth{
		Provider: "xai",
		Attributes: map[string]string{
			homeUpstreamModelAttributeKey: "grok-4.5",
			homeOriginalAliasAttributeKey: "grok-latest",
		},
	}

	result := homeForceMappingAliasResult(auth, "grok-latest")
	if result.ForceMapping || result.OriginalAlias != "" {
		t.Fatalf("homeForceMappingAliasResult() = %+v, want no force mapping", result)
	}
}
