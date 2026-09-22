package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Renderer analytics dual delivery.
//
// The claude.ai web renderer hosted in the Desktop webview fires its
// `claudeai.*` product analytics twice: once through analytics.js to Segment
// (`track`) and once as a `ProductAnalyticsEvent` in the Desktop event-logging
// batch whose `properties` string carries the same Segment properties followed
// by `_dual_fire`, `anonymous_id`, `service_name` and `path`. The event names
// do not exist in the local Electron chunks (renderer origin), so the contract
// is pinned on the captured schema in
// testdata/desktop-telemetry-renderer-native.json (see
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-renderer-source.mjs).
//
// Only events the gateway already projects for Segment are mirrored; the copy
// never adds a property the Segment projection does not carry.
const (
	FactRendererMessageSubmitted = "renderer_message_submitted"
	FactRendererSessionTTFT      = "renderer_session_ttft"
)

// FactRendererIdentify is the Desktop event-logging copy (`$identify`) of the
// Segment `identify` call the renderer makes when the epitaxy shell loads.
const FactRendererIdentify = "renderer_identify"

const (
	desktopRendererEventType    = "ProductAnalyticsEvent"
	desktopRendererServiceName  = "claude_ai"
	desktopRendererPath         = "/epitaxy/$sessionId"
	desktopRendererIdentifyPath = "/epitaxy"
)

func init() {
	registerExecutableEvents("desktop-event-logging", map[string]string{
		FactRendererMessageSubmitted: "claudeai.code.message.submitted",
		FactRendererSessionTTFT:      "claudeai.code.session.ttft",
		FactRendererIdentify:         "$identify",
	})
}

// desktopRendererIdentifyProperties keeps the captured `$identify` key order.
type desktopRendererIdentifyProperties struct {
	AnonymousID string `json:"anonymous_id"`
	ServiceName string `json:"service_name"`
	Path        string `json:"path"`
}

// emitDesktopRendererIdentify delivers the Desktop `$identify` copy right after
// the Segment identify of the same activation was persisted. segmentPayload is
// the persisted Segment identify; its anonymousId is mirrored verbatim.
func (m *Manager) emitDesktopRendererIdentify(segmentWorker *accountWorker, segmentPayload []byte) {
	if m == nil || segmentWorker == nil || len(segmentPayload) == 0 {
		return
	}
	event, mapped := m.profile.Events[FactRendererIdentify]
	if !mapped || strings.TrimSpace(event.EventName) == "" {
		return
	}
	worker, errWorker := m.workerFor(segmentWorker.authSnapshot())
	if errWorker != nil {
		log.WithError(errWorker).Warn("claude desktop renderer telemetry: identify copy has no Desktop worker")
		return
	}
	properties, errProperties := json.Marshal(desktopRendererIdentifyProperties{
		AnonymousID: gjson.GetBytes(segmentPayload, "anonymousId").String(),
		ServiceName: desktopRendererServiceName,
		Path:        desktopRendererIdentifyPath,
	})
	if errProperties != nil {
		worker.recordQueueFailure(errProperties)
		return
	}
	eventID := uuid.New().String()
	payload, errMarshal := json.Marshal(desktopRendererEvent{
		EventType: desktopRendererEventType,
		EventData: desktopRendererEventData{
			EventName: event.EventName, EventID: eventID,
			EventTimestamp:   m.now().UTC().Format("2006-01-02T15:04:05.000Z"),
			AccountUUID:      worker.binding.AccountUUID,
			OrganizationUUID: worker.binding.OrganizationUUID,
			Properties:       string(properties),
		},
	})
	if errMarshal != nil {
		worker.recordQueueFailure(errMarshal)
		return
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version: 1, EventUUID: eventID, EndpointRole: m.profile.EndpointRole, CatalogFact: FactRendererIdentify, CatalogEvent: event.EventName,
		OccurredAt: m.now().UTC().Format(time.RFC3339Nano), Binding: worker.binding, SessionID: m.appSessionID,
		ClientRequestID: eventID, Payload: payload,
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: identify copy was not persisted")
		return
	}
	select {
	case worker.wake <- struct{}{}:
	default:
	}
}

