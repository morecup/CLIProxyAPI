package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestV140609ObservedEventContractsRemainBoundToSanitizedCaptureCatalog(t *testing.T) {
	summary, errSummary := V140609ObservedEventContractSummaryValue()
	if errSummary != nil {
		t.Fatal(errSummary)
	}
	if summary.SchemaVersion != 1 || summary.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" || summary.EventCount != 99 {
		t.Fatalf("contract summary = %+v", summary)
	}
	if summary.SourceArtifact != "runtime/v140609/event-catalog.json" || summary.SourceSHA256 != "5010f40fbe64135b1515519576c4aacd69194f705bf0504a52eff6cf9dcc04d3" {
		t.Fatalf("contract provenance = %+v", summary)
	}
	contracts, errContracts := V140609ObservedEventContracts()
	if errContracts != nil {
		t.Fatal(errContracts)
	}
	events := make(map[string]ObservedExecutableEvent)
	for _, event := range V140609ObservedExecutableEvents() {
		events[event.Kind] = event
	}
	for kind, contract := range contracts {
		event, ok := events[kind]
		if !ok || contract.EventName != event.EventName || contract.Source != event.Source {
			t.Fatalf("contract %q = %+v, event = %+v", kind, contract, event)
		}
		if contract.ObservedCount <= 0 || len(contract.Fields) == 0 {
			t.Fatalf("contract %q lacks captured observations or fields: %+v", kind, contract)
		}
	}

	assertObservedField(t, contracts, "mcp_servers_listed", "server_count", "number", true)
	assertObservedField(t, contracts, "desktop_app_startup_perf", "app_ready_ms", "number", true)
	assertObservedField(t, contracts, "desktop_app_startup_perf", "process_footprint_sample_age_ms", "number", false)
	assertObservedField(t, contracts, "tengu_cli_flags", "flag_count", "number", true)
	assertObservedField(t, contracts, "tengu_cli_flags", "flags", "string", true)
}

func assertObservedField(t *testing.T, contracts map[string]ObservedEventContract, kind, fieldName, fieldType string, required bool) {
	t.Helper()
	contract, ok := contracts[kind]
	if !ok {
		t.Fatalf("contract %q is missing", kind)
	}
	field, ok := contract.Fields[fieldName]
	if !ok || field.Required != required {
		t.Fatalf("contract %q field %q = %+v", kind, fieldName, field)
	}
	for _, candidate := range field.Types {
		if candidate == fieldType {
			return
		}
	}
	t.Fatalf("contract %q field %q types = %v, want %q", kind, fieldName, field.Types, fieldType)
}

func TestV140609ObservedEventPayloadContractsAreComplete(t *testing.T) {
	summary, errSummary := V140609ObservedEventPayloadContractSummaryValue()
	if errSummary != nil {
		t.Fatal(errSummary)
	}
	if summary.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" || summary.ObservedCompanionEventCount != 124 ||
		summary.CapturedContractEventCount != 99 || summary.SpecializedContractEventCount != 25 || summary.EndpointOnlyEventCount != 0 {
		t.Fatalf("payload contract summary = %+v", summary)
	}
	if summary.CapturedSourceArtifact != "runtime/v140609/event-catalog.json" || len(summary.CapturedSourceSHA256) != 64 ||
		summary.SpecializedSourceArtifact != "profile/v140609.specialized-contracts.json" || len(summary.SpecializedSourceSHA256) != 64 {
		t.Fatalf("payload contract provenance = %+v", summary)
	}

	counts := map[string]int{}
	for _, event := range V140609ObservedExecutableEvents() {
		resolution, ok, errContract := V140609ObservedEventPayloadContract(event.Kind)
		if errContract != nil {
			t.Fatal(errContract)
		}
		if !ok {
			t.Fatalf("payload contract resolution missing for %q", event.Kind)
		}
		counts[resolution.Classification]++
		if resolution.Classification == ObservedPayloadContractEndpointOnly {
			t.Fatalf("event %q still has endpoint-only payload coverage", event.Kind)
		}
		if resolution.Contract.EventName != event.EventName || resolution.Contract.Source != event.Source {
			t.Fatalf("payload contract %q = %+v, event = %+v", event.Kind, resolution.Contract, event)
		}
	}
	if counts[ObservedPayloadContractCaptured] != 99 || counts[ObservedPayloadContractSpecialized] != 25 || counts[ObservedPayloadContractEndpointOnly] != 0 {
		t.Fatalf("payload contract class counts = %v", counts)
	}
}

