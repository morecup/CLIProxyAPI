package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Resume / bridge facts of the pinned SDK (Claude Code 2.1.247), see
// testdata/sdk-telemetry-resume-bridge-native.json:
//   - tengu_bridge_repl_started (module _448.js) fires once per bridge session
//     right after the bridge credential is minted, before the transport
//     connects: {has_initial_messages, v2:true, expires_in_s,
//     inProtectedNamespace:false, ...MSr()}. ax() is the _837.js gate that
//     returns false on every host and MSr() spreads no keys because ISr()
//     yields {namespace: undefined, cluster: undefined}.
//   - tengu_session_resumed (module chunk-zhnz59d4.js, print / sdk_url lane)
//     fires after the resumed transcript is hydrated: {entrypoint:"print",
//     success:true, interruption_kind, resume_duration_ms}.
//
// The native logger (module _675.js) prepends subscription_type and
// cc_prompt_id (when a prompt is active) before the event's own keys.
const (
	FactSDKBridgeReplStarted = "bridge_repl_started"
	FactSDKSessionResumed    = "session_resumed"
)

const (
	// SessionResumedEntrypointPrint is the entrypoint of the print / sdk_url
	// lane the Desktop worker runs in.
	SessionResumedEntrypointPrint = "print"
	// SessionResumedInterruptionNone is the pinned fallback
	// turnInterruptionState?.kind ?? "none": the gateway adopts completed
	// history only, so no interrupted turn kind is ever produced.
	SessionResumedInterruptionNone = "none"
)

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKBridgeReplStarted: "tengu_bridge_repl_started",
		FactSDKSessionResumed:    "tengu_session_resumed",
	})
}

// BridgeReplStarted mirrors the native bridge start inputs: E (initial
// history rows handed to the bridge) and kr.expires_in of the minted bridge
// credential.
type BridgeReplStarted struct {
	HasInitialMessages bool
	ExpiresInS         int
}

// SessionResumed mirrors the native print-lane resume success inputs.
type SessionResumed struct {
	InterruptionKind string
	Duration         time.Duration
}

type sdkBridgeReplStartedMetadata struct {
	SubscriptionType     string `json:"subscription_type,omitempty"`
	PromptID             string `json:"cc_prompt_id,omitempty"`
	HasInitialMessages   bool   `json:"has_initial_messages"`
	V2                   bool   `json:"v2"`
	ExpiresInS           int    `json:"expires_in_s"`
	InProtectedNamespace bool   `json:"inProtectedNamespace"`
}

type sdkSessionResumedMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	Entrypoint       string `json:"entrypoint"`
	Success          bool   `json:"success"`
	InterruptionKind string `json:"interruption_kind"`
	ResumeDurationMS int64  `json:"resume_duration_ms"`
}

func bridgeReplStartedMetadata(subscription, promptID string, started BridgeReplStarted) sdkBridgeReplStartedMetadata {
	return sdkBridgeReplStartedMetadata{
		SubscriptionType:   subscription,
		PromptID:           promptID,
		HasInitialMessages: started.HasInitialMessages,
		V2:                 true,
		ExpiresInS:         started.ExpiresInS,
		// ax() => false on every host; MSr() contributes no keys.
		InProtectedNamespace: false,
	}
}

func sessionResumedMetadata(subscription, promptID string, resumed SessionResumed) sdkSessionResumedMetadata {
	kind := strings.TrimSpace(resumed.InterruptionKind)
	if kind == "" {
		kind = SessionResumedInterruptionNone
	}
	return sdkSessionResumedMetadata{
		SubscriptionType: subscription,
		PromptID:         promptID,
		Entrypoint:       SessionResumedEntrypointPrint,
		Success:          true,
		InterruptionKind: kind,
		// Math.round(performance.now()-m) of the lane start.
		ResumeDurationMS: int64(resumed.Duration.Round(time.Millisecond) / time.Millisecond),
	}
}

// SDKResumeBridgeObserver is the control-plane observer that also delivers
// tengu_bridge_repl_started. Started events carry the minted credential's
// expires_in and the real history state of the grant; every other kind is
// delegated to SDKBridgeObserver unchanged.
func (m *Manager) SDKResumeBridgeObserver(auth *cliproxyauth.Auth, owner claudecontrol.BridgeOwner) (claudecontrol.BridgeObserver, error) {
	base, err := m.SDKBridgeObserver(auth, owner)
	if err != nil || base == nil {
		return base, err
	}
	return func(event claudecontrol.BridgeEvent) error {
		if event.Kind != claudecontrol.BridgeStarted {
			return base(event)
		}
		if event.Owner != owner {
			return fmt.Errorf("Claude Desktop SDK bridge start ownership mismatch")
		}
		if event.ExpiresIn <= 0 || event.Archive != nil {
			return fmt.Errorf("Claude Desktop SDK bridge start dimensions are unavailable")
		}
		started := BridgeReplStarted{HasInitialMessages: event.HasInitialMessages, ExpiresInS: event.ExpiresIn}
		return m.recordSDKFact(m.ctx, auth, owner.SDKSessionID, event.Model, event.PromptID, FactSDKBridgeReplStarted, func(subscription, promptID string) any {
			return bridgeReplStartedMetadata(subscription, promptID, started)
		})
	}, nil
}

// RecordSDKSessionResumed emits tengu_session_resumed (print lane, success)
// once the owned session's completed history was adopted. session and model
// identify the owned query's SDK session; promptID may be empty because the
// resume lane runs before the first prompt of the process.
func (m *Manager) RecordSDKSessionResumed(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, resumed SessionResumed) error {
	if resumed.Duration < 0 {
		return fmt.Errorf("Claude Desktop SDK session resume duration is negative")
	}
	// Native lane order: j("tengu_resume_print",{}) opens the `if(r.resume)`
	// try block before the session lookup; tengu_session_resumed follows
	// once the transcript is hydrated (sdk_tool_lifecycle.go).
	if m.sdkFactDeclared(FactSDKResumePrint) {
		if err := m.RecordSDKResumePrint(ctx, auth, session, model, promptID); err != nil {
			return err
		}
	}
	return m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKSessionResumed, func(subscription, promptID string) any {
		return sessionResumedMetadata(subscription, promptID, resumed)
	})
}
