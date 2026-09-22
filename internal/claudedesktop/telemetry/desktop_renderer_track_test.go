package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// The probe facts are test-only: they are mapped in the profile but never
// registered as executable, so coverage counts stay untouched.
const (
	rendererTrackProbeFact = "renderer_track_probe"
	rendererPageProbeFact  = "renderer_page_probe"
)

func TestRendererTrackHelperDeliversOrderedSegmentCallAndDesktopCopy(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	for _, batch := range []*claudeprofile.TelemetryBatchProfile{
		&bundle.Telemetry.Batch, &bundle.SDKTelemetry.Batch, &bundle.AuxiliaryTelemetry.Segment.Batch,
		&bundle.AuxiliaryTelemetry.DatadogLogs.Batch, &bundle.AuxiliaryTelemetry.DatadogLogsBrowser.Batch,
		&bundle.AuxiliaryTelemetry.DatadogRUM.Batch, &bundle.AuxiliaryTelemetry.Sentry.Batch,
	} {
		batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		batch.JitterMinimum = 1
		batch.JitterMaximum = 1
	}
	bundle.AuxiliaryTelemetry.Segment.Events[rendererTrackProbeFact] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.transcript.open_settled"}
	bundle.Telemetry.Events[rendererTrackProbeFact] = claudeprofile.TelemetryEventProfile{EventName: "claudeai.epitaxy.transcript.open_settled"}
	// The page probe is mapped for Segment only: no Desktop copy may appear.
	bundle.AuxiliaryTelemetry.Segment.Events[rendererPageProbeFact] = claudeprofile.TelemetryEventProfile{EventName: "/login"}
	if errValidate := bundle.Validate(); errValidate != nil {
		t.Fatalf("validate test bundle: %v", errValidate)
	}
	clock := &testClock{now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	var captureMu sync.Mutex
	captured := make(map[string][]recordedRequest)
	rendererRole := bundle.Telemetry.EndpointRole
	sdkRole := bundle.SDKTelemetry.EndpointRole
	manager := NewManager(Options{
		StatePath: t.TempDir(), Bundle: bundle, Now: clock.Now, RandomFloat: func() float64 { return 0 },
		EndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) HTTPDoer {
			return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				captureMu.Lock()
				captured[role] = append(captured[role], recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
				captureMu.Unlock()
				proto, major, minor := "HTTP/2.0", 2, 0
				if role == sdkRole || role == datadogLogsRole || role == datadogLogsBrowserRole {
					proto, major, minor = "HTTP/1.1", 1, 1
				}
				return &http.Response{StatusCode: http.StatusOK, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
		},
	})
	t.Cleanup(manager.Close)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
	facts := testRequestFacts("77777777-7777-4777-8777-777777777777")
	span := manager.BeginRequest(context.Background(), auth, facts)
	if span.worker == nil || span.auxiliaryWorkers[segmentRole] == nil {
		t.Fatal("renderer workers were not bound to the span")
	}
	properties := json.RawMessage(`{"zeta_last":1,"alpha_first":"x","nested":{"b":2,"a":1}}`)
	manager.emitRendererTrack(context.Background(), span, rendererTrackProbeFact, properties)
	manager.emitRendererPage(context.Background(), span, rendererPageProbeFact, json.RawMessage(`{"path":"/login","title":"Claude"}`))
	// Unknown facts and malformed properties are ignored without touching the workers.
	manager.emitRendererTrack(context.Background(), span, "renderer_unmapped_probe", properties)
	manager.emitRendererTrack(context.Background(), span, rendererTrackProbeFact, json.RawMessage(`[1,2]`))
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	captureMu.Lock()
	rendererRequests := append([]recordedRequest(nil), captured[rendererRole]...)
	segmentRequests := append([]recordedRequest(nil), captured[segmentRole]...)
	captureMu.Unlock()

	var segmentTrackRaw, segmentPageRaw string
	anonymous := ""
	for _, request := range segmentRequests {
		// analytics.js batch order on the wire: {writeKey, batch, sentAt}.
		body := string(request.Body)
		if !strings.HasPrefix(body, `{"writeKey":`) || !strings.Contains(body, `,"batch":[`) || !strings.Contains(body, `],"sentAt":"`) {
			t.Fatalf("Segment batch envelope order is not native: %.80s", body)
		}
		var batch struct {
			Batch []json.RawMessage `json:"batch"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, item := range batch.Batch {
			raw := string(item)
			if !strings.HasPrefix(raw, `{"timestamp":`) || !strings.Contains(raw, `,"writeKey":"`) || !strings.HasSuffix(raw, `,"_metadata":{"bundled":"[]","unbundled":null,"bundledIds":null}}`) {
				t.Fatalf("Segment item order is not native (timestamp first, writeKey before _metadata): %.120s", raw)
			}
			if writeKeyAt, metadataAt := strings.Index(raw, `,"writeKey":"`), strings.LastIndex(raw, `,"_metadata":`); writeKeyAt < 0 || metadataAt < writeKeyAt || strings.Count(raw[writeKeyAt:metadataAt], `":"`) != 1 {
				t.Fatalf("writeKey is not immediately before _metadata: %.120s", raw)
			}
			var head struct {
				Type        string          `json:"type"`
				Event       string          `json:"event"`
				Name        string          `json:"name"`
				Properties  json.RawMessage `json:"properties"`
				AnonymousID string          `json:"anonymousId"`
			}
			if errHead := json.Unmarshal(item, &head); errHead != nil {
				t.Fatal(errHead)
			}
			switch {
			case head.Type == "track" && head.Event == "claudeai.epitaxy.transcript.open_settled":
				segmentTrackRaw = string(head.Properties)
				anonymous = head.AnonymousID
				if !strings.Contains(raw, `,"event":"claudeai.epitaxy.transcript.open_settled","type":"track","properties":{`) {
					t.Fatalf("track item key order is not native: %.160s", raw)
				}
			case head.Type == "page" && head.Name == "/login":
				segmentPageRaw = string(head.Properties)
				if !strings.Contains(raw, `,"type":"page","properties":{"path":"/login","title":"Claude"},"name":"/login","context":{`) {
					t.Fatalf("page item key order is not native: %.160s", raw)
				}
			}
		}
	}
	if segmentTrackRaw != string(properties) {
		t.Fatalf("Segment track properties lost their order or content: %s", segmentTrackRaw)
	}
	if segmentPageRaw != `{"path":"/login","title":"Claude"}` {
		t.Fatalf("Segment page properties = %s", segmentPageRaw)
	}
	if anonymous == "" {
		t.Fatal("Segment track carried no anonymousId")
	}

	copies := 0
	for _, request := range rendererRequests {
		var batch struct {
			Events []struct {
				EventType string `json:"event_type"`
				EventData struct {
					EventName  string `json:"event_name"`
					Properties string `json:"properties"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, event := range batch.Events {
			switch event.EventData.EventName {
			case "claudeai.epitaxy.transcript.open_settled":
				copies++
				want := strings.TrimSuffix(string(properties), "}") + `,"_dual_fire":true,"anonymous_id":"` + anonymous + `","service_name":"claude_ai","path":"/epitaxy/$sessionId"}`
				if event.EventType != desktopRendererEventType || event.EventData.Properties != want {
					t.Fatalf("Desktop copy = %s %s, want %s", event.EventType, event.EventData.Properties, want)
				}
			case "/login":
				t.Fatal("a Desktop copy was emitted for a fact the Desktop profile does not map")
			}
		}
	}
	if copies != 1 {
		t.Fatalf("Desktop copies = %d, want 1", copies)
	}
	if status := manager.Status(); status.LiveEmitterCoverage.LiveEndpointEventCount != manager.Status().LiveEmitterCoverage.LiveEndpointEventCount {
		t.Fatal("coverage snapshot is unstable")
	}
}
