package prompt

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	sdkReactiveMaxHeadTruncations       = 3
	sdkReactiveSubstantiveUserTokens    = 1000
	sdkReactiveHeadTruncationSubtype    = "reactive_compaction_head_truncation"
	sdkReactiveHeadTruncationMarkerText = "[earlier conversation truncated for compaction retry]"
)

// SDKReactiveAttempt owns one native summary iteration, not an HTTP retry.
// Repeating an iteration with media stripped retains its attempt number.
// Message slices are structural copies; callers resolve content by UUID in
// their own live conversation store, never from telemetry or HTTP row counts.
type SDKReactiveAttempt struct {
	Attempt             int                 `json:"attempt"`
	GroupsToSummarize   int                 `json:"groupsToSummarize"`
	GroupsToPreserve    int                 `json:"groupsToPreserve"`
	MessagesToSummarize int                 `json:"messagesToSummarize"`
	StrippedMedia       bool                `json:"strippedMedia"`
	SplitKind           string              `json:"splitKind"`
	HeadTruncations     int                 `json:"headTruncations"`
	StepMode            string              `json:"stepMode,omitempty"`
	StepSize            *int                `json:"stepSize,omitempty"`
	TokenGap            *int64              `json:"tokenGap,omitempty"`
	Summarize           []SDKHistoryMessage `json:"-"`
	Preserve            []SDKHistoryMessage `json:"-"`
}

// A success only means that the summary query produced a usable result.
// Restoration, hooks and atomic history application are a separate operation.
// Generic payloads are local to RunSDKReactiveCompaction; no payload is logged,
// retained by Tracker, or accepted as proof of an application success event.
type SDKReactiveQueryResult[T any] struct {
	Success            bool
	Payload            T
	Reason             string
	TokenGap           *int64
	ViaCreditsBoundary bool
	Status             *int
}

type SDKReactiveResult[T any] struct {
	ReadyToApply                           bool
	Payload                                T
	Attempts, TotalGroups, GroupsPreserved int
	SplitKind                              string
	HeadTruncations                        int
	Preserve                               []SDKHistoryMessage
	Reason                                 string
	CreditsRescue                          bool
	Status                                 *int
}

type sdkReactivePromptTooLong struct {
	tokenGap     *int64
	summarizeEnd int
}

type sdkReactiveSummarizeAll struct {
	summarize       []SDKHistoryMessage
	preserve        []SDKHistoryMessage
	headTruncations int
}

var ErrSDKReactiveHistoryUnknown = errors.New("unobserved native compaction history")
var ErrSDKReactiveEstimateUnknown = errors.New("unobserved native compaction group estimate")

// sdkReactiveGapStep follows P9n. An excessive token gap halves the remaining
// groups instead of jumping past all useful assistant history.
func sdkReactiveGapStep(estimates []int64, count int, gap int64) int {
	var tokens int64
	step := 0
	for i := count - 1; i >= 0; i-- {
		tokens += estimates[i]
		step++
		if tokens >= gap {
			break
		}
	}
	if step >= count-1 {
		return max(1, count/2)
	}
	return step
}

func sdkReactiveMessageEstimate(message SDKHistoryMessage) (int64, error) {
	switch message.Type {
	case "user", "assistant", "api_system", "attachment":
		if !message.TokenEstimate.Known || message.TokenEstimate.Tokens < 0 || message.TokenEstimate.Tokens > 1<<40 {
			return 0, ErrSDKReactiveEstimateUnknown
		}
		return message.TokenEstimate.Tokens, nil
	default:
		return 0, nil
	}
}

func sdkReactiveMessageEstimates(messages []SDKHistoryMessage) (int64, error) {
	var tokens int64
	for _, message := range messages {
		estimate, err := sdkReactiveMessageEstimate(message)
		if err != nil {
			return 0, err
		}
		tokens += estimate
	}
	return tokens, nil
}

func sdkReactiveGroupEstimates(groups [][]SDKHistoryMessage) ([]int64, error) {
	estimates := make([]int64, len(groups))
	for index, group := range groups {
		estimate, err := sdkReactiveMessageEstimates(group)
		if err != nil {
			return nil, err
		}
		estimates[index] = estimate
	}
	return estimates, nil
}

