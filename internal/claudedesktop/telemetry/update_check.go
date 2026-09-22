package telemetry

import (
	"fmt"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func isUpdateCheckFact(fact string) bool {
	return fact == FactUpdateCheckStarted || fact == FactUpdateNotAvailable
}

// ObserveUpdateCheck records the automatic production MSIX startup check.
// The startup executor calls the first fact immediately before HEAD and the
// second only after a complete GET confirms the captured no-update response.
// These are application facts, not user sessions or inference requests.
func (m *Manager) ObserveUpdateCheck(auth *cliproxyauth.Auth, fact string) error {
	if !m.Enabled() {
		return nil
	}
	if !isUpdateCheckFact(fact) {
		return fmt.Errorf("unsupported update-check fact")
	}
	if m.ctx.Err() != nil {
		return m.ctx.Err()
	}
	worker, errWorker := m.workerForDelivery(auth, m.rendererDelivery)
	if errWorker != nil {
		return errWorker
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	if m.ctx.Err() != nil || m.freezeOnShutdown.Load() {
		return fmt.Errorf("update telemetry runtime is stopped")
	}
	key := worker.directory
	if m.updateChecks == nil {
		m.updateChecks = make(map[string]string)
	}
	state := m.updateChecks[key]
	if state == FactUpdateNotAvailable || (state == FactUpdateCheckStarted && fact == FactUpdateCheckStarted) {
		return nil
	}
	if fact == FactUpdateNotAvailable && state != FactUpdateCheckStarted {
		return fmt.Errorf("update outcome has no observed check")
	}
	metadata := map[string]any{
		"current_version": strings.TrimSuffix(m.bundle.DesktopVersion, ".0"),
		"update_channel":  "production", "is_manual": false,
	}
	if errEnqueue := worker.enqueueProjected(m.ctx, fact, "", "", metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	m.updateChecks[key] = fact
	return nil
}
