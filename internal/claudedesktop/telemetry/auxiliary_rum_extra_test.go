package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type auxiliaryRUMGolden struct {
	Lane             string   `json:"lane"`
	RUMSDKVersion    string   `json:"rum_sdk_version"`
	ExecutableEvents []string `json:"executable_events"`
	Events           []struct {
		EventName     string   `json:"event_name"`
		Roles         []string `json:"roles"`
		GatewayMoment *string  `json:"gateway_moment"`
		Condition     string   `json:"condition"`
		Boundary      string   `json:"boundary"`
	} `json:"events"`
}

func loadAuxiliaryRUMGolden(t *testing.T) auxiliaryRUMGolden {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "auxiliary-telemetry-rum-native.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var golden auxiliaryRUMGolden
	if errDecode := json.Unmarshal(raw, &golden); errDecode != nil {
		t.Fatal(errDecode)
	}
	return golden
}

func TestAuxiliaryRUMExtraPairsRetainHistoricalBoundaryEvidenceAndNowHaveCompanionSources(t *testing.T) {
	golden := loadAuxiliaryRUMGolden(t)
	if golden.Lane != "W8 rum-sentry" || golden.RUMSDKVersion != auxiliaryRUMSDKVersion || len(golden.ExecutableEvents) != 0 {
		t.Fatalf("golden header = %+v", golden)
	}
	boundaries := auxiliaryRUMBoundaries()
	if len(boundaries) != len(golden.Events) {
		t.Fatalf("boundaries = %d, golden events = %d", len(boundaries), len(golden.Events))
	}
	for index, boundary := range boundaries {
		pinned := golden.Events[index]
		if pinned.EventName != boundary.EventName || len(pinned.Roles) != 1 || pinned.Roles[0] != boundary.EndpointRole {
			t.Fatalf("boundary %d = %s/%s, golden = %v/%s", index, boundary.EndpointRole, boundary.EventName, pinned.Roles, pinned.EventName)
		}
		if pinned.GatewayMoment != nil || strings.TrimSpace(pinned.Boundary) == "" || strings.TrimSpace(pinned.Condition) == "" {
			t.Fatalf("golden %s is not a boundary record: %+v", pinned.EventName, pinned)
		}
		if strings.TrimSpace(boundary.NativeCondition) == "" || len(boundary.BlockingFields) == 0 {
			t.Fatalf("boundary %s lacks a native condition or blocking fields", boundary.EventName)
		}
		if !auxiliaryRUMBoundaryRegistered(boundary.EndpointRole, boundary.EventName) {
			t.Fatalf("%s/%s has no executable Desktop companion source", boundary.EndpointRole, boundary.EventName)
		}
	}
}

func TestAuxiliaryRUMExtraMappingsAndDeliveryContracts(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, boundary := range auxiliaryRUMBoundaries() {
		events := bundle.AuxiliaryTelemetry.DatadogRUM.Events
		if boundary.EndpointRole == sentryRole {
			events = bundle.AuxiliaryTelemetry.Sentry.Events
		}
		mapped := false
		for _, event := range events {
			if event.EventName == boundary.EventName {
				mapped = true
			}
		}
		if !mapped {
			t.Fatalf("bundle does not map %s/%s", boundary.EndpointRole, boundary.EventName)
		}
	}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle})
	t.Cleanup(manager.Close)
	before := manager.Status().LiveEmitterCoverage
	if before.Status != "complete" || before.PayloadContractStatus != "complete" || before.LiveEventNameCount != 231 || before.LiveEndpointEventCount != 303 || before.UnmodeledEndpointEventCount != 0 {
		t.Fatalf("W8 companion sources are not included in endpoint coverage: %+v", before)
	}

	// The existing RUM and Sentry batches must still round-trip through the
	// delivery encoders that would carry these events if a later lane finds a
	// gateway moment for them.
	materials := auxiliaryTestMaterials()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rumEncoded, errRUM := encodeDeliveryBatch(manager.auxiliaryDeliveries[datadogRUMRole], []Envelope{{CatalogEvent: "view", Payload: json.RawMessage(`{"type":"view","service":"claude-ai","source":"browser"}`)}}, map[string]string{"client_token": materials.DatadogRUMClientToken, "application_id": materials.DatadogRUMApplicationID}, now)
	if errRUM != nil {
		t.Fatal(errRUM)
	}
	if rumEncoded.query.Get("dd-evp-origin-version") != auxiliaryRUMSDKVersion {
		t.Fatalf("RUM intake pins SDK %q, golden pins %q", rumEncoded.query.Get("dd-evp-origin-version"), auxiliaryRUMSDKVersion)
	}
	var rumLine map[string]any
	if errDecode := json.Unmarshal(bytes.TrimSpace(rumEncoded.body), &rumLine); errDecode != nil || rumLine["type"] != "view" {
		t.Fatalf("RUM NDJSON did not decode: %v %v", errDecode, rumLine)
	}
	sentryEncoded, errSentry := encodeDeliveryBatch(manager.auxiliaryDeliveries[sentryRole], []Envelope{{CatalogEvent: "session", Payload: json.RawMessage(`{"init":true,"status":"ok"}`)}}, map[string]string{"public_key": materials.SentryPublicKey}, now)
	if errSentry != nil {
		t.Fatal(errSentry)
	}
	lines := bytes.Split(bytes.TrimSpace(sentryEncoded.body), []byte{'\n'})
	if len(lines) != 3 || !bytes.Contains(lines[1], []byte(`"type":"session"`)) || bytes.Contains(sentryEncoded.body, []byte(`"type":"attachment"`)) {
		t.Fatalf("Sentry envelope shape = %d lines: %s", len(lines), sentryEncoded.body)
	}
}
