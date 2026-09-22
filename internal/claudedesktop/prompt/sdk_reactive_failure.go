package prompt

import (
	"regexp"
	"strconv"
	"strings"
)

type SDKReactiveFailure struct {
	Reason   string `json:"reason"`
	TokenGap *int64 `json:"token_gap,omitempty"`
}

var sdkReactiveTokenGap = regexp.MustCompile(`(?i)prompt is too long[^0-9]*([0-9]+)[\t-\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*tokens?[\t-\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*>[\t-\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*([0-9]+)`)
var sdkReactiveMediaPath = regexp.MustCompile(`messages[.\[]([0-9]+)[\].]+content[.\[]([0-9]+)[\].]+(?:tool_result[.\[]content[.\[]([0-9]+)[\].]+)?(image|document|pdf)`)

func sdkCapabilityRejected(text, code string) bool {
	prefix := "capability_rejected: " + code
	for from := 0; from < len(text); {
		index := strings.Index(text[from:], prefix)
		if index < 0 {
			return false
		}
		end := from + index + len(prefix)
		if end == len(text) {
			return true
		}
		next := text[end]
		if !(next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' || next >= '0' && next <= '9' || strings.ContainsRune("_:.-", rune(next))) {
			return true
		}
		from += index + 1
	}
	return false
}

// ClassifySDKReactiveFailure implements the reviewed PTL/media decision
// helpers for an observed API failure. It never stores or returns error text.
// Credits-boundary rescue requires a separate native feature-state fact and
// must not be enabled merely because an error contains a billing message.
func ClassifySDKReactiveFailure(text string) SDKReactiveFailure {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "input is too long for requested model") || sdkCapabilityRejected(text, "prompt_too_long") {
		result := SDKReactiveFailure{Reason: "prompt_too_long"}
		match := sdkReactiveTokenGap.FindStringSubmatch(text)
		if len(match) != 0 {
			actual, actualErr := strconv.ParseInt(match[1], 10, 64)
			limit, limitErr := strconv.ParseInt(match[2], 10, 64)
			// Beyond exact JS integers, native Number rounding is not an
			// evidence-backed token gap; leave the gap unknown.
			if actualErr == nil && limitErr == nil && actual <= 1<<53-1 && limit <= 1<<53-1 && actual > limit {
				gap := actual - limit
				result.TokenGap = &gap
			}
		}
		return result
	}
	if strings.Contains(text, "request_too_large") || sdkCapabilityRejected(text, "image_block") || sdkCapabilityRejected(text, "document_block") ||
		sdkCapabilityRejected(text, "media_budget") || sdkReactiveMediaPath.MatchString(text) {
		return SDKReactiveFailure{Reason: "media_too_large"}
	}
	for _, phrase := range []string{"could not process image", "image exceeds", "image dimensions exceed", "image does not match the provided media type",
		"image cannot be empty", "exceeds api limit", "images exceed the api limit", "unable to resize image", "unable to compress image", "image file is empty",
		"could not process pdf", "pdf pages", "the pdf specified was not valid", "the pdf specified is password protected", "pdf cannot be empty", "too much media"} {
		if strings.Contains(lower, phrase) {
			return SDKReactiveFailure{Reason: "media_too_large"}
		}
	}
	return SDKReactiveFailure{Reason: "error"}
}
