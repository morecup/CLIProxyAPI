package profile

import (
	"fmt"
	"regexp"
	"strings"
)

// TelemetryObservedScope preserves the baseline and every supplement as
// separately hashed memberships. Overlapping observations are never summed;
// an indexed event is not evidence of a producer or complete causal behavior.
type TelemetryObservedScope struct {
	SchemaVersion                  int                       `json:"schema_version"`
	Policy                         string                    `json:"policy"`
	UnionSHA256                    string                    `json:"union_sha256"`
	BaselineEndpointEventCount     int                       `json:"baseline_endpoint_event_count"`
	BaselineEventNameCount         int                       `json:"baseline_event_name_count"`
	SupplementalEndpointEventCount int                       `json:"supplemental_endpoint_event_count"`
	SupplementalEventNameCount     int                       `json:"supplemental_event_name_count"`
	Sources                        []TelemetryObservedSource `json:"sources"`
}

type TelemetryObservedSource struct {
	Kind           string                   `json:"kind"`
	Artifact       string                   `json:"artifact"`
	SHA256         string                   `json:"sha256"`
	EndpointEvents []TelemetryEndpointEvent `json:"endpoint_events"`
}

var observedArtifactName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,159}\.json$`)
var observedEventName = regexp.MustCompile(`^[A-Za-z$/][A-Za-z0-9_.$:/-]{0,159}$`)

func validateTelemetryObservedScope(evidence TelemetryEvidenceProfile) error {
	coverage := evidence.EmitterCoverage
	scope := coverage.ObservedScope
	if scope == nil {
		if coverage.SourceArtifact == "event-state-transitions" {
			return nil // Older baseline-only bundles keep their own scope.
		}
		return fmt.Errorf("claude desktop profile: observed union scope is missing")
	}
	if coverage.SourceArtifact != "observed-event-union" || scope.SchemaVersion != 1 || scope.Policy != "observations-not-completion" ||
		!validSHA256(scope.UnionSHA256) || len(scope.Sources) == 0 || len(scope.Sources) > 128 {
		return fmt.Errorf("claude desktop profile: observed union scope is invalid")
	}
	artifactMatches := false
	for _, artifact := range evidence.Artifacts {
		if artifact.Name == coverage.SourceArtifact && artifact.SHA256 == scope.UnionSHA256 {
			artifactMatches = true
		}
	}
	if !artifactMatches {
		return fmt.Errorf("claude desktop profile: observed union artifact digest mismatch")
	}
	roles := make(map[string]bool, len(evidence.ObservedEndpoints))
	for _, endpoint := range evidence.ObservedEndpoints {
		roles[endpoint.Role] = true
	}
	sources, all, allNames := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	baseline, baselineNames := make(map[string]bool), make(map[string]bool)
	baselineCount := 0
	for index, source := range scope.Sources {
		if !observedArtifactName.MatchString(source.Artifact) || !validSHA256(source.SHA256) || sources[source.Artifact] ||
			(source.Kind != "baseline" && source.Kind != "supplement") || len(source.EndpointEvents) > 4096 {
			return fmt.Errorf("claude desktop profile: observed union source is invalid")
		}
		sources[source.Artifact] = true
		if source.Kind == "baseline" {
			baselineCount++
			if index != 0 || baselineCount != 1 || len(source.EndpointEvents) == 0 {
				return fmt.Errorf("claude desktop profile: observed union baseline must be first and unique")
			}
		}
		previous := ""
		for _, event := range source.EndpointEvents {
			key := event.EndpointRole + "\x00" + event.EventName
			if !roles[event.EndpointRole] || !observedEventName.MatchString(event.EventName) || strings.Contains(event.EventName, "://") || key <= previous {
				return fmt.Errorf("claude desktop profile: observed union source membership is invalid")
			}
			previous = key
			all[key], allNames[event.EventName] = true, true
			if source.Kind == "baseline" {
				baseline[key], baselineNames[event.EventName] = true, true
			}
		}
	}
	if baselineCount != 1 || len(baseline) != scope.BaselineEndpointEventCount || len(baselineNames) != scope.BaselineEventNameCount ||
		len(all)-len(baseline) != scope.SupplementalEndpointEventCount || len(allNames)-len(baselineNames) != scope.SupplementalEventNameCount ||
		len(all) != len(coverage.ObservableEndpointEvents) || len(allNames) != len(coverage.ObservableEventNames) {
		return fmt.Errorf("claude desktop profile: observed union counts do not reconcile")
	}
	for _, event := range coverage.ObservableEndpointEvents {
		if !all[event.EndpointRole+"\x00"+event.EventName] {
			return fmt.Errorf("claude desktop profile: observed union omits a coverage pair")
		}
	}
	for _, name := range coverage.ObservableEventNames {
		if !allNames[name] {
			return fmt.Errorf("claude desktop profile: observed union omits a coverage name")
		}
	}
	return nil
}
