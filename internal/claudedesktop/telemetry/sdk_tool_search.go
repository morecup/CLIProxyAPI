package telemetry

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Tool-search deferral facts of the pinned SDK (Claude Code 2.1.247,
// module _448.js): j1e emits tengu_tool_search_mode_decision for every
// request build, XHr emits tengu_deferred_tools_pool_change when the
// deferred_tools_delta attachment is produced, and the ToolSearch tool call
// emits tengu_tool_search_outcome. Property names and their order follow the
// native object literals; the native logger prepends subscription_type and
// cc_prompt_id (module _675.js) before the event's own metadata.
const (
	FactSDKToolSearchModeDecision  = "tool_search_mode_decision"
	FactSDKDeferredToolsPoolChange = "deferred_tools_pool_change"
	FactSDKToolSearchOutcome       = "tool_search_outcome"
)

// Native j1e modes and reasons. Only the Desktop-reachable subset is named;
// any other native string is still accepted verbatim by the emitters.
const (
	ToolSearchModeTST      = "tst"
	ToolSearchModeStandard = "standard"

	ToolSearchReasonTSTEnabled       = "tst_enabled"
	ToolSearchReasonModelUnsupported = "model_unsupported"
	ToolSearchReasonNoToolsInRequest = "no_tools_in_request"
	ToolSearchReasonNotRegistered    = "not_registered"
	ToolSearchReasonStandardMode     = "standard_mode"

	DeferredToolsCallSiteMain     = "attachments_main"
	DeferredToolsCallSiteSubagent = "attachments_subagent"

	ToolSearchQueryTypeSelect  = "select"
	ToolSearchQueryTypeKeyword = "keyword"
	ToolSearchUserType         = "external"
)

// ToolSearchModeDecision mirrors the arguments of the native j1e reporter
// (enabled, mode, reason, checkedModel, mcpToolCount, mcpNonBlocking).
type ToolSearchModeDecision struct {
	Enabled        bool
	Mode           string
	Reason         string
	CheckedModel   string
	MCPToolCount   int
	MCPNonBlocking bool
}

// DeferredToolsPoolChange mirrors the native XHr telemetry payload.
// AttachmentTypesSeen is sorted and comma-joined at serialization time.
type DeferredToolsPoolChange struct {
	AddedCount          int
	ReaddedCount        int
	UnlistedCount       int
	RemovedCount        int
	PendingChanged      bool
	PendingCount        int
	LastPendingCount    int
	NeedsAuthChanged    bool
	NeedsAuthCount      int
	LastNeedsAuthCount  int
	FailedChanged       bool
	FailedCount         int
	LastFailedCount     int
	PriorAnnouncedCount int
	MessagesLength      int
	AttachmentCount     int
	DTDCount            int
	CallSite            string
	QuerySource         string
	AttachmentTypesSeen []string
}

// ToolSearchOutcome mirrors the native ToolSearch call telemetry payload.
// QuerySelectCount is only serialized for select queries (native undefined
// otherwise).
type ToolSearchOutcome struct {
	QueryLength          int
	QuerySelectCount     int
	QueryType            string
	MatchCount           int
	TotalDeferredTools   int
	MaxResults           int
	HasMatches           bool
	MCPServersConfigured int
	MCPServersConnected  int
	MCPServersCached     int
	MCPServersPending    int
	MCPToolsInPool       int
}

type sdkToolSearchModeDecisionMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	Enabled          bool   `json:"enabled"`
	Mode             string `json:"mode"`
	Reason           string `json:"reason"`
	CheckedModel     string `json:"checkedModel"`
	MCPToolCount     int    `json:"mcpToolCount"`
	MCPNonBlocking   bool   `json:"mcpNonBlocking"`
	UserType         string `json:"userType"`
}

type sdkDeferredToolsPoolChangeMetadata struct {
	SubscriptionType    string `json:"subscription_type,omitempty"`
	PromptID            string `json:"cc_prompt_id,omitempty"`
	AddedCount          int    `json:"addedCount"`
	ReaddedCount        int    `json:"readdedCount"`
	UnlistedCount       int    `json:"unlistedCount"`
	RemovedCount        int    `json:"removedCount"`
	PendingChanged      bool   `json:"pendingChanged"`
	PendingCount        int    `json:"pendingCount"`
	LastPendingCount    int    `json:"lastPendingCount"`
	NeedsAuthChanged    bool   `json:"needsAuthChanged"`
	NeedsAuthCount      int    `json:"needsAuthCount"`
	LastNeedsAuthCount  int    `json:"lastNeedsAuthCount"`
	FailedChanged       bool   `json:"failedChanged"`
	FailedCount         int    `json:"failedCount"`
	LastFailedCount     int    `json:"lastFailedCount"`
	PriorAnnouncedCount int    `json:"priorAnnouncedCount"`
	MessagesLength      int    `json:"messagesLength"`
	AttachmentCount     int    `json:"attachmentCount"`
	DTDCount            int    `json:"dtdCount"`
	CallSite            string `json:"callSite"`
	QuerySource         string `json:"querySource"`
	AttachmentTypesSeen string `json:"attachmentTypesSeen"`
}

