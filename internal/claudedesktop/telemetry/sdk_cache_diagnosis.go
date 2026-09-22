package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
)

const FactSDKCacheDiagnosis = "prompt_cache_diagnosis_received"

type sdkCacheDiagnosisRequest struct {
	previousMessageID string
	oneHourTTL        bool
}

type sdkCacheDiagnosisMetadata struct {
	SubscriptionType  string `json:"subscription_type"`
	PromptID          string `json:"cc_prompt_id"`
	DiagnosisType     string `json:"diagnosisType"`
	Is1hCacheTTL      bool   `json:"is1hCacheTTL"`
	IsCowork          bool   `json:"isCowork"`
	Model             string `json:"model"`
	PreviousMessageID string `json:"previousMessageId"`
	QueryDepth        int    `json:"queryDepth"`
	QuerySource       string `json:"querySource"`
	RequestID         string `json:"requestId"`
	TokensMissed      int64  `json:"tokensMissed"`
}

func sdkCacheDiagnosisRequestFromBody(body []byte) sdkCacheDiagnosisRequest {
	result := sdkCacheDiagnosisRequest{}
	if value := gjson.GetBytes(body, "diagnostics.previous_message_id"); value.Type == gjson.String {
		if id := strings.TrimSpace(value.String()); sdkResponseIdentifier(id, "msg_") {
			result.previousMessageID = id
		}
	}
	inspectBlock := func(block gjson.Result) {
		if block.Get("cache_control.type").String() == "ephemeral" && block.Get("cache_control.ttl").String() == "1h" {
			result.oneHourTTL = true
		}
	}
	for _, key := range []string{"system", "tools"} {
		for _, block := range gjson.GetBytes(body, key).Array() {
			inspectBlock(block)
		}
	}
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		for _, block := range message.Get("content").Array() {
			inspectBlock(block)
		}
	}
	return result
}

// Diagnostics are read only from their protocol field. Separate role-specific
// observers may transiently decode title/compact text; arbitrary response or
// tool text is never interpreted as a diagnostic event.
func (s *RequestSpan) ObserveResponsePayload(payload []byte, streaming bool) {
	if streaming {
		for _, line := range bytes.Split(payload, []byte("\n")) {
			s.ObserveStreamLine(line)
		}
		return
	}
	s.observeSDKCompactionResponse(payload)
	if gjson.ValidBytes(payload) {
		s.observeSDKCacheDiagnosis(gjson.ParseBytes(payload))
		s.observeSDKTitleResponse(gjson.ParseBytes(payload))
	}
}

func (s *RequestSpan) ObserveStreamLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	s.observeSDKCompactionResponse(payload)
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	s.observeSDKTitleResponse(root)
	if root.Get("type").String() == "message_start" {
		s.observeSDKCacheDiagnosis(root.Get("message"))
	}
}

func (s *RequestSpan) observeSDKCacheDiagnosis(message gjson.Result) {
	if s == nil || s.manager == nil || s.sdkWorker == nil || message.Get("type").String() != "message" {
		return
	}
	diagnosis := message.Get("diagnostics.cache_miss_reason")
	if !diagnosis.Exists() || diagnosis.Type == gjson.Null {
		return
	}
	var value struct {
		Type         string `json:"type"`
		MissedTokens *int64 `json:"cache_missed_input_tokens"`
	}
	validValue := json.Unmarshal([]byte(diagnosis.Raw), &value) == nil && value.Type == "messages_changed" && value.MissedTokens != nil && *value.MissedTokens >= 0 && *value.MissedTokens <= 1<<53-1
	subscription := subscriptionType(s.sdkWorker.authSnapshot())
	s.mu.Lock()
	if s.finished || !s.requestObserved || !s.responseObserved || s.cacheDiagnosisRecorded || s.facts.Role != claudeprofile.RoleMain || s.responseStatus < 200 || s.responseStatus >= 300 {
		s.mu.Unlock()
		return
	}
	s.cacheDiagnosisRecorded = true
	facts, request, requestID := s.facts, s.cacheDiagnosisRequest, s.requestID
	if !validValue {
		s.mu.Unlock()
		s.retainResponseFactIssue(factIssueCacheDiagnosis, facts)
		return
	}
	_, depth := sdkQueryLineage(s.sdkWorker, facts)
	if !sdkResponseIdentifier(request.previousMessageID, "msg_") || !sdkResponseIdentifier(requestID, "req_") || depth == nil {
		s.mu.Unlock()
		s.retainResponseFactIssue(factIssueCacheDiagnosis, facts)
		return
	}
	s.cacheDiagnosis = &sdkCacheDiagnosisMetadata{
		SubscriptionType: subscription, PromptID: facts.PromptID,
		DiagnosisType: value.Type, Is1hCacheTTL: request.oneHourTTL, Model: facts.Model,
		PreviousMessageID: request.previousMessageID, QueryDepth: *depth, QuerySource: facts.QuerySource,
		RequestID: requestID, TokensMissed: *value.MissedTokens,
	}
	s.mu.Unlock()
}

func sdkResponseIdentifier(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 128 {
		return false
	}
	for _, ch := range value[len(prefix):] {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}
