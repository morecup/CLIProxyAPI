package telemetry

// Query-build facts of the pinned SDK (Claude Code 2.1.247, module
// _448.js; audit-sdk-telemetry-query-build-source.mjs, golden
// testdata/sdk-telemetry-query-build-native.json). After an assistant turn
// ended with tool calls, the native query loop emits
// tengu_query_before_attachments once the tools ran, tengu_attachments when
// the attachment generators produced at least one attachment, then
// tengu_query_after_attachments, and the next request build emits
// tengu_api_before_normalize after the tool-search decision and before
// tengu_api_query. Property names and their order follow the native object
// literals; the native logger prepends subscription_type and cc_prompt_id
// (module _675.js).
const (
	FactSDKQueryBeforeAttachments = "query_before_attachments"
	FactSDKAttachments            = "attachments"
	FactSDKQueryAfterAttachments  = "query_after_attachments"
	FactSDKAPIBeforeNormalize     = "api_before_normalize"
)

// Native attachment type of the deferred-tools reminder (the only attachment
// the owned request build produces today).
const AttachmentTypeDeferredToolsDelta = "deferred_tools_delta"

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKQueryBeforeAttachments: "tengu_query_before_attachments",
		FactSDKAttachments:            "tengu_attachments",
		FactSDKQueryAfterAttachments:  "tengu_query_after_attachments",
		FactSDKAPIBeforeNormalize:     "tengu_api_before_normalize",
	})
}

// QueryLoopAttachments mirrors the native loop counters of one continuation
// iteration: Fe.length (internal rows before the assistant turn), We.length
// (internal assistant rows, one per streamed content block), jt.length
// before attachments (one internal user row per tool_result), the
// attachment types produced in order, and the edited_text_file count among
// them. Lineage (queryChainId/queryDepth) comes from the span's facts.
type QueryLoopAttachments struct {
	MessagesForQueryCount     int
	AssistantMessagesCount    int
	ToolResultsCount          int
	AttachmentTypes           []string
	FileChangeAttachmentCount int
	// ComputeDuration, when set, emits the sampled
	// tengu_attachment_compute_duration of this iteration's generator run at
	// its native position: inside y0s, after tengu_query_before_attachments
	// and before tengu_attachments.
	ComputeDuration func()
}

type sdkQueryBeforeAttachmentsMetadata struct {
	SubscriptionType       string `json:"subscription_type,omitempty"`
	PromptID               string `json:"cc_prompt_id,omitempty"`
	MessagesForQueryCount  int    `json:"messagesForQueryCount"`
	AssistantMessagesCount int    `json:"assistantMessagesCount"`
	ToolResultsCount       int    `json:"toolResultsCount"`
	QueryChainID           string `json:"queryChainId,omitempty"`
	QueryDepth             *int   `json:"queryDepth,omitempty"`
}

type sdkAttachmentsMetadata struct {
	SubscriptionType string   `json:"subscription_type,omitempty"`
	PromptID         string   `json:"cc_prompt_id,omitempty"`
	AttachmentTypes  []string `json:"attachment_types"`
}

type sdkQueryAfterAttachmentsMetadata struct {
	SubscriptionType          string `json:"subscription_type,omitempty"`
	PromptID                  string `json:"cc_prompt_id,omitempty"`
	TotalToolResultsCount     int    `json:"totalToolResultsCount"`
	FileChangeAttachmentCount int    `json:"fileChangeAttachmentCount"`
	QueryChainID              string `json:"queryChainId,omitempty"`
	QueryDepth                *int   `json:"queryDepth,omitempty"`
}

type sdkAPIBeforeNormalizeMetadata struct {
	SubscriptionType          string `json:"subscription_type,omitempty"`
	PromptID                  string `json:"cc_prompt_id,omitempty"`
	PreNormalizedMessageCount int    `json:"preNormalizedMessageCount"`
}

