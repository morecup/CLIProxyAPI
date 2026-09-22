package prompt

import (
	"context"
	"errors"
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
}

type SDKReactiveResult[T any] struct {
	ReadyToApply                           bool
	Payload                                T
	Attempts, TotalGroups, GroupsPreserved int
	Preserve                               []SDKHistoryMessage
	Reason                                 string
	CreditsRescue                          bool
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

func sdkReactiveGroupEstimates(groups [][]SDKHistoryMessage) ([]int64, error) {
	estimates := make([]int64, len(groups))
	for index, group := range groups {
		for _, m := range group {
			switch m.Type {
			case "user", "assistant", "api_system", "attachment":
				if !m.TokenEstimate.Known || m.TokenEstimate.Tokens < 0 || m.TokenEstimate.Tokens > 1<<40 {
					return nil, ErrSDKReactiveEstimateUnknown
				}
				estimates[index] += m.TokenEstimate.Tokens
			}
		}
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

// RunSDKReactiveCompaction implements the pinned r1e summary-selection loop.
// It executes the supplied summary operation and uses its observed outcome to
// choose the next native group range. It neither assumes success from an HTTP
// status nor emits trigger/success telemetry for an unowned application.
// The callback is synchronous per operation; independent sessions can run in
// parallel. The context is the owner's cancellation scope, not a new deadline.
func RunSDKReactiveCompaction[T any](ctx context.Context, history SDKHistorySnapshot, initialTokenGap *int64,
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
	// Derive groups from the copied message list; do not trust cached or
	// caller-supplied Groups to partition a different history snapshot.
	groups := GroupSDKHistory(append([]SDKHistoryMessage(nil), history.Messages...))
	count := len(groups)
	result.TotalGroups = count
	if count < 2 {
		result.Reason = "too_few_groups"
		return result, nil
	}
	keep, attempt := 1, 0
	stripped := false
	var estimates []int64
	var err error
	mode := ""
	var step *int
	var gap *int64
	if initialTokenGap != nil && count > 3 {
		estimates, err = sdkReactiveGroupEstimates(groups)
		if err != nil {
			return result, err
		}
		remaining := *initialTokenGap - estimates[count-1]
		if remaining > 0 {
			n := sdkReactiveGapStep(estimates, count-1, remaining)
			keep, mode, step = 1+n, "seeded", &n
			g := *initialTokenGap
			gap = &g
		}
	}
	for keep < count {
		result.Attempts = attempt
		if err := ctx.Err(); err != nil {
			result.Reason = "aborted"
			return result, nil
		}
		attempt++
		split := count - keep
		summarize := sdkReactiveFlatten(groups[:split])
		preserve := sdkReactiveFlatten(groups[split:])
		hasAssistant := false
		for _, m := range summarize {
			if m.Type == "assistant" {
				hasAssistant = true
				break
			}
		}
		if !hasAssistant {
			result.Attempts = attempt - 1
			result.Reason = "too_few_groups"
			if attempt > 1 {
				result.Reason = "exhausted"
			}
			return result, nil
		}
		plan := SDKReactiveAttempt{Attempt: attempt, GroupsToSummarize: split, GroupsToPreserve: keep, MessagesToSummarize: len(summarize),
			StrippedMedia: stripped, StepMode: mode, StepSize: step, TokenGap: gap, Summarize: summarize, Preserve: append([]SDKHistoryMessage(nil), preserve...)}
		// The callback cannot mutate the next attempt's metadata pointers.
		if step != nil {
			n := *step
			plan.StepSize = &n
		}
		if gap != nil {
			n := *gap
			plan.TokenGap = &n
		}
		outcome, errQuery := query(ctx, plan)
		result.Attempts = attempt
		if errQuery != nil {
			return result, errQuery
		}
		if ctx.Err() != nil {
			result.Reason = "aborted"
			return result, nil
		}
		if outcome.Success {
			result.ReadyToApply, result.Payload, result.Preserve, result.GroupsPreserved = true, outcome.Payload, preserve, keep
			return result, nil
		}
		switch outcome.Reason {
		case "aborted", "error":
			result.Reason = outcome.Reason
			return result, nil
		case "media_too_large":
			if stripped {
				result.Reason = "media_unstrippable"
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
		n := 1
		mode = "gap_unparseable"
		gap = nil
		if outcome.TokenGap != nil {
			if estimates == nil {
				estimates, err = sdkReactiveGroupEstimates(groups)
				if err != nil {
					return result, err
				}
			}
			g := *outcome.TokenGap
			gap = &g
			n, mode = sdkReactiveGapStep(estimates, split, g), "gap_guided"
		}
		step = &n
		keep += n
	}
	result.Reason = "exhausted"
	return result, nil
}
