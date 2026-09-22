package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSDKBridgePlaceholderUsed = "bridge_placeholder_used"
	FactSDKBridgeTeardown        = "bridge_teardown"
)

// SDKBridgeObserver binds to an existing account worker and an exact Host
// generation. Query cancellation is intentionally not a lifetime gate: native
// teardown emits after the query stops, before application telemetry closes.
// Only the control owner supplies model/prompt changes; helper spans do not.
func (m *Manager) SDKBridgeObserver(auth *cliproxyauth.Auth, owner claudecontrol.BridgeOwner) (claudecontrol.BridgeObserver, error) {
	if m == nil || !m.Enabled() {
		return nil, nil
	}
	if m.ctx.Err() != nil || owner.DesktopSessionID == "" || owner.QueryID == "" || owner.SDKSessionID == "" {
		return nil, fmt.Errorf("Claude Desktop SDK bridge owner is unavailable")
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		m.setEndpointState(m.sdkDelivery.endpointRole, "awaiting-sdk-bridge-facts", "SDK bridge event queue is unavailable")
		return nil, err
	}
	var mu sync.Mutex
	closed := false
	return func(event claudecontrol.BridgeEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if closed || m.ctx.Err() != nil {
			return nil
		}
		issue := func(cause error) error {
			errStore := worker.setFactIssue(factIssueSDKBridge, owner.SDKSessionID, owner.QueryID, true)
			worker.recordQueueFailure(errors.Join(cause, errStore))
			return errors.Join(cause, errStore)
		}
		if event.Owner != owner {
			return issue(fmt.Errorf("Claude Desktop SDK bridge event ownership mismatch"))
		}
		fact := ""
		switch event.Kind {
		case claudecontrol.BridgePlaceholderUsed:
			if event.Archive != nil {
				return issue(fmt.Errorf("Claude Desktop used-placeholder event has an archive outcome"))
			}
			fact = FactSDKBridgePlaceholderUsed
		case claudecontrol.BridgeTeardown:
			if event.Archive == nil || event.Archive.Status == "" {
				return issue(fmt.Errorf("Claude Desktop bridge teardown outcome is missing"))
			}
			fact, closed = FactSDKBridgeTeardown, true
		default:
			return issue(fmt.Errorf("Claude Desktop SDK bridge event is unsupported"))
		}
		betas, known := m.sdkProfile.InputBetaHeader(event.Model)
		if !known || event.At.IsZero() {
			return issue(fmt.Errorf("Claude Desktop SDK bridge lifecycle dimensions are unavailable"))
		}
		metadata := map[string]any{"v2": true, "subscription_type": subscriptionType(worker.authSnapshot())}
		if event.PromptID != "" {
			id, errID := uuid.Parse(event.PromptID)
			if errID != nil || id == uuid.Nil {
				return issue(fmt.Errorf("Claude Desktop SDK bridge prompt identity is invalid"))
			}
			metadata["cc_prompt_id"] = event.PromptID
		}
		if event.Archive != nil {
			encoded, _ := json.Marshal(event.Archive)
			if err := json.Unmarshal(encoded, &metadata); err != nil {
				return issue(err)
			}
		}
		facts := RequestFacts{SessionID: owner.SDKSessionID, Model: event.Model, Betas: betas, PromptID: event.PromptID}
		if err := m.enqueueSDKEventAt(worker, fact, facts, metadata, event.At); err != nil {
			return issue(err)
		}
		// A different event's success cannot erase a lost bridge occurrence.
		return nil
	}, nil
}