func TestV140609EndpointContractGoldenMatchesSpecializedContracts(t *testing.T) {
	raw, errRead := os.ReadFile(filepath.Join("..", "telemetry", "testdata", "desktop-telemetry-endpoint-contracts-native.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	digest := sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
	if got := hex.EncodeToString(digest[:]); got != "c7b7732477cb3cc36e7f57d934f86d4c12625003cca6711537719d756eb26301" {
		t.Fatalf("endpoint contract golden SHA-256 = %s", got)
	}
	var golden struct {
		ProfileID               string            `json:"profile_id"`
		TargetEventCount        int               `json:"target_event_count"`
		OccurrenceCount         int               `json:"occurrence_count"`
		DesktopObservationCount int               `json:"desktop_observation_count"`
		SegmentObservationCount int               `json:"segment_observation_count"`
		ProgramPropertyOrder    []string          `json:"program_property_order"`
		DesktopSuffixOrder      []string          `json:"desktop_suffix_order"`
		ControlledRoutes        map[string]string `json:"controlled_routes"`
		Events                  []struct {
			Kind                 string                                `json:"kind"`
			EventName            string                                `json:"event_name"`
			DesktopObservedCount int                                   `json:"desktop_observed_count"`
			SegmentObservedCount int                                   `json:"segment_observed_count"`
			EventPropertyOrder   []string                              `json:"event_property_order"`
			DesktopRoutes        []string                              `json:"desktop_routes"`
			Fields               map[string]ObservedEventFieldContract `json:"fields"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(raw, &golden); errDecode != nil {
		t.Fatal(errDecode)
	}
	if golden.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" || golden.TargetEventCount != 11 || len(golden.Events) != 11 ||
		golden.OccurrenceCount != 68 || golden.DesktopObservationCount != 34 || golden.SegmentObservationCount != 34 {
		t.Fatalf("endpoint contract golden identity = %+v", golden)
	}
	contracts, errContracts := V140609SpecializedEventContracts()
	if errContracts != nil {
		t.Fatal(errContracts)
	}
	for _, observed := range golden.Events {
		contract, ok := contracts[observed.Kind]
		if !ok || contract.EventName != observed.EventName || contract.Source != ObservedEventSourceRenderer ||
			contract.ObservedCount != observed.DesktopObservedCount || observed.SegmentObservedCount != observed.DesktopObservedCount {
			t.Fatalf("golden event %q does not match contract: %+v", observed.Kind, contract)
		}
		wantOrder := append(append(append([]string{}, golden.ProgramPropertyOrder...), observed.EventPropertyOrder...), golden.DesktopSuffixOrder...)
		if !reflect.DeepEqual(contract.FieldOrder, wantOrder) {
			t.Fatalf("contract %q field order = %v, want %v", observed.Kind, contract.FieldOrder, wantOrder)
		}
		for name, field := range observed.Fields {
			actual, exists := contract.Fields[name]
			if !exists || actual.Required != field.Required || !reflect.DeepEqual(actual.Types, field.Types) {
				t.Fatalf("contract %q field %q = %+v, golden = %+v", observed.Kind, name, actual, field)
			}
		}
		for _, name := range append(append([]string{}, golden.ProgramPropertyOrder...), golden.DesktopSuffixOrder...) {
			if field := contract.Fields[name]; field.Owner != ObservedFieldOwnerProgram {
				t.Fatalf("contract %q field %q owner = %q", observed.Kind, name, field.Owner)
			}
		}
		wantPaths := make([]string, 0, len(observed.DesktopRoutes))
		for _, route := range observed.DesktopRoutes {
			wantPaths = append(wantPaths, golden.ControlledRoutes[route])
		}
		if !reflect.DeepEqual(contract.Fields["path"].Enum, wantPaths) {
			t.Fatalf("contract %q paths = %v, want %v", observed.Kind, contract.Fields["path"].Enum, wantPaths)
		}
	}
}