type sdkToolSearchOutcomeMetadata struct {
	SubscriptionType     string `json:"subscription_type,omitempty"`
	PromptID             string `json:"cc_prompt_id,omitempty"`
	QueryLength          int    `json:"queryLength"`
	QuerySelectCount     *int   `json:"querySelectCount,omitempty"`
	QueryType            string `json:"queryType"`
	MatchCount           int    `json:"matchCount"`
	TotalDeferredTools   int    `json:"totalDeferredTools"`
	MaxResults           int    `json:"maxResults"`
	HasMatches           bool   `json:"hasMatches"`
	MCPServersConfigured int    `json:"mcpServersConfigured"`
	MCPServersConnected  int    `json:"mcpServersConnected"`
	MCPServersCached     int    `json:"mcpServersCached"`
	MCPServersPending    int    `json:"mcpServersPending"`
	MCPToolsInPool       int    `json:"mcpToolsInPool"`
}

func toolSearchModeDecisionMetadata(subscription, promptID string, decision ToolSearchModeDecision) sdkToolSearchModeDecisionMetadata {
	return sdkToolSearchModeDecisionMetadata{
		SubscriptionType: subscription,
		PromptID:         promptID,
		Enabled:          decision.Enabled,
		Mode:             decision.Mode,
		Reason:           decision.Reason,
		CheckedModel:     decision.CheckedModel,
		MCPToolCount:     decision.MCPToolCount,
		MCPNonBlocking:   decision.MCPNonBlocking,
		UserType:         ToolSearchUserType,
	}
}

func deferredToolsPoolChangeMetadata(subscription, promptID string, change DeferredToolsPoolChange) sdkDeferredToolsPoolChangeMetadata {
	types := make([]string, 0, len(change.AttachmentTypesSeen))
	seen := make(map[string]struct{}, len(change.AttachmentTypesSeen))
	for _, value := range change.AttachmentTypesSeen {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		types = append(types, value)
	}
	sort.Strings(types)
	return sdkDeferredToolsPoolChangeMetadata{
		SubscriptionType:    subscription,
		PromptID:            promptID,
		AddedCount:          change.AddedCount,
		ReaddedCount:        change.ReaddedCount,
		UnlistedCount:       change.UnlistedCount,
		RemovedCount:        change.RemovedCount,
		PendingChanged:      change.PendingChanged,
		PendingCount:        change.PendingCount,
		LastPendingCount:    change.LastPendingCount,
		NeedsAuthChanged:    change.NeedsAuthChanged,
		NeedsAuthCount:      change.NeedsAuthCount,
		LastNeedsAuthCount:  change.LastNeedsAuthCount,
		FailedChanged:       change.FailedChanged,
		FailedCount:         change.FailedCount,
		LastFailedCount:     change.LastFailedCount,
		PriorAnnouncedCount: change.PriorAnnouncedCount,
		MessagesLength:      change.MessagesLength,
		AttachmentCount:     change.AttachmentCount,
		DTDCount:            change.DTDCount,
		CallSite:            defaultString(change.CallSite, "unknown"),
		QuerySource:         defaultString(change.QuerySource, "unknown"),
		AttachmentTypesSeen: strings.Join(types, ","),
	}
}

func toolSearchOutcomeMetadata(subscription, promptID string, outcome ToolSearchOutcome) sdkToolSearchOutcomeMetadata {
	metadata := sdkToolSearchOutcomeMetadata{
		SubscriptionType:     subscription,
		PromptID:             promptID,
		QueryLength:          outcome.QueryLength,
		QueryType:            outcome.QueryType,
		MatchCount:           outcome.MatchCount,
		TotalDeferredTools:   outcome.TotalDeferredTools,
		MaxResults:           outcome.MaxResults,
		HasMatches:           outcome.HasMatches,
		MCPServersConfigured: outcome.MCPServersConfigured,
		MCPServersConnected:  outcome.MCPServersConnected,
		MCPServersCached:     outcome.MCPServersCached,
		MCPServersPending:    outcome.MCPServersPending,
		MCPToolsInPool:       outcome.MCPToolsInPool,
	}
	if outcome.QueryType == ToolSearchQueryTypeSelect {
		count := outcome.QuerySelectCount
		metadata.QuerySelectCount = &count
	}
	return metadata
}

var toolSearchSelectQuery = regexp.MustCompile(`(?i)^select:(.+)$`)

