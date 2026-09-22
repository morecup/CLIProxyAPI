package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeDesktopRendererTelemetryObservationUsesControlledRoute(t *testing.T) {
	observation := ClaudeDesktopRendererTelemetryObservation{
		Kind:       "chorus_ideas_suggestions_shown",
		Route:      ClaudeDesktopRendererRouteNew,
		Properties: map[string]any{"idea_id": "idea"},
	}
	encoded, errMarshal := json.Marshal(observation)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(encoded), `"route":"new"`) || strings.Contains(string(encoded), `"path"`) {
		t.Fatalf("renderer observation JSON = %s", encoded)
	}
	var decoded ClaudeDesktopRendererTelemetryObservation
	if errUnmarshal := json.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if decoded.Route != ClaudeDesktopRendererRouteNew {
		t.Fatalf("renderer route = %q", decoded.Route)
	}
	if ClaudeDesktopRendererRouteShell != "shell" || ClaudeDesktopRendererRouteSession != "session" {
		t.Fatal("controlled renderer route values changed")
	}
}
