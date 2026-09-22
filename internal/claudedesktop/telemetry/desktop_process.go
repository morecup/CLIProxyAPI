package telemetry

import (
	"context"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Lane D1 (desktop-process): Desktop 1.40609 main-process events emitted
// around lifecycle moments the gateway already models. Native pins live in
// testdata/desktop-telemetry-process-native.json (produced by
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-process-source.mjs).

// FactDesktopBinaryResolved maps to desktop_ccd_binary_resolved: the CCD
// binary preflight resolved the pinned CLI before spawning it for a session.
const FactDesktopBinaryResolved = "desktop_binary_resolved"

// desktopBinaryResolutionRequiredVersion is the resolveHostBinary() literal
// for a build-pinned CLI that is verified on disk at the required version,
// which is the emulated steady state (bundle code_version).
const desktopBinaryResolutionRequiredVersion = "required_version"

// desktopBinaryResolvedMetadata mirrors the native builder key order:
// {resolution, resolved_version, required_version}.
type desktopBinaryResolvedMetadata struct {
	Resolution      string `json:"resolution"`
	ResolvedVersion string `json:"resolved_version"`
	RequiredVersion string `json:"required_version"`
}

func (m desktopBinaryResolvedMetadata) toMap() map[string]any {
	return map[string]any{
		"resolution":       m.Resolution,
		"resolved_version": m.ResolvedVersion,
		"required_version": m.RequiredVersion,
	}
}

// binaryResolvedMetadata builds the native payload for the emulated state:
// the pinned CLI (bundle code_version) resolves at its required version.
func (m *Manager) binaryResolvedMetadata() (desktopBinaryResolvedMetadata, bool) {
	if m == nil || m.bundle == nil {
		return desktopBinaryResolvedMetadata{}, false
	}
	version := strings.TrimSpace(m.bundle.CodeVersion)
	if version == "" {
		return desktopBinaryResolvedMetadata{}, false
	}
	return desktopBinaryResolvedMetadata{
		Resolution:      desktopBinaryResolutionRequiredVersion,
		ResolvedVersion: version,
		RequiredVersion: version,
	}, true
}

// desktopSessionStartHook runs on the renderer worker immediately before the
// first-turn desktop_ccd_session_initialized event of a session is enqueued,
// which is where the native CCD binary preflight and spawn happen.
type desktopSessionStartHook func(ctx context.Context, w *accountWorker, facts RequestFacts)

var desktopSessionStartHooks []desktopSessionStartHook

func registerDesktopSessionStartHook(hook desktopSessionStartHook) {
	if hook != nil {
		desktopSessionStartHooks = append(desktopSessionStartHooks, hook)
	}
}

// runDesktopSessionStartHooks is the single insertion point used by
// store.go ensureSessionInitialized. Hook failures never block the session.
func (w *accountWorker) runDesktopSessionStartHooks(ctx context.Context, facts RequestFacts) {
	if w == nil {
		return
	}
	for _, hook := range desktopSessionStartHooks {
		hook(ctx, w, facts)
	}
}

func emitDesktopBinaryResolved(ctx context.Context, w *accountWorker, facts RequestFacts) {
	if w == nil || w.manager == nil {
		return
	}
	if _, declared := w.manager.profile.Events[FactDesktopBinaryResolved]; !declared {
		return
	}
	metadata, ok := w.manager.binaryResolvedMetadata()
	if !ok {
		return
	}
	if errEnqueue := w.enqueueProjected(ctx, FactDesktopBinaryResolved, facts.SessionID, facts.ClientRequestID, metadata.toMap()); errEnqueue != nil {
		w.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop renderer telemetry: binary-resolved event was not persisted")
	}
}

func init() {
	registerExecutableEvents("desktop-event-logging", map[string]string{
		FactDesktopBinaryResolved: "desktop_ccd_binary_resolved",
	})
	registerDesktopSessionStartHook(emitDesktopBinaryResolved)
}
