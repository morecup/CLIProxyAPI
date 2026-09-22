package profile

import "testing"

func TestObservedScopeRejectsInconsistentEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TelemetryEvidenceProfile)
	}{
		{"missing scope", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope = nil }},
		{"wrong policy", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.Policy = "complete" }},
		{"wrong schema", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.SchemaVersion++ }},
		{"wrong union hash", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.UnionSHA256 = e.SourceManifestSHA256
		}},
		{"wrong baseline count", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.BaselineEndpointEventCount++ }},
		{"wrong supplement name count", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.SupplementalEventNameCount++ }},
		{"missing baseline", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.Sources = e.EmitterCoverage.ObservedScope.Sources[1:]
		}},
		{"duplicate source", func(e *TelemetryEvidenceProfile) {
			s := e.EmitterCoverage.ObservedScope
			s.Sources = append(s.Sources, s.Sources[1])
		}},
		{"second baseline", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.Sources[1].Kind = "baseline" }},
		{"outside artifact", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.Sources[1].Artifact = "../outside.json"
		}},
		{"bad source hash", func(e *TelemetryEvidenceProfile) { e.EmitterCoverage.ObservedScope.Sources[1].SHA256 = "invalid" }},
		{"duplicate membership", func(e *TelemetryEvidenceProfile) {
			s := &e.EmitterCoverage.ObservedScope.Sources[1]
			s.EndpointEvents = append(s.EndpointEvents, s.EndpointEvents[0])
		}},
		{"unknown role", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.Sources[0].EndpointEvents[0].EndpointRole = "unknown"
		}},
		{"URL instead of event", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.Sources[0].EndpointEvents[0].EventName = "https://private"
		}},
		{"query instead of event", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservedScope.Sources[0].EndpointEvents[0].EventName = "/login?token=value"
		}},
		{"missing coverage pair", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservableEndpointEvents = e.EmitterCoverage.ObservableEndpointEvents[1:]
		}},
		{"false coverage membership", func(e *TelemetryEvidenceProfile) {
			e.EmitterCoverage.ObservableEndpointEvents[0].EventName = "not_observed"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle, err := BuiltinV140609()
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(&bundle.TelemetryEvidence)
			if err = validateTelemetryEvidenceProfile(bundle.TelemetryEvidence, bundle.Telemetry, bundle.SDKTelemetry); err == nil {
				t.Fatal("accepted inconsistent scope")
			}
		})
	}
}

func TestObservedScopeLegacyBaselineCompatibility(t *testing.T) {
	bundle, err := BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	e := &bundle.TelemetryEvidence
	scope := e.EmitterCoverage.ObservedScope
	if scope == nil || len(scope.Sources) != 13 {
		t.Fatal("full source scope missing")
	}
	e.EmitterCoverage.ObservableEndpointEvents = scope.Sources[0].EndpointEvents
	names := map[string]bool{}
	for _, p := range e.EmitterCoverage.ObservableEndpointEvents {
		names[p.EventName] = true
	}
	original := []string{}
	for _, name := range e.EmitterCoverage.ObservableEventNames {
		if names[name] {
			original = append(original, name)
		}
	}
	e.EmitterCoverage.ObservableEventNames = original
	e.EmitterCoverage.SourceArtifact = "event-state-transitions"
	e.EmitterCoverage.ObservedScope = nil
	e.Artifacts = e.Artifacts[:len(e.Artifacts)-1]
	if err = validateTelemetryEvidenceProfile(*e, bundle.Telemetry, bundle.SDKTelemetry); err != nil {
		t.Fatal(err)
	}
	if len(original) != 203 || len(e.EmitterCoverage.ObservableEndpointEvents) != 260 {
		t.Fatal("legacy scope changed")
	}
}