// ToolSearchOutcomeFromQuery derives the query facts the way the native call
// does: queryLength is the JavaScript string length, the select form is the
// case-insensitive /^select:(.+)$/ match and querySelectCount counts the
// commas of the raw query plus one. MCP counters stay zero: the owned pool
// has no MCP servers or tools.
func ToolSearchOutcomeFromQuery(query string, maxResults, matchCount, totalDeferredTools int) ToolSearchOutcome {
	outcome := ToolSearchOutcome{
		QueryLength:        len(utf16.Encode([]rune(query))),
		QueryType:          ToolSearchQueryTypeKeyword,
		MatchCount:         matchCount,
		TotalDeferredTools: totalDeferredTools,
		MaxResults:         maxResults,
		HasMatches:         matchCount > 0,
	}
	if toolSearchSelectQuery.MatchString(query) {
		outcome.QueryType = ToolSearchQueryTypeSelect
		outcome.QuerySelectCount = strings.Count(query, ",") + 1
	}
	return outcome
}

// ObserveToolSearchModeDecision records the native j1e decision of the
// request this span observes. Call it once per request build, before the
// request is sent, so the event precedes tengu_api_query as it does natively.
func (s *RequestSpan) ObserveToolSearchModeDecision(decision ToolSearchModeDecision) {
	s.enqueueToolSearchEvent(FactSDKToolSearchModeDecision, func(subscription, promptID string) any {
		return toolSearchModeDecisionMetadata(subscription, promptID, decision)
	})
}

// ObserveDeferredToolsPoolChange records the native XHr telemetry for a
// request whose deferred_tools_delta reminder was produced.
func (s *RequestSpan) ObserveDeferredToolsPoolChange(change DeferredToolsPoolChange) {
	if change.QuerySource == "" && s != nil {
		change.QuerySource = sdkQuerySource(s.facts.Role)
	}
	s.enqueueToolSearchEvent(FactSDKDeferredToolsPoolChange, func(subscription, promptID string) any {
		return deferredToolsPoolChangeMetadata(subscription, promptID, change)
	})
}

// ObserveToolSearchOutcome records a ToolSearch execution that happened while
// this span is still open (a span-less variant is RecordSDKToolSearchOutcome).
func (s *RequestSpan) ObserveToolSearchOutcome(outcome ToolSearchOutcome) {
	s.enqueueToolSearchEvent(FactSDKToolSearchOutcome, func(subscription, promptID string) any {
		return toolSearchOutcomeMetadata(subscription, promptID, outcome)
	})
}

func (s *RequestSpan) enqueueToolSearchEvent(fact string, build func(subscription, promptID string) any) {
	if !s.Active() || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	facts, finished := s.facts, s.finished
	s.mu.Unlock()
	if finished {
		return
	}
	// The wrapper default beta header is the logger dimension, not the
	// Messages request header (which is only known once the request is sent).
	betas, known := s.manager.sdkProfile.InputBetaHeader(facts.Model)
	if !known {
		log.WithField("model", facts.Model).Warn("claude desktop SDK telemetry: tool search event dropped because the model has no profiled beta header")
		return
	}
	facts.Betas = betas
	metadata := build(subscriptionType(s.sdkWorker.authSnapshot()), facts.PromptID)
	if errEnqueue := s.manager.enqueueSDKEvent(s.sdkWorker, fact, facts, metadata); errEnqueue != nil {
		s.sdkWorker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).WithField("fact", fact).Warn("claude desktop SDK telemetry: tool search event was not persisted")
	}
}

// RecordSDKToolSearchOutcome emits tengu_tool_search_outcome outside a request
// span: the ToolSearch tool runs between two owned API requests, after the
// producing request's span has finished. session and model are the owned
// query's SDK session and model; promptID is the current prompt (empty for
// none).
func (m *Manager) RecordSDKToolSearchOutcome(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, outcome ToolSearchOutcome) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if m.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop telemetry owner is unavailable")
	}
	session, model = strings.TrimSpace(session), strings.TrimSpace(model)
	if session == "" || model == "" {
		return fmt.Errorf("Claude Desktop tool search outcome identity is incomplete")
	}
	betas, known := m.sdkProfile.InputBetaHeader(model)
	if !known {
		return fmt.Errorf("Claude Desktop tool search outcome model %q has no profiled beta header", model)
	}
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		return errWorker
	}
	facts := RequestFacts{SessionID: session, Model: model, Betas: betas, PromptID: strings.TrimSpace(promptID)}
	metadata := toolSearchOutcomeMetadata(subscriptionType(worker.authSnapshot()), facts.PromptID, outcome)
	if errEnqueue := m.enqueueSDKEvent(worker, FactSDKToolSearchOutcome, facts, metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}
