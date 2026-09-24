// Package wire contains the stable portions of a first-party Claude Desktop
// Code request that the proxy can safely recognize and forward unchanged.
package wire

import (
	"net/http"
	"strings"
)

// IsCodeRequest reports whether headers identify an already-real Claude Code
// request emitted by Claude Desktop. Generic Anthropic callers deliberately do
// not qualify for transparent forwarding.
func IsCodeRequest(headers http.Header) bool {
	requestClass := strings.TrimSpace(HeaderValue(headers, "x-claude-code-request-class"))
	platform := strings.TrimSpace(HeaderValue(headers, "anthropic-client-platform"))
	userAgent := strings.ToLower(HeaderValue(headers, "user-agent"))
	return requestClass != "" && strings.EqualFold(platform, "desktop_app") && strings.Contains(userAgent, "claude-cli/")
}

// ForwardHeaders returns only the client identity and protocol headers that a
// real Code request may carry upstream. Authentication is intentionally not
// included: the account executor supplies the proxy-owned OAuth credential.
func ForwardHeaders(incoming http.Header) http.Header {
	forwarded := make(http.Header)
	for name, values := range incoming {
		if !HeaderAllowed(name) {
			continue
		}
		forwarded[name] = append([]string(nil), values...)
	}
	return forwarded
}

// HeaderAllowed reports whether a request header is part of the safe Code
// wire contract. Keep this list synchronized with the captured Desktop client,
// rather than forwarding arbitrary downstream proxy headers.
func HeaderAllowed(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "accept", "accept-encoding", "content-type", "user-agent", "connection",
		"anthropic-beta", "anthropic-client-platform", "anthropic-client-version",
		"anthropic-dangerous-direct-browser-access", "anthropic-version", "x-app",
		"x-cc-atis", "x-cc-compaction-request", "x-client-request-id", "x-claude-code-request-class", "x-claude-code-session-id":
		return true
	}
	return strings.HasPrefix(name, "x-stainless-") || strings.HasPrefix(name, "x-claude-code-")
}

// HeaderValue returns the first value for name without depending on Go's
// canonicalization. The Desktop transport intentionally preserves selected
// wire casing, so callers must also handle non-canonical map keys.
func HeaderValue(headers http.Header, name string) string {
	for candidate, values := range headers {
		if strings.EqualFold(candidate, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
