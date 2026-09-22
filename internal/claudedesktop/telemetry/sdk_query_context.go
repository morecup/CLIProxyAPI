package telemetry

import (
	"encoding/json"
	"math/rand/v2"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

// Query-context facts of the pinned SDK (Claude Code 2.1.247, module
// _448.js; audit-sdk-telemetry-query-context-source.mjs, golden
// testdata/sdk-telemetry-query-context-native.json). Inside the attachment
// generator set y0s every generator runs through fi(label, generator), which
// times the awaited generator and, when Math.random()<0.05, emits
// tengu_attachment_compute_duration with the label, the elapsed
// milliseconds, the summed JSON.stringify length of the non-null results and
// the result count. The gateway runs one real generator, the
// deferred_tools_delta reminder, and ports the sample around it. Property
// order follows the native object literal; the native logger prepends
// subscription_type and cc_prompt_id (module _675.js).
const FactSDKAttachmentComputeDuration = "attachment_compute_duration"

const FactSDKDefaultCreditBetaStrip = "fallback_credit_beta_strip"

// AttachmentComputeSampleRate is the native Math.random()<0.05 threshold.
const AttachmentComputeSampleRate = 0.05

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKAttachmentComputeDuration: "tengu_attachment_compute_duration",
		FactSDKDefaultCreditBetaStrip:    "tengu_rotunda_pennant_strip",
	})
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKDefaultCreditBetaStrip: "tengu_rotunda_pennant_strip",
	})
}

type sdkDefaultCreditBetaStripMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	Shape            string `json:"shape"`
	Mode             string `json:"mode"`
	NonStreaming     bool   `json:"non_streaming"`
	QuerySource      string `json:"query_source"`
	StickyScope      string `json:"sticky_scope"`
}

// ObserveDefaultCreditBetaStrip is called only after the owned main request
// has actually removed the rejected descriptor and before its one retry.
// SDK 2.1.247 mo/xjn pins this exact field order and default-mode values.
func (s *RequestSpan) ObserveDefaultCreditBetaStrip(nonStreaming bool) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	role := s.facts.Role
	s.mu.Unlock()
	if role != claudeprofile.RoleMain {
		return
	}
	s.enqueueSDKFact(FactSDKDefaultCreditBetaStrip, func(subscription, promptID string) any {
		return sdkDefaultCreditBetaStripMetadata{
			SubscriptionType: subscription,
			PromptID:         promptID,
			Shape:            "credit_beta_header",
			Mode:             "none",
			NonStreaming:     nonStreaming,
			QuerySource:      "sdk",
			StickyScope:      "session",
		}
	})
}

// AttachmentComputeSample is one timed generator run: the native label, the
// measured duration and the results the generator returned (r), from which
// attachment_count is r.length and attachment_size_bytes the summed
// JSON.stringify length of the non-null entries.
type AttachmentComputeSample struct {
	Label    string
	Duration time.Duration
	// Attachments are the attachment objects the generator produced in the
	// native wire shape; a nil slice is the native empty result [].
	Attachments []json.RawMessage
}

// DeferredToolsDeltaAttachment is the native deferred_tools_delta
// attachment object (qhe: {type:"deferred_tools_delta",...XHr result}) in
// its pinned key order; without MCP servers addedLines equal addedNames and
// the MCP lists are empty.
type DeferredToolsDeltaAttachment struct {
	Type                string   `json:"type"`
	AddedNames          []string `json:"addedNames"`
	AddedLines          []string `json:"addedLines"`
	RemovedNames        []string `json:"removedNames"`
	ReaddedNames        []string `json:"readdedNames"`
	PendingMcpServers   []string `json:"pendingMcpServers"`
	NeedsAuthMcpServers []string `json:"needsAuthMcpServers"`
	FailedMcpServers    []string `json:"failedMcpServers"`
}

// NewDeferredToolsDeltaAttachment builds the wire attachment the gateway's
// reminder corresponds to from the names it announced as added and removed.
func NewDeferredToolsDeltaAttachment(added, removed []string) json.RawMessage {
	attachment := DeferredToolsDeltaAttachment{
		Type:                AttachmentTypeDeferredToolsDelta,
		AddedNames:          nonNilStrings(added),
		AddedLines:          nonNilStrings(added),
		RemovedNames:        nonNilStrings(removed),
		ReaddedNames:        []string{},
		PendingMcpServers:   []string{},
		NeedsAuthMcpServers: []string{},
		FailedMcpServers:    []string{},
	}
	encoded, err := json.Marshal(attachment)
	if err != nil {
		return nil
	}
	return encoded
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

type sdkAttachmentComputeDurationMetadata struct {
	SubscriptionType    string `json:"subscription_type,omitempty"`
	PromptID            string `json:"cc_prompt_id,omitempty"`
	Label               string `json:"label"`
	DurationMS          int64  `json:"duration_ms"`
	AttachmentSizeBytes int    `json:"attachment_size_bytes"`
	AttachmentCount     int    `json:"attachment_count"`
}

// AttachmentSizeBytes is the native reduce((i,a)=>i+le(a).length,0) over the
// non-null results: JSON.stringify length counts UTF-16 code units, which
// equals the byte length for the ASCII tool names the owned reminder lists.
func (s AttachmentComputeSample) AttachmentSizeBytes() int {
	total := 0
	for _, attachment := range s.Attachments {
		if len(attachment) == 0 || string(attachment) == "null" {
			continue
		}
		total += len(attachment)
	}
	return total
}

func attachmentComputeDurationMetadata(subscription, promptID string, sample AttachmentComputeSample) sdkAttachmentComputeDurationMetadata {
	return sdkAttachmentComputeDurationMetadata{
		SubscriptionType:    subscription,
		PromptID:            promptID,
		Label:               sample.Label,
		DurationMS:          sample.Duration.Milliseconds(),
		AttachmentSizeBytes: sample.AttachmentSizeBytes(),
		AttachmentCount:     len(sample.Attachments),
	}
}

// AttachmentComputeSampled reports the native Math.random()<0.05 draw for
// one generator run; draw is the uniform [0,1) value.
func AttachmentComputeSampled(draw float64) bool {
	return draw < AttachmentComputeSampleRate
}

// ObserveAttachmentComputeDuration records tengu_attachment_compute_duration
// for one timed generator run of the request this span observes when the
// draw selects it. A nil draw uses the process random source.
func (s *RequestSpan) ObserveAttachmentComputeDuration(sample AttachmentComputeSample, draw func() float64) {
	if !s.Active() {
		return
	}
	if draw == nil {
		draw = rand.Float64
	}
	if !AttachmentComputeSampled(draw()) {
		return
	}
	s.enqueueSDKFact(FactSDKAttachmentComputeDuration, func(subscription, promptID string) any {
		return attachmentComputeDurationMetadata(subscription, promptID, sample)
	})
}
