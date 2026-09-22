package telemetry

// Startup-session facts of the pinned SDK (Claude Code 2.1.247), lane W2.
// Golden: testdata/sdk-telemetry-startup-session-native.json
// (audit-sdk-telemetry-startup-session-source.mjs).
//
//   - tengu_concurrent_sessions (_10.js cli action handler): after the
//     allow-rule report, Cd(a).then((d)=>{if(d>=2)M("tengu_concurrent_sessions",
//     {num_sessions:d})}). Cd = GL (_675.js) counts the <pid>.json records of
//     the per-user session registry for process.pid and every live pid, so the
//     value is "this process plus every other live Claude Code process". The
//     gateway's counterpart of one Claude Code process is one telemetry
//     Manager (one appSessionID, one startup sequence); the package registry
//     activeManagers lists every open Manager of this gateway process, which
//     is every emulated SDK process on this host.
//   - tengu_timer {event:"startup"} (chunk-zhnz59d4.js runHeadless DJ): logged
//     right after the runHeadless_entry checkpoint with
//     durationMs=Math.round(process.uptime()*1000), mcpNonBlocking=wa() (the
//     same gate the SDK initialize handshake reports as mcpNonBlocking),
//     mcpClientCount=l.configuredMcpServerCount and resumed=!!(l.resume||
//     l.continue). The emulated process is spawned when the Manager starts
//     (startedAt) with a fresh application session, no configured MCP servers
//     (tengu_init mcpClientCount 0, handshake mcp_client_count 0) and no
//     --resume/--continue, so durationMs is the Manager's own uptime at the
//     emission and resumed is false. The plugins_init variant of tengu_timer
//     times the native plugin initialization (Ri/Ti), which the gateway does
//     not perform; it stays a boundary.
//
// The native logger (_675.js) prepends subscription_type; no prompt is active
// at session start, so cc_prompt_id is absent.
const (
	FactSDKConcurrentSessions = "concurrent_sessions"
	FactSDKTimer              = "timer"
)

// Native startup order pinned by the audit: the concurrent-session count is
// reported after tengu_shell_allow_rules_at_init (250); the headless startup
// timer is logged at the runHeadless entry, which precedes the SDK initialize
// handshake (300) inside the same module.
const (
	sdkStartupOrderConcurrentSessions = 260
	sdkStartupOrderTimerStartup       = 270
)

// sdkTimerStartupEvent is the native event label rt("startup").
const sdkTimerStartupEvent = "startup"

// sdkConcurrentSessionsMetadata mirrors M("tengu_concurrent_sessions",{num_sessions:d}).
type sdkConcurrentSessionsMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	NumSessions      int    `json:"num_sessions"`
}

// sdkTimerStartupMetadata mirrors j("tengu_timer",{event:rt("startup"),
// durationMs, mcpNonBlocking, mcpClientCount, resumed}) in native key order.
type sdkTimerStartupMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	Event            string `json:"event"`
	DurationMS       int64  `json:"durationMs"`
	MCPNonBlocking   bool   `json:"mcpNonBlocking"`
	MCPClientCount   int    `json:"mcpClientCount"`
	Resumed          bool   `json:"resumed"`
}

// concurrentSessionsMetadata applies the native gate: the event exists only
// when at least two live sessions were counted (the counting process included).
func concurrentSessionsMetadata(subscription string, numSessions int) (sdkConcurrentSessionsMetadata, bool) {
	if numSessions < 2 {
		return sdkConcurrentSessionsMetadata{}, false
	}
	return sdkConcurrentSessionsMetadata{SubscriptionType: subscription, NumSessions: numSessions}, true
}

func timerStartupMetadata(subscription string, uptimeMS int64, mcpClientCount int, resumed bool) sdkTimerStartupMetadata {
	if uptimeMS < 0 {
		uptimeMS = 0
	}
	return sdkTimerStartupMetadata{
		SubscriptionType: subscription,
		Event:            sdkTimerStartupEvent,
		DurationMS:       uptimeMS,
		MCPNonBlocking:   true,
		MCPClientCount:   mcpClientCount,
		Resumed:          resumed,
	}
}

// liveEmulatedSDKSessionCount counts the emulated SDK processes of this
// gateway process: every registered Manager that is enabled and has not been
// shut down. Each Manager owns one application session and one SDK startup
// sequence, which is the gateway's equivalent of one live <pid>.json record
// in the native per-user session registry. The counting Manager is included,
// exactly as GL counts the process's own record.
func liveEmulatedSDKSessionCount() int {
	activeManagers.RLock()
	defer activeManagers.RUnlock()
	count := 0
	for _, manager := range activeManagers.values {
		if manager == nil || !manager.Enabled() || manager.ctx == nil || manager.ctx.Err() != nil {
			continue
		}
		count++
	}
	return count
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKConcurrentSessions: "tengu_concurrent_sessions",
		FactSDKTimer:              "tengu_timer",
	})
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKTimer: "tengu_timer",
	})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKConcurrentSessions, Order: sdkStartupOrderConcurrentSessions, Build: func(m *Manager, subscription string) (any, bool) {
		if !sdkStartupFactMapped(m, FactSDKConcurrentSessions) {
			return nil, false
		}
		metadata, ok := concurrentSessionsMetadata(subscription, liveEmulatedSDKSessionCount())
		if !ok {
			return nil, false
		}
		return metadata, true
	}})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKTimer, Order: sdkStartupOrderTimerStartup, Build: func(m *Manager, subscription string) (any, bool) {
		if !sdkStartupFactMapped(m, FactSDKTimer) {
			return nil, false
		}
		// The emulated process starts with the Manager: uptime is measured
		// from startedAt, the same baseline the handshake's uptime_ms uses.
		return timerStartupMetadata(subscription, m.now().Sub(m.startedAt).Milliseconds(), 0, false), true
	}})
}
