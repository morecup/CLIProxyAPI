package profile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTelemetryProfileRejectsContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Bundle)
		want   string
	}{
		{
			name: "endpoint",
			mutate: func(bundle *Bundle) {
				bundle.Telemetry.Endpoint = "https://example.invalid/api/event_logging/v2/batch"
			},
			want: "captured Claude Desktop event endpoint",
		},
		{
			name: "credential header",
			mutate: func(bundle *Bundle) {
				bundle.Telemetry.Headers = append(bundle.Telemetry.Headers, TelemetryHeader{Name: "Authorization", Value: "Bearer forbidden"})
			},
			want: "must not embed credential header",
		},
		{
			name: "event type",
			mutate: func(bundle *Bundle) {
				bundle.Telemetry.EventType = "UnverifiedEvent"
			},
			want: "unsupported telemetry event type",
		},
		{
			name: "batch policy",
			mutate: func(bundle *Bundle) {
				bundle.Telemetry.Batch.MaxPendingEvents = 1
			},
			want: "batch policy is invalid",
		},
		{
			name: "unsupported endpoint enabled",
			mutate: func(bundle *Bundle) {
				bundle.Telemetry.Unsupported[0].Status = "enabled"
			},
			want: "invalid unsupported telemetry endpoint",
		},
		{
			name: "evidence digest",
			mutate: func(bundle *Bundle) {
				bundle.TelemetryEvidence.SourceManifestSHA256 = "invalid"
			},
			want: "source manifest digest",
		},
		{
			name: "duplicate observed endpoint",
			mutate: func(bundle *Bundle) {
				bundle.TelemetryEvidence.ObservedEndpoints = append(bundle.TelemetryEvidence.ObservedEndpoints, bundle.TelemetryEvidence.ObservedEndpoints[0])
			},
			want: "duplicate observed telemetry endpoint",
		},
		{
			name: "observed body coverage count",
			mutate: func(bundle *Bundle) {
				bundle.TelemetryEvidence.ObservedEndpoints[0].MissingBodyFlowCount++
			},
			want: "invalid observed telemetry endpoint declaration",
		},
		{
			name: "SDK endpoint",
			mutate: func(bundle *Bundle) {
				bundle.SDKTelemetry.Endpoint = "https://claude.ai/api/event_logging/v2/batch"
			},
			want: "captured Anthropic event endpoint",
		},
		{
			name: "SDK header order",
			mutate: func(bundle *Bundle) {
				bundle.SDKTelemetry.HeaderOrder[0], bundle.SDKTelemetry.HeaderOrder[1] = bundle.SDKTelemetry.HeaderOrder[1], bundle.SDKTelemetry.HeaderOrder[0]
			},
			want: "header order",
		},
		{
			name: "SDK credential fixture",
			mutate: func(bundle *Bundle) {
				bundle.SDKTelemetry.Headers = append(bundle.SDKTelemetry.Headers, TelemetryHeader{Name: "Authorization", Value: "Bearer forbidden"})
			},
			want: "must not embed dynamic header",
		},
		{
			name: "SDK process profile",
			mutate: func(bundle *Bundle) {
				bundle.SDKTelemetry.Process.ConstrainedMemory = 0
			},
			want: "process constrained_memory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle, errBundle := BuiltinV140609()
			if errBundle != nil {
				t.Fatal(errBundle)
			}
			test.mutate(bundle)
			if errValidate := bundle.Validate(); errValidate == nil || !strings.Contains(errValidate.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", errValidate, test.want)
			}
		})
	}
}

