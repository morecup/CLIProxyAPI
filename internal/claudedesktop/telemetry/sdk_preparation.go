package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	log "github.com/sirupsen/logrus"
)

const (
	FactSDKQuery                 = "api_query"
	FactSDKSystemBlock           = "sysprompt_block"
	FactSDKAfterNormalize        = "api_after_normalize"
	FactSDKSystemBoundary        = "sysprompt_boundary_found"
	FactSDKSystemMissingBoundary = "sysprompt_missing_boundary_marker"
)

type sdkBoundaryMetadata struct {
	SubscriptionType   string `json:"subscription_type"`
	PromptID           string `json:"cc_prompt_id,omitempty"`
	BlockCount         int    `json:"blockCount"`
	StaticBlockLength  int    `json:"staticBlockLength"`
	DynamicBlockLength int    `json:"dynamicBlockLength"`
}

type sdkMissingBoundaryMetadata struct {
	SubscriptionType string `json:"subscription_type"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	PromptBlockCount int    `json:"promptBlockCount"`
}

type sdkBoundaryProjection struct {
	fact     string
	metadata any
}

type sdkAfterNormalizeMetadata struct {
	SubscriptionType           string `json:"subscription_type"`
	PromptID                   string `json:"cc_prompt_id,omitempty"`
	PostNormalizedMessageCount int    `json:"postNormalizedMessageCount"`
	APISystemMessageCount      int    `json:"apiSystemMessageCount"`
}

type sdkQueryMetadata struct {
	SubscriptionType      string  `json:"subscription_type"`
	PromptID              string  `json:"cc_prompt_id,omitempty"`
	Model                 string  `json:"model"`
	MessagesLength        int     `json:"messagesLength"`
	Temperature           float64 `json:"temperature"`
	Provider              string  `json:"provider"`
	BuildAgeMins          int64   `json:"buildAgeMins"`
	Betas                 string  `json:"betas"`
	PermissionMode        string  `json:"permissionMode"`
	QuerySource           string  `json:"querySource"`
	MessageClientPlatform string  `json:"messageClientPlatform,omitempty"`
	QueryChainID          string  `json:"queryChainId,omitempty"`
	QueryDepth            *int    `json:"queryDepth,omitempty"`
	ThinkingType          string  `json:"thinkingType"`
	EffortValue           string  `json:"effortValue,omitempty"`
	FastMode              bool    `json:"fastMode"`
	PreviousRequestID     string  `json:"previousRequestId,omitempty"`
	BaseURL               string  `json:"baseUrl"`
}

type sdkSystemBlockMetadata struct {
	SubscriptionType string `json:"subscription_type"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	Length           int    `json:"length"`
	Hash             string `json:"hash"`
	boundary         *sdkBoundaryProjection
}

var sdkBillingCCH = regexp.MustCompile(`\bcch=[0-9a-f]{5};`)

