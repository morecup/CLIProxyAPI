package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type desktopRendererInventoryGolden struct {
	Events []struct {
		EventName          string         `json:"event_name"`
		DualFireFact       string         `json:"dual_fire_fact"`
		PropertyKeys       []string       `json:"property_keys"`
		Constants          map[string]any `json:"constants"`
		DualFireSuffixKeys []string       `json:"dual_fire_suffix_keys"`
	} `json:"events"`
	Boundaries []struct {
		EventName    string `json:"event_name"`
		Executable   bool   `json:"executable"`
		DualFireFact string `json:"dual_fire_fact"`
	} `json:"boundaries"`
}

func loadDesktopRendererInventoryGolden(t *testing.T) desktopRendererInventoryGolden {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "desktop-telemetry-renderer-inventory-native.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var golden desktopRendererInventoryGolden
	if errDecode := json.Unmarshal(raw, &golden); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(golden.Events) != 3 {
		t.Fatalf("golden pins %d executable events, want 3", len(golden.Events))
	}
	return golden
}

// orderedJSONKeys returns the top-level keys of a JSON object in wire order.
func orderedJSONKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("properties are not a JSON object: %s", raw)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestRendererShellStatePropertiesKeepCapturedKeyOrder(t *testing.T) {
	golden := loadDesktopRendererInventoryGolden(t)
	events, errBuild := buildRendererShellStateEvents(testAccountA, testOrgA, "stripe_subscription", "1.40609.0")
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if len(events) != len(golden.Events) {
		t.Fatalf("built %d events, golden %d", len(events), len(golden.Events))
	}
	for i, expected := range golden.Events {
		event := events[i]
		if rendererShellStateEventNames[event.Fact] != expected.EventName || event.Fact != expected.DualFireFact {
			t.Fatalf("event %d = %s (%s), golden %s (%s)", i, event.Fact, rendererShellStateEventNames[event.Fact], expected.DualFireFact, expected.EventName)
		}
		keys := orderedJSONKeys(t, event.Properties)
		if strings.Join(keys, ",") != strings.Join(expected.PropertyKeys, ",") {
			t.Fatalf("%s key order = %v, golden %v", expected.EventName, keys, expected.PropertyKeys)
		}
		var properties map[string]any
		if errDecode := json.Unmarshal(event.Properties, &properties); errDecode != nil {
			t.Fatal(errDecode)
		}
		for key, want := range expected.Constants {
			got, _ := json.Marshal(properties[key])
			wantEncoded, _ := json.Marshal(want)
			if string(got) != string(wantEncoded) {
				t.Fatalf("%s.%s = %s, golden constant %s", expected.EventName, key, got, wantEncoded)
			}
		}
		if properties["account_uuid"] != testAccountA || properties["organization_uuid"] != testOrgA || properties["billing_type"] != "stripe_subscription" {
			t.Fatalf("%s identity properties = %v", expected.EventName, properties)
		}
	}
}