func sdkReactiveFlatten(groups [][]SDKHistoryMessage) []SDKHistoryMessage {
	var result []SDKHistoryMessage
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

func sdkReactiveClone(messages []SDKHistoryMessage) []SDKHistoryMessage {
	return append([]SDKHistoryMessage(nil), messages...)
}

func sdkReactiveHasAssistant(messages []SDKHistoryMessage) bool {
	for _, message := range messages {
		if message.Type == "assistant" && !message.IsVirtual {
			return true
		}
	}
	return false
}

func sdkReactiveSubstantive(messages []SDKHistoryMessage) (bool, error) {
	if sdkReactiveHasAssistant(messages) {
		return true, nil
	}
	var users []SDKHistoryMessage
	for _, message := range messages {
		if message.Type == "user" && !message.IsVirtual && !message.Synthetic && message.WireToolResultID == "" {
			users = append(users, message)
		}
	}
	tokens, err := sdkReactiveMessageEstimates(users)
	return tokens >= sdkReactiveSubstantiveUserTokens, err
}

// sdkReactiveSummarizeAllSplit mirrors the native last-resort tail selection:
// keep the last ordinary user turn after the last completed assistant/tool
// result anchor, while summarizing everything before it.
func sdkReactiveSummarizeAllSplit(messages []SDKHistoryMessage) ([]SDKHistoryMessage, []SDKHistoryMessage) {
	anchor := -1
	for index, message := range messages {
		if message.Type == "assistant" && !message.IsVirtual ||
			message.Type == "user" && !message.IsVirtual && message.WireToolResultID != "" {
			anchor = index
		}
	}
	split := -1
	for index, message := range messages {
		if index > anchor && message.Type == "user" && !message.IsVirtual && !message.Synthetic && message.WireToolResultID == "" {
			split = index
		}
	}
	if split < 0 {
		return sdkReactiveClone(messages), nil
	}
	return sdkReactiveClone(messages[:split]), sdkReactiveClone(messages[split:])
}

func sdkReactiveHeadMarker() SDKHistoryMessage {
	return SDKHistoryMessage{Type: "user", Subtype: sdkReactiveHeadTruncationSubtype, Synthetic: true,
		TokenEstimate: SDKTokenEstimate{Known: true}}
}

func sdkReactiveIsHeadMarker(message SDKHistoryMessage) bool {
	return message.Type == "user" && message.Subtype == sdkReactiveHeadTruncationSubtype && message.Synthetic && message.UUID == ""
}

// sdkReactiveTruncateHead drops complete native groups. A parsed token gap
// drives the number removed; otherwise it drops roughly the first 20 percent.
// If the remaining request starts with an assistant, a temporary user marker
// is inserted so the helper request preserves valid message ordering.
func sdkReactiveTruncateHead(messages []SDKHistoryMessage, tokenGap *int64) ([]SDKHistoryMessage, error) {
	if len(messages) > 0 && sdkReactiveIsHeadMarker(messages[0]) {
		messages = messages[1:]
	}
	groups := GroupSDKHistory(messages)
	if len(groups) < 2 {
		return nil, nil
	}
	drop := 0
	if tokenGap != nil {
		var tokens int64
		for _, group := range groups {
			estimate, err := sdkReactiveMessageEstimates(group)
			if err != nil {
				return nil, err
			}
			tokens += estimate
			drop++
			if tokens >= *tokenGap {
				break
			}
		}
	} else {
		drop = max(1, len(groups)/5)
	}
	drop = min(drop, len(groups)-1)
	if drop < 1 {
		return nil, nil
	}
	remaining := sdkReactiveFlatten(groups[drop:])
	if len(remaining) > 0 && remaining[0].Type == "assistant" {
		remaining = append([]SDKHistoryMessage{sdkReactiveHeadMarker()}, remaining...)
	}
	return remaining, nil
}

func sdkReactiveExactSuffix(messages, suffix []SDKHistoryMessage) (int, bool) {
	if len(suffix) > len(messages) {
		return 0, false
	}
	start := len(messages) - len(suffix)
	for index, message := range suffix {
		if message != messages[start+index] {
			return 0, false
		}
	}
	return start, true
}

func sdkReactiveGroupBoundary(groups [][]SDKHistoryMessage, index int) bool {
	position := 0
	for _, group := range groups {
		if position == index {
			return true
		}
		position += len(group)
	}
	return position == index
}

// ValidateSDKReactiveAttempt verifies the structural native selection without
// reading request content. It accepts full-group round suffixes and the exact
// wire-row tail retained by summarize_all, including its temporary marker.
func ValidateSDKReactiveAttempt(history SDKHistorySnapshot, attempt SDKReactiveAttempt) bool {
	if !history.OwnedMessagesKnown || len(history.Messages) > maxSDKHistoryMessages || attempt.MessagesToSummarize != len(attempt.Summarize) {
		return false
	}
	groups := GroupSDKHistory(sdkReactiveClone(history.Messages))
	if len(groups) < 2 || attempt.Attempt < 1 || attempt.Attempt > len(groups)+sdkReactiveMaxHeadTruncations {
		return false
	}
	switch attempt.SplitKind {
	case "round":
		if attempt.HeadTruncations != 0 || attempt.GroupsToSummarize < 1 || attempt.GroupsToPreserve < 1 ||
			attempt.GroupsToSummarize+attempt.GroupsToPreserve != len(groups) {
			return false
		}
		wantSummarize := sdkReactiveFlatten(groups[:attempt.GroupsToSummarize])
		wantPreserve := sdkReactiveFlatten(groups[attempt.GroupsToSummarize:])
		return sdkReactiveSlicesEqual(attempt.Summarize, wantSummarize) && sdkReactiveSlicesEqual(attempt.Preserve, wantPreserve)
	case "summarize_all":
		if attempt.HeadTruncations < 0 || attempt.HeadTruncations > sdkReactiveMaxHeadTruncations ||
			attempt.GroupsToSummarize != len(groups) || attempt.GroupsToPreserve != 0 {
			return false
		}
		active := sdkReactiveFlatten(groups)
		preserveStart, ok := sdkReactiveExactSuffix(active, attempt.Preserve)
		if !ok {
			return false
		}
		summarize := attempt.Summarize
		hasMarker := len(summarize) > 0 && sdkReactiveIsHeadMarker(summarize[0])
		if hasMarker {
			summarize = summarize[1:]
		}
		if len(summarize) == 0 || len(summarize) > preserveStart {
			return false
		}
		start := preserveStart - len(summarize)
		if !sdkReactiveSlicesEqual(summarize, active[start:preserveStart]) || !sdkReactiveGroupBoundary(groups, start) {
			return false
		}
		if attempt.HeadTruncations == 0 {
			if hasMarker || start != 0 {
				return false
			}
		} else if start == 0 || hasMarker != (summarize[0].Type == "assistant") {
			return false
		}
		for _, message := range summarize {
			if message.Synthetic {
				return false
			}
		}
		substantive, err := sdkReactiveSubstantive(append(sdkReactiveClone(summarize), attempt.Preserve...))
		return err == nil && substantive
	default:
		return false
	}
}

func sdkReactiveSlicesEqual(left, right []SDKHistoryMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index, message := range left {
		if message != right[index] {
			return false
		}
	}
	return true
}

// ResolveReactiveSummary resolves the owned portion of a native selection and
// materializes only the reviewed temporary marker used by head truncation.
func (v *SDKCompactionView) ResolveReactiveSummary(messages []SDKHistoryMessage) ([]json.RawMessage, error) {
	hasMarker := len(messages) > 0 && sdkReactiveIsHeadMarker(messages[0])
	if hasMarker {
		messages = messages[1:]
	}
	rows, err := v.Resolve(messages)
	if err != nil {
		return nil, err
	}
	if !hasMarker {
		return rows, nil
	}
	marker := json.RawMessage(`{"role":"user","content":"[earlier conversation truncated for compaction retry]"}`)
	return append([]json.RawMessage{marker}, rows...), nil
}

// RunSDKReactiveCompaction implements the pinned automatic summary-selection
// loop, including the default-enabled summarize_all fallback and at most three
// complete-group head truncations. It executes the supplied summary operation
// but does not emit application success before restoration and Commit.
func RunSDKReactiveCompaction[T any](ctx context.Context, history SDKHistorySnapshot, initialTokenGap *int64,
	query func(context.Context, SDKReactiveAttempt) (SDKReactiveQueryResult[T], error)) (SDKReactiveResult[T], error) {
	return runSDKReactiveCompaction(ctx, history, initialTokenGap, true, query)
}

func runSDKReactiveCompaction[T any](ctx context.Context, history SDKHistorySnapshot, initialTokenGap *int64, allowFallback bool,
	query func(context.Context, SDKReactiveAttempt) (SDKReactiveQueryResult[T], error)) (SDKReactiveResult[T], error) {
	var result SDKReactiveResult[T]
	if !history.OwnedMessagesKnown {
		return result, ErrSDKReactiveHistoryUnknown
	}
	if len(history.Messages) > maxSDKHistoryMessages {
		return result, ErrSDKReactiveHistoryUnknown
	}
	if query == nil {
		return result, errors.New("missing native compaction summary operation")
	}
	groups := GroupSDKHistory(sdkReactiveClone(history.Messages))
	count := len(groups)
	result.TotalGroups = count
	if count < 2 {
		result.Reason = "too_few_groups"
		return result, nil
	}
	active := sdkReactiveFlatten(groups)
	keep, attempt := 1, 0
	stripped := false
	var estimates []int64
	var estimateErr error
	ensureEstimates := func() ([]int64, error) {
		if estimates == nil {
			estimates, estimateErr = sdkReactiveGroupEstimates(groups)
		}
		return estimates, estimateErr
	}
	mode := ""
	var step *int
	var gap *int64
	if initialTokenGap != nil && count > 3 {
		values, err := ensureEstimates()
		if err != nil {
			return result, err
		}
		remaining := *initialTokenGap - values[count-1]
		if remaining > 0 {
			n := sdkReactiveGapStep(values, count-1, remaining)
			keep, mode, step = 1+n, "seeded", &n
			g := *initialTokenGap
			gap = &g
		}
	}
	var lastPromptTooLong *sdkReactivePromptTooLong
	var summarizeAll *sdkReactiveSummarizeAll
	for {
		result.Attempts = attempt
		if err := ctx.Err(); err != nil {
			result.Reason = "aborted"
			if summarizeAll != nil {
				result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
			}
			return result, nil
		}

		var summarize, preserve []SDKHistoryMessage
		splitKind, headTruncations, groupsPreserved := "round", 0, keep
		groupsToSummarize := max(0, count-keep)
		if summarizeAll == nil {
			summarize = sdkReactiveFlatten(groups[:groupsToSummarize])
			if !sdkReactiveHasAssistant(summarize) {
				if !allowFallback {
					result.Reason = "too_few_groups"
					if attempt > 0 {
						result.Reason = "exhausted"
					}
					return result, nil
				}
				toSummarize, toKeep := sdkReactiveSummarizeAllSplit(active)
				var adjustedGap *int64
				needsTruncation := false
				var candidates []int64
				if lastPromptTooLong != nil && lastPromptTooLong.tokenGap != nil {
					values, err := ensureEstimates()
					if err != nil {
						return result, err
					}
					candidate := *lastPromptTooLong.tokenGap
					for _, estimate := range values[lastPromptTooLong.summarizeEnd:] {
						candidate += estimate
					}
					candidates = append(candidates, candidate)
				}
				if initialTokenGap != nil {
					candidates = append(candidates, *initialTokenGap)
				}
				if len(candidates) > 0 {
					largest := candidates[0]
					for _, candidate := range candidates[1:] {
						largest = max(largest, candidate)
					}
					keptTokens, err := sdkReactiveMessageEstimates(toKeep)
					if err != nil {
						return result, err
					}
					if largest -= keptTokens; largest > 0 {
						value := largest
						adjustedGap, needsTruncation = &value, true
					}
				} else if lastPromptTooLong != nil {
					needsTruncation = true
				}
				if needsTruncation {
					toSummarize, estimateErr = sdkReactiveTruncateHead(toSummarize, adjustedGap)
					if estimateErr != nil {
						return result, estimateErr
					}
				}
				substantive, err := sdkReactiveSubstantive(append(sdkReactiveClone(toSummarize), toKeep...))
				if err != nil {
					return result, err
				}
				if toSummarize == nil || !substantive {
					result.Reason = "too_few_groups"
					if attempt > 0 {
						result.Reason = "exhausted"
					}
					result.SplitKind, result.HeadTruncations = "summarize_all", 0
					return result, nil
				}
				summarizeAll = &sdkReactiveSummarizeAll{summarize: toSummarize, preserve: toKeep}
				if needsTruncation {
					summarizeAll.headTruncations = 1
				}
				result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
				mode, step, gap = "", nil, nil
				continue
			}
			preserve = sdkReactiveFlatten(groups[groupsToSummarize:])
		} else {
			summarize, preserve = summarizeAll.summarize, summarizeAll.preserve
			splitKind, headTruncations, groupsPreserved = "summarize_all", summarizeAll.headTruncations, 0
			groupsToSummarize = count
		}

		attempt++
		plan := SDKReactiveAttempt{
			Attempt: attempt, GroupsToSummarize: groupsToSummarize, GroupsToPreserve: groupsPreserved,
			MessagesToSummarize: len(summarize), StrippedMedia: stripped, SplitKind: splitKind, HeadTruncations: headTruncations,
			StepMode: mode, StepSize: step, TokenGap: gap, Summarize: sdkReactiveClone(summarize), Preserve: sdkReactiveClone(preserve),
		}
		if step != nil {
			n := *step
			plan.StepSize = &n
		}
		if gap != nil {
			n := *gap
			plan.TokenGap = &n
		}
		if !ValidateSDKReactiveAttempt(history, plan) {
			return result, errors.New("invalid native reactive compaction attempt")
		}
		outcome, errQuery := query(ctx, plan)
		result.Attempts = attempt
		if errQuery != nil {
			return result, errQuery
		}
		if ctx.Err() != nil {
			result.Reason = "aborted"
			if summarizeAll != nil {
				result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
			}
			return result, nil
		}
		if outcome.Success {
			result.ReadyToApply, result.Payload = true, outcome.Payload
			result.Preserve, result.GroupsPreserved = sdkReactiveClone(preserve), groupsPreserved
			result.SplitKind, result.HeadTruncations = splitKind, headTruncations
			return result, nil
		}
		switch outcome.Reason {
		case "aborted", "error":
			result.Reason = outcome.Reason
			if outcome.Reason == "error" && outcome.Status != nil {
				status := *outcome.Status
				result.Status = &status
			}
			if summarizeAll != nil {
				result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
			}
			return result, nil
		case "media_too_large":
			if stripped {
				result.Reason = "media_unstrippable"
				if summarizeAll != nil {
					result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
				}
				return result, nil
			}
			stripped = true
			attempt--
			continue
		case "prompt_too_long":
			if outcome.ViaCreditsBoundary {
				result.CreditsRescue = true
			}
		default:
			return result, errors.New("unrecognized native compaction summary outcome")
		}

		if summarizeAll != nil {
			if summarizeAll.headTruncations < sdkReactiveMaxHeadTruncations {
				var tokenGap *int64
				if outcome.TokenGap != nil {
					value := *outcome.TokenGap
					tokenGap = &value
				}
				next, err := sdkReactiveTruncateHead(summarizeAll.summarize, tokenGap)
				if err != nil {
					return result, err
				}
				substantive, err := sdkReactiveSubstantive(append(sdkReactiveClone(next), summarizeAll.preserve...))
				if err != nil {
					return result, err
				}
				if next != nil && substantive {
					summarizeAll.summarize = next
					summarizeAll.headTruncations++
					result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
					continue
				}
			}
			result.Reason = "exhausted"
			result.SplitKind, result.HeadTruncations = "summarize_all", summarizeAll.headTruncations
			return result, nil
		}

		var tokenGap *int64
		if outcome.TokenGap != nil {
			value := *outcome.TokenGap
			tokenGap = &value
		}
		lastPromptTooLong = &sdkReactivePromptTooLong{tokenGap: tokenGap, summarizeEnd: groupsToSummarize}
		n := 1
		mode = "gap_unparseable"
		gap = nil
		if tokenGap != nil {
			values, err := ensureEstimates()
			if err != nil {
				return result, err
			}
			g := *tokenGap
			gap = &g
			n, mode = sdkReactiveGapStep(values, groupsToSummarize, g), "gap_guided"
		}
		step = &n
		keep += n
	}
}
