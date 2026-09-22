package telemetry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// registerExecutableEvents lets a topic file declare the production emitters
// it implements at init time. coverage.go keeps the core lifecycle list; the
// same rule applies to registered entries: a profile declaration alone is not
// an emitter, and every registered pair needs a delivery test in the topic's
// test file.
func registerExecutableEvents(role string, events map[string]string) {
	if executableEvents[role] == nil {
		executableEvents[role] = make(map[string]string, len(events))
	}
	for fact, name := range events {
		executableEvents[role][fact] = name
	}
}

// enqueueSDKFact records one SDK event against the request this span observes
// while the span is still open. The builder receives the native logger
// prefix inputs (subscription_type, cc_prompt_id) and returns the metadata
// object in native property order.
func (s *RequestSpan) enqueueSDKFact(fact string, build func(subscription, promptID string) any) {
	if !s.Active() || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	facts, finished := s.facts, s.finished
	s.mu.Unlock()
	if finished {
		return
	}
	betas, known := s.manager.sdkProfile.InputBetaHeader(facts.Model)
	if !known {
		log.WithFields(log.Fields{"model": facts.Model, "fact": fact}).Warn("claude desktop SDK telemetry: event dropped because the model has no profiled beta header")
		return
	}
	facts.Betas = betas
	metadata := build(subscriptionType(s.sdkWorker.authSnapshot()), facts.PromptID)
	if errEnqueue := s.manager.enqueueSDKEvent(s.sdkWorker, fact, facts, metadata); errEnqueue != nil {
		s.sdkWorker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).WithField("fact", fact).Warn("claude desktop SDK telemetry: event was not persisted")
	}
}

// recordSDKFact emits one SDK event outside a request span (between two owned
// requests, at tool execution or at session lifecycle points). session and
// model identify the owned query's SDK session; promptID may be empty.
func (m *Manager) recordSDKFact(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID, fact string, build func(subscription, promptID string) any) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if m.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop telemetry owner is unavailable")
	}
	session, model = strings.TrimSpace(session), strings.TrimSpace(model)
	if session == "" || model == "" {
		return fmt.Errorf("Claude Desktop SDK event %q identity is incomplete", fact)
	}
	betas, known := m.sdkProfile.InputBetaHeader(model)
	if !known {
		return fmt.Errorf("Claude Desktop SDK event %q model %q has no profiled beta header", fact, model)
	}
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		return errWorker
	}
	facts := RequestFacts{SessionID: session, Model: model, Betas: betas, PromptID: strings.TrimSpace(promptID)}
	metadata := build(subscriptionType(worker.authSnapshot()), facts.PromptID)
	if errEnqueue := m.enqueueSDKEvent(worker, fact, facts, metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

// RecordSDKFact is the exported span-less emitter for topic packages outside
// telemetry (executors, helps). fact must be registered by a topic file.
func (m *Manager) RecordSDKFact(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID, fact string, build func(subscription, promptID string) any) error {
	return m.recordSDKFact(ctx, auth, session, model, promptID, fact, build)
}

// sdkStartupEvent is an SDK session-start event registered by a topic file.
// Order places it relative to the core startup events (tengu_started = 100,
// tengu_init = 200, tengu_sdk_init_handshake = 300); equal orders keep
// registration order. Build returns the metadata in native property order
// and false when the native emission condition does not hold for the
// emulated session.
type sdkStartupEvent struct {
	Fact      string
	Order     int
	Build     func(m *Manager, subscription string) (any, bool)
	BuildMany func(m *Manager, subscription string) []any
}

var sdkStartupEvents []sdkStartupEvent

func registerSDKStartupEvent(event sdkStartupEvent) {
	sdkStartupEvents = append(sdkStartupEvents, event)
	sort.SliceStable(sdkStartupEvents, func(i, j int) bool { return sdkStartupEvents[i].Order < sdkStartupEvents[j].Order })
}

type sdkStartupEmission struct {
	fact     string
	order    int
	metadata any
}

// withRegisteredStartupEvents merges the registered startup events into the
// core sequence by order.
func (m *Manager) withRegisteredStartupEvents(core []sdkStartupEmission, subscription string) []sdkStartupEmission {
	merged := make([]sdkStartupEmission, 0, len(core)+len(sdkStartupEvents))
	merged = append(merged, core...)
	for _, event := range sdkStartupEvents {
		if event.Build == nil && event.BuildMany == nil {
			continue
		}
		// A registered startup event whose fact the loaded profile does not
		// declare is skipped instead of failing the whole session start; the
		// coverage report then shows it as a declaration gap.
		if profile, declared := m.sdkProfile.Events[event.Fact]; !declared || profile.EventName == "" {
			log.WithField("fact", event.Fact).Debug("claude desktop SDK telemetry: registered startup event is not declared by the profile; skipped")
			continue
		}
		if event.BuildMany != nil {
			for _, metadata := range event.BuildMany(m, subscription) {
				merged = append(merged, sdkStartupEmission{fact: event.Fact, order: event.Order, metadata: metadata})
			}
			continue
		}
		metadata, ok := event.Build(m, subscription)
		if ok {
			merged = append(merged, sdkStartupEmission{fact: event.Fact, order: event.Order, metadata: metadata})
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].order < merged[j].order })
	return merged
}
