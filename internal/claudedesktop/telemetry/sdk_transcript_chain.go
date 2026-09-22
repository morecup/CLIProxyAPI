package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSDKChainParentCycle    = "chain_parent_cycle"
	FactSDKChainTimestamp      = "chain_timestamp_fallback"
	FactSDKChainParallelResult = "chain_parallel_tr_recovered"
)

// SDKTranscriptChainObserver is created at the paused query's restoration
// boundary, after its worker model has been adopted. No input or helper has
// supplied a current prompt there. Native D calls are SDK log events; these
// names are absent from the pinned Datadog allowlist.
func (m *Manager) SDKTranscriptChainObserver(ctx context.Context, auth *cliproxyauth.Auth, session, query, model string, readConfig func(string) (json.RawMessage, error)) (func(claudeprompt.SDKTranscriptChainEvent), error) {
	if m == nil || !m.Enabled() {
		return nil, nil
	}
	if ctx == nil || ctx.Err() != nil || m.ctx.Err() != nil {
		return nil, fmt.Errorf("Claude Desktop transcript observer owner is unavailable")
	}
	for _, value := range []string{session, query} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil {
			return nil, fmt.Errorf("Claude Desktop transcript observer identity is invalid")
		}
	}
	return func(event claudeprompt.SDKTranscriptChainEvent) {
		if ctx.Err() != nil || m.ctx.Err() != nil {
			return
		}
		var worker *accountWorker
		acquireWorker := func() bool {
			if ctx.Err() != nil || m.ctx.Err() != nil {
				return false
			}
			if worker != nil {
				return true
			}
			var err error
			worker, err = m.workerForDelivery(auth, m.sdkDelivery)
			if err != nil {
				m.setEndpointState(m.sdkDelivery.endpointRole, "awaiting-sdk-prompt-facts", "SDK transcript event queue is unavailable")
				return false
			}
			return true
		}
		issue := func(cause error) {
			if !acquireWorker() {
				return
			}
			errStore := worker.setFactIssue(factIssueSDKTranscript, session, query, true)
			worker.recordQueueFailure(errors.Join(cause, errStore))
		}
		fact := ""
		metadata := map[string]any{}
		switch event.Kind {
		case claudeprompt.SDKChainParentCycle:
			fact = FactSDKChainParentCycle
		case claudeprompt.SDKChainTimestamp:
			fact = FactSDKChainTimestamp
		case claudeprompt.SDKChainParallelResult:
			fact = FactSDKChainParallelResult
			metadata["recovered_count"] = event.RecoveredCount
		}
		// Native sampling and the first-party kill gate precede queue access.
		// An intentional drop must not create a worker or report missing delivery
		// materials; configuration failures still use the durable issue ledger.
		if readConfig != nil {
			raw, err := readConfig("tengu_event_sampling_config")
			if err != nil {
				issue(err)
				return
			}
			rate := sdkTranscriptSampleRate(raw, event.Kind, rand.Float64)
			if rate != nil {
				if *rate == 0 {
					return
				}
				metadata["sample_rate"] = *rate
			}
			raw, err = readConfig("tengu_frond_boric")
			if err != nil {
				issue(err)
				return
			}
			var disabled map[string]json.RawMessage
			var firstParty bool
			if json.Unmarshal(raw, &disabled) == nil && json.Unmarshal(disabled["firstParty"], &firstParty) == nil && firstParty {
				return
			}
		}
		if ctx.Err() != nil || m.ctx.Err() != nil {
			return
		}
		if fact == "" || event.At.IsZero() || (fact == FactSDKChainParallelResult && event.RecoveredCount < 1) || (fact != FactSDKChainParallelResult && event.RecoveredCount != 0) {
			issue(fmt.Errorf("Claude Desktop transcript reconstruction event is invalid"))
			return
		}
		betas, known := m.sdkProfile.InputBetaHeader(model)
		if !known {
			issue(fmt.Errorf("Claude Desktop transcript observer model is unavailable"))
			return
		}
		if !acquireWorker() {
			return
		}
		metadata["subscription_type"] = subscriptionType(worker.authSnapshot())
		facts := RequestFacts{SessionID: session, Model: model, Betas: betas}
		if err := m.enqueueSDKEventAt(worker, fact, facts, metadata, event.At); err != nil {
			issue(err)
		}
		// A subsequent successful observation does not repair a lost occurrence.
	}, nil
}

// Native $7 draws only for a numeric rate strictly between zero and one.
// Missing, null, string, out-of-range and rate-one values mean no sampling
// metadata. A rejected draw returns zero rather than the configured rate.
func sdkTranscriptSampleRate(raw json.RawMessage, name string, random func() float64) *float64 {
	var configuration map[string]json.RawMessage
	var event map[string]json.RawMessage
	var rate *float64
	if json.Unmarshal(raw, &configuration) != nil || json.Unmarshal(configuration[name], &event) != nil ||
		json.Unmarshal(event["sample_rate"], &rate) != nil || rate == nil || *rate < 0 || *rate >= 1 {
		return nil
	}
	if *rate > 0 && random() >= *rate {
		*rate = 0
	}
	return rate
}