func TestRendererShellStateFirstSessionDeliversSegmentAndDesktopCopies(t *testing.T) {
	golden := loadDesktopRendererInventoryGolden(t)
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	mapShellState := func(bundle *claudeprofile.Bundle) {
		for fact, name := range rendererShellStateEventNames {
			bundle.AuxiliaryTelemetry.Segment.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
			bundle.Telemetry.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
		}
	}
	// The built-in bundle declares the shell-state facts; the baseline removes
	// them so the delta still proves the declarations make the pairs executable.
	baseline := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		for fact := range rendererShellStateEventNames {
			delete(bundle.AuxiliaryTelemetry.Segment.Events, fact)
			delete(bundle.Telemetry.Events, fact)
		}
	})
	before := baseline.Status().LiveEmitterCoverage
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, mapShellState)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	auth.Metadata["subscription_type"] = "stripe_subscription"
	// Two sessions of the same account: the shell load is reported once.
	for _, sessionID := range []string{"99999999-9999-4999-8999-999999999999", "99999999-9999-4999-8999-999999999998"} {
		span := manager.BeginRequest(t.Context(), auth, testRequestFacts(sessionID))
		if !span.Active() {
			t.Fatal("main span inactive")
		}
		span.FinishSuccess(t.Context())
	}
	if errFlush := manager.Flush(t.Context()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	segmentProperties := make(map[string][]json.RawMessage)
	segmentAnonymous := ""
	desktopProperties := make(map[string][]string)
	var desktopOrder []string
	for _, request := range doer.Requests() {
		if strings.HasPrefix(request.URL, "https://claude.ai/") {
			var batch struct {
				Events []struct {
					EventType string `json:"event_type"`
					EventData struct {
						EventName        string `json:"event_name"`
						AccountUUID      string `json:"account_uuid"`
						OrganizationUUID string `json:"organization_uuid"`
						Properties       string `json:"properties"`
					} `json:"event_data"`
				} `json:"events"`
			}
			if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
				t.Fatal(errDecode)
			}
			for _, event := range batch.Events {
				desktopOrder = append(desktopOrder, event.EventData.EventName)
				if event.EventType != desktopRendererEventType {
					continue
				}
				if event.EventData.AccountUUID != testAccountA || event.EventData.OrganizationUUID != testOrgA {
					t.Fatalf("desktop copy envelope = %+v", event.EventData)
				}
				desktopProperties[event.EventData.EventName] = append(desktopProperties[event.EventData.EventName], event.EventData.Properties)
			}
			continue
		}
		var batch struct {
			Batch []struct {
				Type        string          `json:"type"`
				Event       string          `json:"event"`
				Properties  json.RawMessage `json:"properties"`
				AnonymousID string          `json:"anonymousId"`
			} `json:"batch"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode == nil {
			for _, item := range batch.Batch {
				if item.Type == "track" && item.Event != "" {
					segmentProperties[item.Event] = append(segmentProperties[item.Event], item.Properties)
					segmentAnonymous = item.AnonymousID
				}
			}
		}
	}
	joined := "," + strings.Join(desktopOrder, ",") + ","
	shellLoad := ",claudeai.desktop.sidebar.state_set,claudeai.settings.chat_font.active,claudeai.epitaxy.side_pane.layout_changed,"
	if strings.Count(joined, shellLoad) != 1 || !strings.Contains(joined, ",desktop_ccd_session_initialized,") {
		t.Fatalf("shell-state copies must be delivered once in captured order: %v", desktopOrder)
	}
	if strings.Index(joined, ",desktop_ccd_session_initialized,") < strings.Index(joined, shellLoad) {
		t.Fatalf("shell load must precede the first session initialisation: %v", desktopOrder)
	}
	for _, expected := range golden.Events {
		twins := segmentProperties[expected.EventName]
		copies := desktopProperties[expected.EventName]
		if len(twins) != 1 || len(copies) != 1 {
			t.Fatalf("%s delivered %d Segment tracks and %d Desktop copies, want exactly one each per activation", expected.EventName, len(twins), len(copies))
		}
		if keys := orderedJSONKeys(t, twins[0]); strings.Join(keys, ",") != strings.Join(expected.PropertyKeys, ",") {
			t.Fatalf("Segment %s key order = %v, golden %v", expected.EventName, keys, expected.PropertyKeys)
		}
		suffix, _ := json.Marshal(segmentAnonymous)
		want := strings.TrimSuffix(strings.TrimSpace(string(twins[0])), "}") + `,"_dual_fire":true,"anonymous_id":` + string(suffix) + `,"service_name":"claude_ai","path":"/epitaxy/$sessionId"}`
		if copies[0] != want {
			t.Fatalf("Desktop copy of %s = %s, want Segment twin plus suffix %s", expected.EventName, copies[0], want)
		}
		copyKeys := orderedJSONKeys(t, []byte(copies[0]))
		if strings.Join(copyKeys[len(copyKeys)-len(expected.DualFireSuffixKeys):], ",") != strings.Join(expected.DualFireSuffixKeys, ",") {
			t.Fatalf("Desktop copy suffix of %s = %v, golden %v", expected.EventName, copyKeys, expected.DualFireSuffixKeys)
		}
		var properties map[string]any
		if errDecode := json.Unmarshal(twins[0], &properties); errDecode != nil {
			t.Fatal(errDecode)
		}
		if properties["billing_type"] != "stripe_subscription" || properties["app_version"] != strings.TrimSuffix(manager.bundle.DesktopVersion, ".0") {
			t.Fatalf("Segment %s derived values = %v", expected.EventName, properties)
		}
	}
	after := manager.Status().LiveEmitterCoverage
	executable := manager.executableEndpointEvents()
	for _, expected := range golden.Events {
		for _, role := range []string{segmentRole, manager.profile.EndpointRole} {
			if _, ok := executable[role+"\x00"+expected.EventName]; !ok {
				t.Fatalf("%s %q is not counted as executable", role, expected.EventName)
			}
		}
	}
	if after.LiveEndpointEventCount-before.LiveEndpointEventCount != 2*len(golden.Events) {
		t.Fatalf("coverage delta = %d endpoint events (before %d, after %d), want %d", after.LiveEndpointEventCount-before.LiveEndpointEventCount, before.LiveEndpointEventCount, after.LiveEndpointEventCount, 2*len(golden.Events))
	}
	t.Logf("names=%d endpoint_events=%d/%d gaps=%d (before: endpoint_events=%d)", after.LiveEventNameCount, after.LiveEndpointEventCount, after.ObservableEndpointEventCount, after.ObservableEndpointEventCount-after.LiveEndpointEventCount, before.LiveEndpointEventCount)
}

func TestRendererShellStateBoundariesRetainEvidenceAndHaveCompanionSources(t *testing.T) {
	golden := loadDesktopRendererInventoryGolden(t)
	observedFacts := make(map[string]string)
	for _, event := range claudeprofile.V140609ObservedExecutableEvents() {
		for _, role := range event.EndpointRoles {
			observedFacts[role+"\x00"+event.EventName] = event.Fact()
		}
	}
	for _, boundary := range golden.Boundaries {
		for _, role := range []string{segmentRole, "desktop-event-logging"} {
			registeredFact := ""
			for fact, name := range executableEvents[role] {
				if name == boundary.EventName {
					registeredFact = fact
					break
				}
			}
			if boundary.Executable {
				if registeredFact == "" || registeredFact != boundary.DualFireFact {
					t.Fatalf("executable boundary %q role %s = fact %q, want %q", boundary.EventName, role, registeredFact, boundary.DualFireFact)
				}
				continue
			}
			observedFact := observedFacts[role+"\x00"+boundary.EventName]
			if observedFact == "" {
				if registeredFact != "" {
					t.Fatalf("historical boundary %q unexpectedly registered for uncaptured role %s as %q", boundary.EventName, role, registeredFact)
				}
				continue
			}
			if registeredFact != observedFact {
				t.Fatalf("historical boundary %q role %s = fact %q, want companion fact %q", boundary.EventName, role, registeredFact, observedFact)
			}
		}
	}
}
