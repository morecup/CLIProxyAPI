package helps

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// PeekClaudeDesktopErrorBody bounds recovery inspection without changing the
// caller-visible error bytes, headers, close ownership or read failure. The
// bound is local observation policy, not a native API limit.
func PeekClaudeDesktopErrorBody(response *http.Response, limit int64) ([]byte, bool) {
	if response == nil || response.Body == nil || limit <= 0 {
		return nil, false
	}
	original := response.Body
	body, err := io.ReadAll(io.LimitReader(original, limit+1))
	readers := []io.Reader{bytes.NewReader(body)}
	if err != nil {
		readers = append(readers, &claudeDesktopReplayReadError{err: err})
	}
	readers = append(readers, original)
	response.Body = &claudeDesktopReplayErrorBody{Reader: io.MultiReader(readers...), closer: original}
	return body, err == nil && int64(len(body)) <= limit
}

type claudeDesktopReplayErrorBody struct {
	io.Reader
	closer io.Closer
}

func (b *claudeDesktopReplayErrorBody) Close() error { return b.closer.Close() }

type claudeDesktopReplayReadError struct{ err error }

func (r *claudeDesktopReplayReadError) Read([]byte) (int, error) {
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

// This descriptor is pinned in SDK 2.1.247 (_675.js, fallback_credit).
const ClaudeDesktopFallbackCreditBeta = "fallback-credit-2026-06-01"

// CanStripDefaultClaudeDesktopCreditBeta limits the implementation to the
// reviewed no-fallback, no-credit, no-companion-header branch. Other native
// modes require their own owned routing/token state, not a generic 400 retry.
func CanStripDefaultClaudeDesktopCreditBeta(headers http.Header, body []byte) bool {
	if !gjson.ValidBytes(body) || gjson.GetBytes(body, "fallbacks").Exists() || gjson.GetBytes(body, "fallback_credit_token").Exists() {
		return false
	}
	found := false
	for key, values := range headers {
		if !strings.EqualFold(key, "anthropic-beta") {
			continue
		}
		for _, value := range values {
			for _, beta := range strings.Split(value, ",") {
				beta = strings.TrimSpace(beta)
				if strings.HasPrefix(beta, "server-side-fallback-") {
					return false
				}
				found = found || beta == ClaudeDesktopFallbackCreditBeta
			}
		}
	}
	return found
}

// ClaudeDesktopCreditBetaRejected matches xjn's credit_beta_header predicate
// on a structured API error. Token errors and unattributed 400s are excluded.
func ClaudeDesktopCreditBetaRejected(body []byte) bool {
	if !gjson.ValidBytes(body) || gjson.GetBytes(body, "type").String() != "error" ||
		gjson.GetBytes(body, "error.type").String() != "invalid_request_error" {
		return false
	}
	message := gjson.GetBytes(body, "error.message")
	if message.Type != gjson.String {
		return false
	}
	text := message.String()
	return strings.Contains(text, "fallback-credit-") && (strings.Contains(text, "anthropic-beta") || strings.Contains(text, "anthropic_beta"))
}

// StripClaudeDesktopFallbackCreditBeta preserves other header names/values
// and removes only the pinned descriptor, including noncanonical wire casing.
func StripClaudeDesktopFallbackCreditBeta(headers http.Header) {
	for key, values := range headers {
		if !strings.EqualFold(key, "anthropic-beta") {
			continue
		}
		keptValues := make([]string, 0, len(values))
		for _, value := range values {
			kept := make([]string, 0)
			for _, beta := range strings.Split(value, ",") {
				if strings.TrimSpace(beta) != ClaudeDesktopFallbackCreditBeta {
					kept = append(kept, beta)
				}
			}
			if len(kept) != 0 {
				keptValues = append(keptValues, strings.Join(kept, ","))
			}
		}
		if len(keptValues) == 0 {
			delete(headers, key)
		} else {
			headers[key] = keptValues
		}
	}
}
