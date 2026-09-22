package telemetry

import (
	"net/http"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	log "github.com/sirupsen/logrus"
)

const FactSDKFastModeOverageRejected = "fast_mode_overage_rejected"

type sdkFastModeOverageMetadata struct {
	SubscriptionType      string `json:"subscription_type"`
	PromptID              string `json:"cc_prompt_id"`
	OverageDisabledReason string `json:"overage_disabled_reason"`
}

// ObserveHTTPResponse records response-header facts independently of a retry
// decision. Ordinary successful responses can carry the same overage header;
// only the captured main fast-request 429 branch represents this rejection.
func (s *RequestSpan) ObserveHTTPResponse(status int, headers http.Header) {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	if s.finished || !s.requestObserved || s.responseObserved {
		s.mu.Unlock()
		return
	}
	s.responseObserved = true
	s.responseStatus = status
	for name, values := range headers {
		if strings.EqualFold(name, "request-id") && len(values) == 1 {
			s.requestID = strings.TrimSpace(values[0])
			break
		}
	}
	facts := s.facts
	s.mu.Unlock()
	if facts.Role != claudeprofile.RoleMain || !facts.FastMode || status != http.StatusTooManyRequests {
		return
	}
	// This version has evidence for this reason only. Do not copy arbitrary
	// header text, or invent the reason from an error message or account plan.
	reason := ""
	for name, values := range headers {
		if strings.EqualFold(name, "anthropic-ratelimit-unified-overage-disabled-reason") && len(values) == 1 {
			reason = strings.TrimSpace(values[0])
			break
		}
	}
	if reason != "org_level_disabled" {
		if reason != "" {
			s.retainResponseFactIssue(factIssueFastOverage, facts)
		}
		return
	}
	if errEnqueue := s.manager.enqueueSDKEvent(s.sdkWorker, FactSDKFastModeOverageRejected, facts, sdkFastModeOverageMetadata{
		SubscriptionType: subscriptionType(s.sdkWorker.authSnapshot()), PromptID: facts.PromptID,
		OverageDisabledReason: reason,
	}); errEnqueue != nil {
		s.sdkWorker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop SDK telemetry: fast-mode rejection event was not persisted")
	}
}