func queryBeforeAttachmentsMetadata(subscription, promptID string, loop QueryLoopAttachments, chainID string, depth *int) sdkQueryBeforeAttachmentsMetadata {
	return sdkQueryBeforeAttachmentsMetadata{
		SubscriptionType:       subscription,
		PromptID:               promptID,
		MessagesForQueryCount:  loop.MessagesForQueryCount,
		AssistantMessagesCount: loop.AssistantMessagesCount,
		ToolResultsCount:       loop.ToolResultsCount,
		QueryChainID:           chainID,
		QueryDepth:             depth,
	}
}

func attachmentsMetadata(subscription, promptID string, types []string) sdkAttachmentsMetadata {
	// Native l.map(c=>c.type): production order, duplicates kept, never null.
	copied := make([]string, 0, len(types))
	copied = append(copied, types...)
	return sdkAttachmentsMetadata{SubscriptionType: subscription, PromptID: promptID, AttachmentTypes: copied}
}

func queryAfterAttachmentsMetadata(subscription, promptID string, loop QueryLoopAttachments, chainID string, depth *int) sdkQueryAfterAttachmentsMetadata {
	return sdkQueryAfterAttachmentsMetadata{
		SubscriptionType:          subscription,
		PromptID:                  promptID,
		TotalToolResultsCount:     loop.ToolResultsCount + len(loop.AttachmentTypes),
		FileChangeAttachmentCount: loop.FileChangeAttachmentCount,
		QueryChainID:              chainID,
		QueryDepth:                depth,
	}
}

func apiBeforeNormalizeMetadata(subscription, promptID string, preNormalizedMessageCount int) sdkAPIBeforeNormalizeMetadata {
	return sdkAPIBeforeNormalizeMetadata{SubscriptionType: subscription, PromptID: promptID, PreNormalizedMessageCount: preNormalizedMessageCount}
}

// queryLineage returns the queryChainId/queryDepth pair tengu_api_query
// reports for this span (undefined natively when the lineage is unknown).
func (s *RequestSpan) queryLineage() (string, *int) {
	if s == nil || s.sdkWorker == nil {
		return "", nil
	}
	s.mu.Lock()
	facts := s.facts
	s.mu.Unlock()
	return sdkQueryLineage(s.sdkWorker, facts)
}

// ObserveQueryLoopAttachments records the native continuation sequence for
// the request this span observes: tengu_query_before_attachments, then
// tengu_attachments when at least one attachment was produced, then
// tengu_query_after_attachments. Call it once per continuation request
// build, before ObserveAPIBeforeNormalize and before the request is sent.
func (s *RequestSpan) ObserveQueryLoopAttachments(loop QueryLoopAttachments) {
	if !s.Active() {
		return
	}
	chainID, depth := s.queryLineage()
	s.enqueueSDKFact(FactSDKQueryBeforeAttachments, func(subscription, promptID string) any {
		return queryBeforeAttachmentsMetadata(subscription, promptID, loop, chainID, depth)
	})
	if loop.ComputeDuration != nil {
		loop.ComputeDuration()
	}
	if len(loop.AttachmentTypes) > 0 {
		s.enqueueSDKFact(FactSDKAttachments, func(subscription, promptID string) any {
			return attachmentsMetadata(subscription, promptID, loop.AttachmentTypes)
		})
	}
	s.enqueueSDKFact(FactSDKQueryAfterAttachments, func(subscription, promptID string) any {
		return queryAfterAttachmentsMetadata(subscription, promptID, loop, chainID, depth)
	})
}

// ObserveAPIBeforeNormalize records tengu_api_before_normalize for the
// request build this span observes. preNormalizedMessageCount is the count
// of internal message rows handed to the request build before normalization
// merged them into the API messages.
func (s *RequestSpan) ObserveAPIBeforeNormalize(preNormalizedMessageCount int) {
	s.enqueueSDKFact(FactSDKAPIBeforeNormalize, func(subscription, promptID string) any {
		return apiBeforeNormalizeMetadata(subscription, promptID, preNormalizedMessageCount)
	})
}