// sdkPreparationMetadata reads only the final wire request and exports scalar
// facts. apiSystemMessageCount counts role=system entries in messages, not
// top-level system blocks. The pre-normalization SDK list is not recoverable
// from this boundary and requires a separate observation.
func (m *Manager) sdkPreparationMetadata(worker *accountWorker, facts RequestFacts, body []byte) (sdkQueryMetadata, *sdkSystemBlockMetadata, int, bool) {
	var wire struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Temperature *float64 `json:"temperature"`
		Thinking    struct {
			Type string `json:"type"`
		} `json:"thinking"`
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Messages == nil {
		return sdkQueryMetadata{}, nil, 0, false
	}
	// The classifier has its own query path and did not emit either event in
	// the v140609 corpus. CountTokens is not a Messages query.
	if facts.Role == claudeprofile.RoleSecurityMonitor || facts.Role == claudeprofile.RoleCountTokens {
		return sdkQueryMetadata{}, nil, 0, false
	}
	systemMessageCount := 0
	for _, message := range wire.Messages {
		if message.Role == "system" {
			systemMessageCount++
		}
	}
	temperature := 1.0
	if wire.Temperature != nil {
		temperature = *wire.Temperature
	}
	thinking := wire.Thinking.Type
	if thinking == "" {
		thinking = "disabled"
	}
	chain, depth := sdkQueryLineage(worker, facts)
	promptID := facts.PromptID
	if facts.Role == claudeprofile.RoleTitle {
		promptID = ""
	}
	query := sdkQueryMetadata{
		SubscriptionType: subscriptionType(worker.authSnapshot()), PromptID: promptID,
		Model: facts.Model, MessagesLength: len(wire.Messages), Temperature: temperature,
		Provider: "firstParty", BuildAgeMins: m.sdkBuildAgeMinutes(), Betas: facts.Betas,
		PermissionMode: defaultString(facts.PermissionMode, "default"), QuerySource: facts.QuerySource,
		MessageClientPlatform: sdkMessageClientPlatform(facts.Role), QueryChainID: chain, QueryDepth: depth,
		ThinkingType: thinking, EffortValue: facts.EffortLevel, FastMode: facts.FastMode,
		PreviousRequestID: facts.PreviousRequestID, BaseURL: claudedesktop.DefaultAPIHost,
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(wire.System, &blocks) != nil || len(blocks) == 0 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
		return query, nil, systemMessageCount, true
	}
	// The observed hash is taken before request CCH signing, with the five
	// hexadecimal digits still zero. Never hash or retain caller system text.
	billing := sdkBillingCCH.ReplaceAllString(blocks[0].Text, "cch=00000;")
	digest := sha256.Sum256([]byte(billing))
	// These are the captured prepared-system layouts. Do not derive a boundary
	// for arbitrary caller system strings or an unrecognized number of blocks.
	allText := true
	for _, block := range blocks {
		allText = allText && block.Type == "text"
	}
	var boundary *sdkBoundaryProjection
	if allText && len(blocks) == 4 {
		boundary = &sdkBoundaryProjection{fact: FactSDKSystemBoundary, metadata: sdkBoundaryMetadata{
			SubscriptionType: query.SubscriptionType, PromptID: promptID, BlockCount: len(blocks),
			StaticBlockLength: javascriptUTF16Length(blocks[2].Text), DynamicBlockLength: javascriptUTF16Length(blocks[3].Text),
		}}
	} else if allText && len(blocks) == 3 {
		boundary = &sdkBoundaryProjection{fact: FactSDKSystemMissingBoundary, metadata: sdkMissingBoundaryMetadata{
			SubscriptionType: query.SubscriptionType, PromptID: promptID, PromptBlockCount: len(blocks),
		}}
	}
	return query, &sdkSystemBlockMetadata{
		SubscriptionType: query.SubscriptionType, PromptID: promptID,
		Length: javascriptUTF16Length(billing), Hash: hex.EncodeToString(digest[:]),
		boundary: boundary,
	}, systemMessageCount, true
}

func (s *RequestSpan) emitSDKPreparation(facts RequestFacts, body []byte) {
	if s.sdkWorker == nil {
		return
	}
	query, block, systemMessageCount, ok := s.manager.sdkPreparationMetadata(s.sdkWorker, facts, body)
	ok = ok && facts.Attempt <= 1
	enqueue := func(fact string, metadata any) {
		if errEnqueue := s.manager.enqueueSDKEvent(s.sdkWorker, fact, facts, metadata); errEnqueue != nil {
			s.sdkWorker.recordQueueFailure(errEnqueue)
			log.WithError(errEnqueue).Warn("claude desktop SDK telemetry: request preparation event was not persisted")
		}
	}
	if ok {
		enqueue(FactSDKAfterNormalize, sdkAfterNormalizeMetadata{
			SubscriptionType: query.SubscriptionType, PromptID: query.PromptID,
			PostNormalizedMessageCount: query.MessagesLength, APISystemMessageCount: systemMessageCount,
		})
	}
	if ok && block != nil {
		// The captured event order reports the prepared layout on both sides
		// of the unsigned billing hash. Both projections share the same
		// request-local, content-free facts; this does not recover SDK internals.
		if block.boundary != nil {
			enqueue(block.boundary.fact, block.boundary.metadata)
		}
		enqueue(FactSDKSystemBlock, block)
		if block.boundary != nil {
			enqueue(block.boundary.fact, block.boundary.metadata)
		}
	}
	promptID := facts.PromptID
	if facts.Role == claudeprofile.RoleTitle {
		promptID = ""
	}
	enqueue(FactSDKCacheBreakpoints, sdkCacheBreakpointsMetadata{
		SubscriptionType: subscriptionType(s.sdkWorker.authSnapshot()), PromptID: promptID,
		TotalMessageCount: facts.MessageCount, CachingEnabled: facts.CachingEnabled,
		SkipCacheWrite: facts.SkipCacheWrite, ForkPointPinned: facts.ForkPointPinned, MarkerCount: facts.MarkerCount,
	})
	if ok {
		enqueue(FactSDKQuery, query)
	}
}
