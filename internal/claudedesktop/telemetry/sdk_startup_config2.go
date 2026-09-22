package telemetry

// Startup configuration facts of the pinned SDK (Claude Code 2.1.247), wave-3
// lane W3. tengu_headless_mcp_prewait (chunk-zhnz59d4.js gC) fires once per
// headless SDK process from the print flow: the first call site passes
// waitForDeferrable:!0 and skipTelemetry defaults to false, so the emit is
// unconditional. Property names and their order follow the native object
// literal; the native logger (_675.js) prepends subscription_type and lists
// the event in the datadog-logs mirror set. The emulated environment has no
// MCP servers (the pinned tengu_sdk_init_handshake carries mcp_client_count
// 0), so the Desktop spawn never pushes --mcp-config (deadline stays the
// 2000 ms default), never passes --sdk-url (localOnly false) and every
// pending/tool count and wait is 0. willDeferMcp is false because
// waitForDeferrable short-circuits the tool-search gates. mcpNonBlocking is
// the host connectNonBlocking() flag shared with the handshake payload.
const FactSDKHeadlessMCPPrewait = "headless_mcp_prewait"

// Native position: after tengu_headless_plugin_install (210) and the shell
// allow-rule init (250), before the tengu_sdk_init_handshake emit (300).
const sdkStartupOrderHeadlessMCPPrewait = 260

// sdkHeadlessMCPPrewaitDefaultDeadlineMS is the gC parameter default
// (`async function gC(e,t=2000,s={})`) used when tN yields no explicit
// --mcp-config deadline.
const sdkHeadlessMCPPrewaitDefaultDeadlineMS = 2000

type sdkHeadlessMCPPrewaitMetadata struct {
	SubscriptionType                    string `json:"subscription_type,omitempty"`
	LocalOnly                           bool   `json:"localOnly"`
	WillDeferMCP                        bool   `json:"willDeferMcp"`
	WaitForDeferrable                   bool   `json:"waitForDeferrable"`
	DeadlineMS                          int    `json:"deadlineMs"`
	PendingBefore                       int    `json:"pendingBefore"`
	PendingWaitedBefore                 int    `json:"pendingWaitedBefore"`
	ToolsBefore                         int    `json:"toolsBefore"`
	WaitedMS                            int64  `json:"waitedMs"`
	PermissionPromptServerPendingBefore bool   `json:"permissionPromptServerPendingBefore"`
	PermissionPromptWaitedMS            int64  `json:"permissionPromptWaitedMs"`
	PendingAfter                        int    `json:"pendingAfter"`
	PendingWaitedAfter                  int    `json:"pendingWaitedAfter"`
	PermissionPromptServerPendingAfter  bool   `json:"permissionPromptServerPendingAfter"`
	ToolsAfter                          int    `json:"toolsAfter"`
	MCPNonBlocking                      bool   `json:"mcpNonBlocking"`
}

// headlessMCPPrewaitMetadata is the native payload for a session without MCP
// servers; mcpNonBlocking mirrors the handshake's connectNonBlocking() flag.
func headlessMCPPrewaitMetadata(subscription string, mcpNonBlocking bool) sdkHeadlessMCPPrewaitMetadata {
	return sdkHeadlessMCPPrewaitMetadata{
		SubscriptionType:  subscription,
		WaitForDeferrable: true,
		DeadlineMS:        sdkHeadlessMCPPrewaitDefaultDeadlineMS,
		MCPNonBlocking:    mcpNonBlocking,
	}
}

// sdkHandshakeMCPNonBlocking is the connectNonBlocking() value the core
// startup sequence reports in tengu_sdk_init_handshake (sdk.go).
const sdkHandshakeMCPNonBlocking = true

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKHeadlessMCPPrewait: "tengu_headless_mcp_prewait",
	})
	// The datadog-logs mirror (_675.js known-event set): startup emissions go
	// through enqueueSDKEventAt, which mirrors every mapped fact to the
	// datadog-logs worker (sdk_startup_mirrors_test.go proves the delivery).
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKHeadlessMCPPrewait: "tengu_headless_mcp_prewait",
	})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKHeadlessMCPPrewait, Order: sdkStartupOrderHeadlessMCPPrewait, Build: func(_ *Manager, subscription string) (any, bool) {
		return headlessMCPPrewaitMetadata(subscription, sdkHandshakeMCPNonBlocking), true
	}})
}
