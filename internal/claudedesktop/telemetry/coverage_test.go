package telemetry

import (
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestCoverageDoesNotCountDeclarationsOrAnotherEndpointAsImplementation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		bundle.TelemetryEvidence.EmitterCoverage.ObservedScope = nil
		bundle.TelemetryEvidence.EmitterCoverage.SourceArtifact = "event-state-transitions"
		bundle.Telemetry.Events["declaration_only"] = claudeprofile.TelemetryEventProfile{EventName: "desktop_declaration_only"}
		bundle.Telemetry.Events["borrowed_sdk_name"] = claudeprofile.TelemetryEventProfile{EventName: "tengu_api_query"}
		bundle.TelemetryEvidence.EmitterCoverage.ObservableEventNames = []string{"tengu_api_query"}
		bundle.TelemetryEvidence.EmitterCoverage.ObservableEndpointEvents = []claudeprofile.TelemetryEndpointEvent{
			{EndpointRole: "desktop-event-logging", EventName: "tengu_api_query"},
			{EndpointRole: "sdk-event-logging", EventName: "tengu_api_query"},
		}
	})
	coverage := manager.Status().LiveEmitterCoverage
	if coverage.Status != "partial" || coverage.UnmodeledCapturedEventCount != 0 || coverage.UnmodeledEndpointEventCount != 1 {
		t.Fatalf("endpoint gap hidden by shared event name: %+v", coverage)
	}
	for _, name := range coverage.LiveEventNames {
		if name == "desktop_declaration_only" {
			t.Fatal("profile declaration counted as an executable emitter")
		}
	}
	if len(coverage.UnverifiedDeclaredEventNames) != 1 || coverage.UnverifiedDeclaredEventNames[0] != "desktop_declaration_only" {
		t.Fatalf("unsupported declarations not reported: %+v", coverage)
	}
	if len(coverage.EndpointEvents) != 2 || coverage.EndpointEvents[0].Executable || !coverage.EndpointEvents[1].Executable {
		t.Fatalf("endpoint inventory borrowed another role's trigger: %+v", coverage.EndpointEvents)
	}
}

func TestBuiltinCoverageReportsExecutableEndpointSubset(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	c := manager.Status().LiveEmitterCoverage
	if c.LiveEndpointEventCount+c.UnmodeledEndpointEventCount != c.ObservableEndpointEventCount {
		t.Fatalf("endpoint coverage does not reconcile: %+v", c)
	}
	if c.LiveEventNameCount != 231 || c.ObservableEventNameCount != 231 || c.LiveEndpointEventCount != 303 || c.ObservableEndpointEventCount != 303 || c.UnmodeledEndpointEventCount != 0 {
		t.Fatalf("unexpected built-in coverage counts: %+v", c)
	}
	if c.Status != "complete" || c.PayloadContractStatus != "complete" || c.SpecializedPayloadContractEventCount != 25 ||
		c.EndpointOnlyPayloadContractEventCount != 0 || c.UncapturedPayloadContractEventCount != 0 || len(c.UnverifiedDeclaredEventNames) != 0 {
		t.Fatalf("unexpected built-in coverage: %+v", c)
	}
	t.Logf("names=%d/%d; endpoint_events=%d/%d; endpoint_gaps=%d", c.LiveEventNameCount, c.ObservableEventNameCount, c.LiveEndpointEventCount, c.ObservableEndpointEventCount, c.UnmodeledEndpointEventCount)
}

func TestBuiltinCoverageGapInventory(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	coverage := manager.Status().LiveEmitterCoverage
	gaps := 0
	for _, pair := range coverage.EndpointEvents {
		if pair.Executable {
			continue
		}
		gaps++
		t.Logf("GAP\t%s\t%s", pair.EndpointRole, pair.EventName)
	}
	if gaps != coverage.UnmodeledEndpointEventCount {
		t.Fatalf("listed %d gaps, status reports %d", gaps, coverage.UnmodeledEndpointEventCount)
	}
}

func TestObservedScopeStatusRetainsFullUnionAndIndependentSnapshots(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	status := manager.Status()
	scope := status.TelemetryEvidence.ObservedScope
	if scope == nil || scope.Policy != "observations-not-completion" || scope.BaselineEndpointEventCount != 260 || scope.BaselineEventNameCount != 203 ||
		scope.SupplementalEndpointEventCount != 43 || scope.SupplementalEventNameCount != 28 || len(scope.Sources) != 13 {
		t.Fatalf("observed provenance lost: %+v", scope)
	}
	wantCounts := []int{260, 65, 37, 55, 141, 64, 102, 121, 37, 129, 0, 9, 118}
	for i, source := range scope.Sources {
		if source.EndpointEventCount != wantCounts[i] || len(source.SHA256) != 64 {
			t.Fatalf("source %d = %+v", i, source)
		}
	}
	c := status.LiveEmitterCoverage
	if c.ObservableEventNameCount != 231 || c.ObservableEndpointEventCount != 303 || len(c.EndpointEvents) != 303 {
		t.Fatalf("union shrunk: %+v", c)
	}
	if status.TelemetryEvidence.Corpus.FlowCount != 12109 || status.TelemetryEvidence.Corpus.EventCount != 15769 {
		t.Fatal("overlapping supplement counts were added to the original corpus")
	}
	seen := map[string]bool{}
	count := 0
	for _, pair := range c.EndpointEvents {
		key := pair.EndpointRole + "\x00" + pair.EventName
		if seen[key] {
			t.Fatalf("duplicate inventory pair %q", key)
		}
		seen[key] = true
		if pair.Executable {
			count++
		}
	}
	if count != c.LiveEndpointEventCount || count+c.UnmodeledEndpointEventCount != 303 {
		t.Fatal("inventory totals disagree")
	}
	scope.Sources[0].Artifact = "changed"
	c.EndpointEvents[0].EventName = "changed"
	next := manager.Status()
	if next.TelemetryEvidence.ObservedScope.Sources[0].Artifact == "changed" || next.LiveEmitterCoverage.EndpointEvents[0].EventName == "changed" {
		t.Fatal("status mutation altered the immutable profile")
	}
}