func TestTelemetryEvidenceMatchesV140609CorpusWithoutSecrets(t *testing.T) {
	bundle, errBundle := BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	evidence := bundle.TelemetryEvidence
	if evidence.SourceManifestSHA256 != "14641f99e8f9311d0c14022b6c008f68f8f9d559353bf1323a76ee293ef8982a" {
		t.Fatalf("source manifest digest = %q", evidence.SourceManifestSHA256)
	}
	if evidence.Corpus.FlowCount != 12109 || evidence.Corpus.HTTPScenarioCount != 343 || evidence.Corpus.EligibleScenarioCount != 454 ||
		evidence.Corpus.TelemetryBatchFlowCount != 777 || evidence.Corpus.EventCount != 15769 || evidence.Corpus.EventNameCount != 189 {
		t.Fatalf("telemetry corpus evidence = %+v", evidence.Corpus)
	}
	statuses := make(map[string]string, len(evidence.ObservedEndpoints))
	for _, endpoint := range evidence.ObservedEndpoints {
		statuses[endpoint.Role] = endpoint.Status
	}
	for role, want := range map[string]string{
		"desktop-event-logging": "captured-delivery-enabled",
		"sdk-event-logging":     "captured-delivery-enabled",
		"segment":               "captured-delivery-enabled",
		"datadog-logs":          "captured-delivery-enabled",
		"datadog-logs-browser":  "captured-delivery-enabled",
		"datadog-rum":           "captured-delivery-enabled",
		"sentry":                "captured-delivery-enabled",
	} {
		if statuses[role] != want {
			t.Fatalf("observed endpoint %q status = %q, want %q", role, statuses[role], want)
		}
	}
	artifactHashes := make(map[string]string, len(evidence.Artifacts))
	for _, artifact := range evidence.Artifacts {
		artifactHashes[artifact.Name] = artifact.SHA256
	}
	if artifactHashes["telemetry-coverage"] != "9a7533d9d382af0ac9a661997ebcf90498ed6a2da174488f844103161d65e93d" {
		t.Fatalf("telemetry coverage digest = %q", artifactHashes["telemetry-coverage"])
	}
	if artifactHashes["event-state-transitions"] != "be9a8bd7337b3bfe542deb8c1082936fa626ce6e3887041364cecc71edea5aac" {
		t.Fatalf("event transition digest = %q", artifactHashes["event-state-transitions"])
	}
	if artifactHashes["event-state-closure"] != "1c53e3a3f52bfb3798b66731a8db6e5554ab6c5a1236877457d8ddaf69caf46f" {
		t.Fatalf("event closure digest = %q", artifactHashes["event-state-closure"])
	}
	if evidence.EmitterCoverage.SourceArtifact != "observed-event-union" || len(evidence.EmitterCoverage.ObservableEventNames) != 231 || len(evidence.EmitterCoverage.ObservableEndpointEvents) != 303 {
		t.Fatalf("emitter coverage evidence = %+v", evidence.EmitterCoverage)
	}
	payload, errMarshal := json.Marshal(evidence)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	lowerPayload := strings.ToLower(string(payload))
	for _, forbidden := range []string{"https://", "http://", "writekey", "authorization", "cookie", "access_token", "refresh_token", "account_uuid", "organization_uuid"} {
		if strings.Contains(lowerPayload, forbidden) {
			t.Fatalf("telemetry evidence contains forbidden captured material %q", forbidden)
		}
	}
}

func TestAuxiliaryTelemetryProfilesUseOnlyRuntimeMaterialReferences(t *testing.T) {
	bundle, errBundle := BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	wantRoles := map[string]string{
		"segment":              "segment_write_key",
		"datadog-logs":         "datadog_logs_api_key",
		"datadog-logs-browser": "datadog_logs_api_key",
		"datadog-rum":          "datadog_rum_client_token",
		"sentry":               "sentry_public_key",
	}
	for _, profile := range bundle.AuxiliaryTelemetry.All() {
		encoded, errMarshal := json.Marshal(profile)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		text := strings.ToLower(string(encoded))
		for _, forbidden := range []string{"dd-api-key=", "sentry_key=", "bearer ", "cookie="} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("auxiliary telemetry profile %q contains runtime material %q", profile.EndpointRole, forbidden)
			}
		}
		found := false
		for _, materialName := range profile.RuntimeMaterials {
			if materialName == wantRoles[profile.EndpointRole] {
				found = true
			}
		}
		if !found {
			t.Fatalf("auxiliary telemetry profile %q runtime material references = %+v", profile.EndpointRole, profile.RuntimeMaterials)
		}
	}
}

func TestSDKTelemetryProfileMatchesCapturedLargeBatchAndEnvironment(t *testing.T) {
	bundle, errBundle := BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	if got := bundle.SDKTelemetry.Batch.MaxEvents; got < 258 {
		t.Fatalf("SDK max_events = %d, must retain captured 258-event batch", got)
	}
	if got := bundle.SDKTelemetry.Batch.MaxBytes; got < 534974 {
		t.Fatalf("SDK max_bytes = %d, must retain captured 534974-byte batch", got)
	}
	if got := bundle.SDKTelemetry.Environment["terminal"]; got != "non-interactive" {
		t.Fatalf("SDK terminal = %#v", got)
	}
	if got := bundle.SDKTelemetry.Environment["package_managers"]; got != "npm" {
		t.Fatalf("SDK package_managers = %#v", got)
	}
}