// desktopRendererEventData keeps the captured event_data key order.
type desktopRendererEventData struct {
	EventName        string `json:"event_name"`
	EventID          string `json:"event_id"`
	EventTimestamp   string `json:"event_timestamp"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
	Properties       string `json:"properties"`
}

type desktopRendererEvent struct {
	EventType string                   `json:"event_type"`
	EventData desktopRendererEventData `json:"event_data"`
}

// desktopRendererDualFireProperties appends the captured dual-fire suffix to
// the Segment track properties object, preserving the Segment property bytes.
// The renderer reports the session route as its path.
func desktopRendererDualFireProperties(segmentPayload []byte) (string, error) {
	return desktopRendererDualFirePropertiesAt(segmentPayload, desktopRendererPath)
}

// desktopRendererDualFirePropertiesAt is the path-parameterised form: shell
// level renderer events (outside a session route) report `/epitaxy`.
func desktopRendererDualFirePropertiesAt(segmentPayload []byte, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = desktopRendererPath
	}
	properties := gjson.GetBytes(segmentPayload, "properties")
	if !properties.IsObject() {
		return "", fmt.Errorf("Segment track properties are unavailable for the Desktop dual-fire copy")
	}
	anonymousID, errAnonymous := json.Marshal(gjson.GetBytes(segmentPayload, "anonymousId").String())
	if errAnonymous != nil {
		return "", errAnonymous
	}
	raw := strings.TrimSpace(properties.Raw)
	body := strings.TrimSuffix(raw, "}")
	if body != "{" {
		body += ","
	}
	var suffix strings.Builder
	suffix.WriteString(body)
	suffix.WriteString(`"_dual_fire":true,"anonymous_id":`)
	suffix.Write(anonymousID)
	encodedPath, errPath := json.Marshal(path)
	if errPath != nil {
		return "", errPath
	}
	suffix.WriteString(`,"service_name":"` + desktopRendererServiceName + `","path":`)
	suffix.Write(encodedPath)
	suffix.WriteString(`}`)
	if !json.Valid([]byte(suffix.String())) {
		return "", fmt.Errorf("Desktop dual-fire properties are not valid JSON")
	}
	return suffix.String(), nil
}

func (m *Manager) projectDesktopRendererDualFire(worker *accountWorker, fact string, segmentPayload []byte) ([]byte, string, error) {
	return m.projectDesktopRendererDualFireAt(worker, fact, segmentPayload, desktopRendererPath)
}

func (m *Manager) projectDesktopRendererDualFireAt(worker *accountWorker, fact string, segmentPayload []byte, path string) ([]byte, string, error) {
	if worker == nil {
		return nil, "", fmt.Errorf("Desktop event-logging worker is unavailable")
	}
	event, ok := m.profile.Events[fact]
	if !ok || strings.TrimSpace(event.EventName) == "" {
		return nil, "", fmt.Errorf("Desktop renderer telemetry fact %q is not mapped", fact)
	}
	properties, errProperties := desktopRendererDualFirePropertiesAt(segmentPayload, path)
	if errProperties != nil {
		return nil, "", errProperties
	}
	encoded, errMarshal := json.Marshal(desktopRendererEvent{
		EventType: desktopRendererEventType,
		EventData: desktopRendererEventData{
			EventName:        event.EventName,
			EventID:          uuid.New().String(),
			EventTimestamp:   m.now().UTC().Format("2006-01-02T15:04:05.000Z"),
			AccountUUID:      worker.binding.AccountUUID,
			OrganizationUUID: worker.binding.OrganizationUUID,
			Properties:       properties,
		},
	})
	return encoded, event.EventName, errMarshal
}

// emitDesktopRendererDualFire delivers the Desktop event-logging copy of a
// Segment renderer event that was just projected for the same span moment.
// It is a no-op when the Segment projection failed or the Desktop renderer
// worker is absent, so the copy can never exist without its Segment twin.
func (m *Manager) emitDesktopRendererDualFire(ctx context.Context, span *RequestSpan, fact string, segmentPayload []byte, errSegment error) {
	_ = ctx
	if m == nil || span == nil || span.worker == nil || errSegment != nil || len(segmentPayload) == 0 {
		return
	}
	// A profile without the renderer dual-fire mapping simply does not emit
	// the copy; only projection or persistence failures count against the worker.
	if event, mapped := m.profile.Events[fact]; !mapped || strings.TrimSpace(event.EventName) == "" {
		return
	}
	worker := span.worker
	payload, eventName, errProject := m.projectDesktopRendererDualFire(worker, fact, segmentPayload)
	if errProject != nil {
		worker.recordQueueFailure(errProject)
		log.WithError(errProject).WithField("fact", fact).Warn("claude desktop renderer telemetry: dual-fire projection failed")
		return
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version:         1,
		EventUUID:       uuid.New().String(),
		EndpointRole:    m.profile.EndpointRole,
		CatalogFact:     fact,
		CatalogEvent:    eventName,
		OccurredAt:      m.now().UTC().Format(time.RFC3339Nano),
		Binding:         worker.binding,
		SessionID:       span.facts.SessionID,
		ClientRequestID: span.facts.ClientRequestID,
		Payload:         payload,
	}); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).WithField("fact", fact).Warn("claude desktop renderer telemetry: dual-fire event was not persisted")
	}
}
