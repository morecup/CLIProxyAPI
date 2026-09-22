package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Generic renderer analytics delivery shared by the renderer topic files.
//
// The claude.ai renderer hosted in the Desktop webview reports a product
// analytics call twice: as a Segment `track` (or `page`) through analytics.js
// and as the Desktop event-logging `ProductAnalyticsEvent` copy whose
// `properties` string repeats the Segment properties followed by the captured
// dual-fire suffix (see desktop_renderer.go). Topic files own the event
// contracts (fact constants, captured key order, value derivation) and call
// emitRendererTrack / emitRendererPage with the ordered properties object; a
// fact is delivered to a role only when that role's profile maps it, so the
// Desktop copy can never exist without its Segment twin.

// rendererCallKind selects the analytics.js call shape.
type rendererCallKind string

const (
	rendererCallTrack rendererCallKind = "track"
	rendererCallPage  rendererCallKind = "page"
)

// projectRendererCall builds the Segment payload for a renderer analytics call
// with the properties object embedded verbatim (key order preserved).
func (m *Manager) projectRendererCall(worker *accountWorker, kind rendererCallKind, fact string, properties json.RawMessage) ([]byte, string, error) {
	if m == nil || worker == nil {
		return nil, "", fmt.Errorf("Segment renderer worker is unavailable")
	}
	profile, ok := m.auxiliaryProfiles[segmentRole]
	if !ok {
		return nil, "", fmt.Errorf("Segment telemetry profile is unavailable")
	}
	event, mapped := profile.Events[fact]
	if !mapped || strings.TrimSpace(event.EventName) == "" {
		return nil, "", fmt.Errorf("Segment telemetry fact %q is not mapped", fact)
	}
	if len(properties) == 0 {
		properties = json.RawMessage(`{}`)
	}
	if !json.Valid(properties) || !strings.HasPrefix(strings.TrimSpace(string(properties)), "{") {
		return nil, "", fmt.Errorf("renderer analytics properties for %q are not a JSON object", fact)
	}
	auth := worker.authSnapshot()
	item := segmentCallItem{
		Timestamp:    m.now().UTC().Format(time.RFC3339Nano),
		Integrations: segmentIntegrations(),
		Type:         string(kind),
		Properties:   properties,
		Context:      m.segmentContext(auth, worker.binding),
		MessageID:    "ajs-next-" + uuid.New().String(),
		UserID:       worker.binding.AccountUUID,
		AnonymousID:  worker.binding.RuntimeUUID,
		Metadata:     segmentMetadata{Bundled: "[]"},
	}
	switch kind {
	case rendererCallPage:
		item.Name = event.EventName
	default:
		item.Event = event.EventName
	}
	encoded, errMarshal := json.Marshal(item)
	return encoded, event.EventName, errMarshal
}

// rendererCopyPath selects the `path` the Desktop copy reports: the session
// route (`/epitaxy/$sessionId`, default) or the shell route (`/epitaxy`).
const (
	rendererCopyPathSession = desktopRendererPath
	rendererCopyPathShell   = desktopRendererIdentifyPath
	rendererCopyPathNew     = "/new"
)

// emitRendererCall delivers the Segment call and, when the Desktop profile maps
// the same fact, its event-logging copy reporting the session route.
// segmentWorker/desktopWorker may be nil (role not enrolled); facts carry the
// session and client request ids.
func (m *Manager) emitRendererCall(ctx context.Context, kind rendererCallKind, segmentWorker, desktopWorker *accountWorker, facts RequestFacts, fact string, properties json.RawMessage) {
	m.emitRendererCallAt(ctx, kind, segmentWorker, desktopWorker, facts, fact, properties, rendererCopyPathSession)
}

// emitRendererCallAt is emitRendererCall with an explicit Desktop copy path.
func (m *Manager) emitRendererCallAt(ctx context.Context, kind rendererCallKind, segmentWorker, desktopWorker *accountWorker, facts RequestFacts, fact string, properties json.RawMessage, copyPath string) {
	if m == nil || segmentWorker == nil {
		return
	}
	if profile, ok := m.auxiliaryProfiles[segmentRole]; !ok || strings.TrimSpace(profile.Events[fact].EventName) == "" {
		// A profile without the mapping does not emit the call; nothing counts
		// against the worker.
		return
	}
	payload, eventName, errProject := m.projectRendererCall(segmentWorker, kind, fact, properties)
	m.enqueueAuxiliary(ctx, segmentWorker, fact, eventName, facts, payload, errProject)
	if errProject != nil || desktopWorker == nil {
		return
	}
	if event, mapped := m.profile.Events[fact]; !mapped || strings.TrimSpace(event.EventName) == "" {
		return
	}
	copyPayload, copyName, errCopy := m.projectDesktopRendererDualFireAt(desktopWorker, fact, payload, copyPath)
	if errCopy != nil {
		desktopWorker.recordQueueFailure(errCopy)
		log.WithError(errCopy).WithField("fact", fact).Warn("claude desktop renderer telemetry: dual-fire projection failed")
		return
	}
	if errEnqueue := desktopWorker.enqueue(Envelope{
		Version:         1,
		EventUUID:       uuid.New().String(),
		EndpointRole:    m.profile.EndpointRole,
		CatalogFact:     fact,
		CatalogEvent:    copyName,
		OccurredAt:      m.now().UTC().Format(time.RFC3339Nano),
		Binding:         desktopWorker.binding,
		SessionID:       facts.SessionID,
		ClientRequestID: facts.ClientRequestID,
		Payload:         copyPayload,
	}); errEnqueue != nil {
		desktopWorker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).WithField("fact", fact).Warn("claude desktop renderer telemetry: dual-fire event was not persisted")
		return
	}
	select {
	case desktopWorker.wake <- struct{}{}:
	default:
	}
}

