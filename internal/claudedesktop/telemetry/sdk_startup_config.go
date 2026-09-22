package telemetry

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
)

// Startup configuration facts of the pinned SDK (Claude Code 2.1.247) that
// fire once per headless SDK process regardless of the prompt. Property names
// and their order follow the native object literals; the native logger
// (module _675.js) prepends subscription_type. The emulated environment is
// the one fixed by the tengu_init metadata in sdk.go: no plugins, no
// marketplaces, no skills directories, no CLAUDE.md files, no MCP servers.
//
//   - tengu_plugin_skills_dir_loaded (_448.js, skills-as-plugins scan)
//     fires from the plugin loader before tengu_init: count/user_count/
//     project_count/project_suppressed_count/error_count are all 0 when
//     neither ~/.claude/skills nor ./.claude/skills exists.
//   - tengu_headless_plugin_install (chunk-zhnz59d4.js installPluginsForHeadless)
//     fires in the print flow after tengu_init in a finally block; with no
//     marketplaces declared marketplaces_installed and delisted_count stay 0.
//   - tengu_claudemd__initial_load (_448.js memory-file load) fires once per
//     session (hasLoggedInitialLoad latch) at the first memory load; with no
//     memory files every count is 0 and the load duration is 0 ms.
const (
	FactSDKPluginSkillsDirLoaded  = "plugin_skills_dir_loaded"
	FactSDKHeadlessPluginInstall  = "headless_plugin_install"
	FactSDKClaudeMDInitialLoad    = "claudemd_initial_load"
	FactSDKClaudeAIMCPEligibility = "claudeai_mcp_eligibility"
)

// Native startup positions relative to tengu_started(100)/tengu_init(200)/
// tengu_sdk_init_handshake(300). The claude.ai connector eligibility check
// (_448.js Uns) runs while MCP configs are gathered, before tengu_init reads
// the has_mcp_* inventory.
const (
	sdkStartupOrderPluginSkillsDirLoaded  = 150
	sdkStartupOrderClaudeAIMCPEligibility = 160
	sdkStartupOrderHeadlessPluginInstall  = 210
	sdkStartupOrderClaudeMDInitialLoad    = 400
)

// ClaudeAIMCPEligibilityMissingScope is the native state when the OAuth
// token lacks the user:mcp_servers scope: the gateway enrolls Claude Desktop
// accounts with the inference-only scope (claudedesktop.OAuthScope), so with
// ENABLE_CLAUDEAI_MCP_SERVERS unset, no disableClaudeAiConnectors setting, no
// safe mode, a first-party host and no API-key precedence the check stops at
// the scope gate without any network fetch.
const ClaudeAIMCPEligibilityMissingScope = "missing_scope"

const claudeAIMCPServersScope = "user:mcp_servers"

type sdkClaudeAIMCPEligibilityMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	State            string `json:"state"`
}

// claudeAIMCPEligibilityMetadata evaluates the native scope gate over the
// enrolled OAuth scope string (space separated). It returns false when the
// scope grants user:mcp_servers: the native state would then depend on a live
// /v1/mcp_servers fetch the gateway does not perform.
func claudeAIMCPEligibilityMetadata(subscription, scope string) (sdkClaudeAIMCPEligibilityMetadata, bool) {
	for _, granted := range strings.Fields(scope) {
		if granted == claudeAIMCPServersScope {
			return sdkClaudeAIMCPEligibilityMetadata{}, false
		}
	}
	return sdkClaudeAIMCPEligibilityMetadata{SubscriptionType: subscription, State: ClaudeAIMCPEligibilityMissingScope}, true
}

type sdkPluginSkillsDirLoadedMetadata struct {
	SubscriptionType       string `json:"subscription_type,omitempty"`
	Count                  int    `json:"count"`
	UserCount              int    `json:"user_count"`
	ProjectCount           int    `json:"project_count"`
	ProjectSuppressedCount int    `json:"project_suppressed_count"`
	ErrorCount             int    `json:"error_count"`
}

type sdkHeadlessPluginInstallMetadata struct {
	SubscriptionType      string `json:"subscription_type,omitempty"`
	MarketplacesInstalled int    `json:"marketplaces_installed"`
	DelistedCount         int    `json:"delisted_count"`
}

type sdkClaudeMDInitialLoadMetadata struct {
	SubscriptionType   string `json:"subscription_type,omitempty"`
	FileCount          int    `json:"file_count"`
	TotalContentLength int    `json:"total_content_length"`
	UserCount          int    `json:"user_count"`
	ProjectCount       int    `json:"project_count"`
	LocalCount         int    `json:"local_count"`
	ManagedCount       int    `json:"managed_count"`
	AutoMemCount       int    `json:"automem_count"`
	DurationMS         int64  `json:"duration_ms"`
}

// pluginSkillsDirLoadedMetadata is the native payload for a session without
// skills directories.
func pluginSkillsDirLoadedMetadata(subscription string) sdkPluginSkillsDirLoadedMetadata {
	return sdkPluginSkillsDirLoadedMetadata{SubscriptionType: subscription}
}

// headlessPluginInstallMetadata is the native payload for a session without
// declared marketplaces.
func headlessPluginInstallMetadata(subscription string) sdkHeadlessPluginInstallMetadata {
	return sdkHeadlessPluginInstallMetadata{SubscriptionType: subscription}
}

// claudeMDInitialLoadMetadata is the native payload for a session without
// memory files.
func claudeMDInitialLoadMetadata(subscription string) sdkClaudeMDInitialLoadMetadata {
	return sdkClaudeMDInitialLoadMetadata{SubscriptionType: subscription}
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKPluginSkillsDirLoaded:  "tengu_plugin_skills_dir_loaded",
		FactSDKHeadlessPluginInstall:  "tengu_headless_plugin_install",
		FactSDKClaudeMDInitialLoad:    "tengu_claudemd__initial_load",
		FactSDKClaudeAIMCPEligibility: "tengu_claudeai_mcp_eligibility",
	})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKPluginSkillsDirLoaded, Order: sdkStartupOrderPluginSkillsDirLoaded, Build: func(_ *Manager, subscription string) (any, bool) {
		return pluginSkillsDirLoadedMetadata(subscription), true
	}})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKClaudeAIMCPEligibility, Order: sdkStartupOrderClaudeAIMCPEligibility, Build: func(_ *Manager, subscription string) (any, bool) {
		metadata, ok := claudeAIMCPEligibilityMetadata(subscription, claudedesktop.OAuthScope)
		if !ok {
			return nil, false
		}
		return metadata, true
	}})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKHeadlessPluginInstall, Order: sdkStartupOrderHeadlessPluginInstall, Build: func(_ *Manager, subscription string) (any, bool) {
		return headlessPluginInstallMetadata(subscription), true
	}})
	registerSDKStartupEvent(sdkStartupEvent{Fact: FactSDKClaudeMDInitialLoad, Order: sdkStartupOrderClaudeMDInitialLoad, Build: func(_ *Manager, subscription string) (any, bool) {
		return claudeMDInitialLoadMetadata(subscription), true
	}})
}
