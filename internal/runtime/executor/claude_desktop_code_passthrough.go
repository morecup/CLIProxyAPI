package executor

import (
	"net/http"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudewire "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/wire"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// isClaudeDesktopCodeWireRequest deliberately requires the stable first-party
// Code markers. They identify semantic Code input, not an outbound wire profile:
// the account executor still rebuilds headers, system, and managed history.
func isClaudeDesktopCodeWireRequest(headers http.Header) bool {
	return claudewire.IsCodeRequest(headers)
}

func claudeDesktopRequestRoleForWireClass(value string) claudeprofile.RequestRole {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "main":
		return claudeprofile.RoleMain
	case "title":
		return claudeprofile.RoleTitle
	case "light-helper":
		return claudeprofile.RoleLightHelper
	case "web-search-helper":
		return claudeprofile.RoleWebSearchHelper
	case "compaction", "compact":
		return claudeprofile.RoleCompaction
	case "subagent":
		return claudeprofile.RoleSubagent
	case "security-monitor":
		return claudeprofile.RoleSecurityMonitor
	case "count-tokens":
		return claudeprofile.RoleCountTokens
	default:
		return ""
	}
}

// claudeDesktopOwnedSessionUUID may use the verified Code session only as an
// internal continuity hint. It is namespaced into a program-owned UUID before
// history, identity metadata, or outbound headers use it; no caller header
// value is copied to the Anthropic request. Other request families retain the
// existing protocol-session derivation.
func claudeDesktopOwnedSessionUUID(headers http.Header, body []byte, codeWire bool) string {
	if !codeWire {
		return helps.ClaudeAgentSessionUUIDForRequest(headers, body, body, false)
	}
	if callerSession := strings.TrimSpace(claudeDesktopHeaderValue(headers, "x-claude-code-session-id")); callerSession != "" {
		internalHint := make(http.Header)
		internalHint.Set("X-Session-ID", callerSession)
		return helps.ClaudeAgentSessionUUIDForRequest(internalHint, nil, nil, false)
	}
	return helps.ClaudeAgentSessionUUIDForRequest(nil, body, body, false)
}

func claudeDesktopCodeHeaderAllowed(name string) bool {
	return claudewire.HeaderAllowed(name)
}

func claudeDesktopHeaderValue(headers http.Header, name string) string {
	return claudewire.HeaderValue(headers, name)
}
