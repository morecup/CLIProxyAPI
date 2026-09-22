package telemetry

// Startup-core facts of the pinned SDK (Claude Code 2.1.247). The native
// setup function (chunk-9rfmtz0x.js) validates the launch cwd through XR
// (_448.js), which logs tengu_shell_set_cwd {success:true} before the setup
// tail logs tengu_started; the cli action handler (_10.js) logs tengu_init
// through iy() and, in the same synchronous statement run, reports the
// permission context's shell allow rules through HZl/Pis as
// tengu_shell_allow_rules_at_init. The native logger (_675.js) prepends
// subscription_type; no prompt is active at session start, so cc_prompt_id
// is absent. Golden: testdata/sdk-telemetry-startup-core-native.json
// (audit-sdk-telemetry-startup-core-source.mjs).
const (
	FactSDKShellSetCwd           = "shell_set_cwd"
	FactSDKShellAllowRulesAtInit = "shell_allow_rules_at_init"
)

// Native startup order pinned by the audit: tengu_shell_set_cwd precedes
// tengu_started (100) inside setup; tengu_shell_allow_rules_at_init follows
// tengu_init (200) and precedes the SDK initialize handshake (300).
const (
	sdkStartupOrderShellSetCwd           = 90
	sdkStartupOrderShellAllowRulesAtInit = 250
)

// sdkShellSetCwdMetadata mirrors XR's D("tengu_shell_set_cwd",{success:!0}).
type sdkShellSetCwdMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	Success          bool   `json:"success"`
}

// sdkShellAllowRulesAtInitMetadata mirrors Pis(e) for a permission context
// without Bash/PowerShell allow rules: the per-category counters
// (<source>_<tool>_<category>) are only defined for non-empty categories, so
// the emulated session (tengu_init numAllowedTools=0, no settings rules)
// carries the total alone.
type sdkShellAllowRulesAtInitMetadata struct {
	SubscriptionType     string `json:"subscription_type,omitempty"`
	TotalShellAllowRules int    `json:"total_shell_allow_rules"`
}

func shellSetCwdMetadata(subscription string) sdkShellSetCwdMetadata {
	return sdkShellSetCwdMetadata{SubscriptionType: subscription, Success: true}
}

func shellAllowRulesAtInitMetadata(subscription string) sdkShellAllowRulesAtInitMetadata {
	return sdkShellAllowRulesAtInitMetadata{SubscriptionType: subscription, TotalShellAllowRules: 0}
}

// sdkStartupFactMapped reports whether the manager's SDK profile maps the
// fact. Both native conditions hold unconditionally for the emulated session;
// the profile mapping is the only gate, so a bundle that does not carry the
// fact (for example another topic's test bundle) skips the event instead of
// failing the whole session start.
func sdkStartupFactMapped(m *Manager, fact string) bool {
	if m == nil {
		return false
	}
	profile, ok := m.sdkProfile.Events[fact]
	return ok && profile.EventName != ""
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKShellSetCwd:           "tengu_shell_set_cwd",
		FactSDKShellAllowRulesAtInit: "tengu_shell_allow_rules_at_init",
	})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKShellSetCwd, Order: sdkStartupOrderShellSetCwd, Build: func(m *Manager, subscription string) (any, bool) {
		if !sdkStartupFactMapped(m, FactSDKShellSetCwd) {
			return nil, false
		}
		return shellSetCwdMetadata(subscription), true
	}})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKShellAllowRulesAtInit, Order: sdkStartupOrderShellAllowRulesAtInit, Build: func(m *Manager, subscription string) (any, bool) {
		if !sdkStartupFactMapped(m, FactSDKShellAllowRulesAtInit) {
			return nil, false
		}
		return shellAllowRulesAtInitMetadata(subscription), true
	}})
}