// emitRendererTrack emits a renderer `track` call (plus Desktop copy) for the
// request span's account at the current span moment.
func (m *Manager) emitRendererTrack(ctx context.Context, span *RequestSpan, fact string, properties json.RawMessage) {
	if m == nil || span == nil {
		return
	}
	m.emitRendererCall(ctx, rendererCallTrack, span.auxiliaryWorkers[segmentRole], span.worker, span.facts, fact, properties)
}

// emitRendererPage emits a renderer `page` call (plus Desktop copy) for the
// request span's account; the profile event name is the page name.
func (m *Manager) emitRendererPage(ctx context.Context, span *RequestSpan, fact string, properties json.RawMessage) {
	if m == nil || span == nil {
		return
	}
	m.emitRendererCall(ctx, rendererCallPage, span.auxiliaryWorkers[segmentRole], span.worker, span.facts, fact, properties)
}

// rendererWorkers resolves the Segment and Desktop workers of an account for
// span-less renderer moments (session lifecycle outside a request).
func (m *Manager) rendererWorkers(auth *cliproxyauth.Auth) (segmentWorker, desktopWorker *accountWorker) {
	if m == nil || auth == nil {
		return nil, nil
	}
	if delivery, ok := m.auxiliaryDeliveries[segmentRole]; ok {
		if worker, err := m.workerForDelivery(auth, delivery); err == nil {
			segmentWorker = worker
		} else {
			log.WithError(err).Debug("claude desktop renderer telemetry: Segment runtime unavailable")
		}
	}
	if worker, err := m.workerFor(auth); err == nil {
		desktopWorker = worker
	} else {
		log.WithError(err).Debug("claude desktop renderer telemetry: Desktop runtime unavailable")
	}
	return segmentWorker, desktopWorker
}

// emitRendererTrackForAccount is the span-less variant of emitRendererTrack.
func (m *Manager) emitRendererTrackForAccount(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts, fact string, properties json.RawMessage) {
	segmentWorker, desktopWorker := m.rendererWorkers(auth)
	m.emitRendererCall(ctx, rendererCallTrack, segmentWorker, desktopWorker, facts, fact, properties)
}

// emitRendererPageForAccount is the span-less variant of emitRendererPage.
func (m *Manager) emitRendererPageForAccount(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts, fact string, properties json.RawMessage) {
	segmentWorker, desktopWorker := m.rendererWorkers(auth)
	m.emitRendererCall(ctx, rendererCallPage, segmentWorker, desktopWorker, facts, fact, properties)
}

// emitRendererTrackAt / emitRendererTrackForAccountAt emit a track whose
// Desktop copy reports an explicit path (rendererCopyPathShell for renderer
// shell events that fire outside a session route).
func (m *Manager) emitRendererTrackAt(ctx context.Context, span *RequestSpan, fact string, properties json.RawMessage, copyPath string) {
	if m == nil || span == nil {
		return
	}
	m.emitRendererCallAt(ctx, rendererCallTrack, span.auxiliaryWorkers[segmentRole], span.worker, span.facts, fact, properties, copyPath)
}

func (m *Manager) emitRendererTrackForAccountAt(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts, fact string, properties json.RawMessage, copyPath string) {
	segmentWorker, desktopWorker := m.rendererWorkers(auth)
	m.emitRendererCallAt(ctx, rendererCallTrack, segmentWorker, desktopWorker, facts, fact, properties, copyPath)
}

// emitRendererPageForAccountAt is emitRendererPageForAccount with an explicit
// Desktop copy path.
func (m *Manager) emitRendererPageForAccountAt(ctx context.Context, auth *cliproxyauth.Auth, facts RequestFacts, fact string, properties json.RawMessage, copyPath string) {
	segmentWorker, desktopWorker := m.rendererWorkers(auth)
	m.emitRendererCallAt(ctx, rendererCallPage, segmentWorker, desktopWorker, facts, fact, properties, copyPath)
}

// Renderer activation hooks run right after the Segment identify of an
// application activation was persisted (the renderer shell load), on the
// Segment worker of that account. They let topic files emit the shell-level
// renderer calls the captured bootstrap batch carries next to identify.
type rendererActivationHook func(ctx context.Context, m *Manager, segmentWorker *accountWorker)

var rendererActivationHooks []rendererActivationHook

func registerRendererActivationHook(hook rendererActivationHook) {
	if hook != nil {
		rendererActivationHooks = append(rendererActivationHooks, hook)
	}
}

// runRendererActivationHooks is the single insertion point used by
// auxiliary.go ensureSegmentIdentified. Hook failures never block activation.
func (m *Manager) runRendererActivationHooks(ctx context.Context, segmentWorker *accountWorker) {
	if m == nil || segmentWorker == nil {
		return
	}
	for _, hook := range rendererActivationHooks {
		hook(ctx, m, segmentWorker)
	}
}

// rendererActivationFacts is the session-less facts object activation hooks
// pass to the emitters: the application session id stands in for the record.
func (m *Manager) rendererActivationFacts() RequestFacts {
	if m == nil {
		return RequestFacts{}
	}
	return RequestFacts{SessionID: m.appSessionID, ClientRequestID: m.appSessionID}
}
