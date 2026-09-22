package prompt

import (
	"context"
	"errors"
	"strings"
)

// SDKCompactHookExecution is the result of an actually executed, owned hook.
// Output and command are request-local content, never telemetry fields. An
// absent runner means unknown hook state, not a successfully empty hook list.
type SDKCompactHookExecution struct {
	Command   string `json:"command"`
	Output    string `json:"output"`
	Succeeded bool   `json:"succeeded"`
	Blocked   bool   `json:"blocked"`
	Cancelled bool   `json:"cancelled"`

	// MatcherKind and HookType are owner observations used only when the
	// compact-hook owner can report the native run inventory. Empty values are
	// unknown and must not be replaced with guessed matcher or callback counts.
	MatcherKind string `json:"-"`
	HookType    string `json:"-"`
}

type SDKCompactHookInput struct {
	Event              string
	Trigger            string
	CustomInstructions *string
	CompactSummary     string
}

type SDKCompactHookOutcome struct {
	completedEvent        string
	NewCustomInstructions string `json:"newCustomInstructions,omitempty"`
	UserDisplayMessage    string `json:"userDisplayMessage,omitempty"`
	BlockedBy             string `json:"blockedBy,omitempty"`
}

type SDKCompactHookRunner func(context.Context, SDKCompactHookInput) ([]SDKCompactHookExecution, error)

var ErrSDKCompactionHooksUnknown = errors.New("unresolved native compaction hook runner")

// RunSDKPreCompactHooks follows RL: isolated contexts still execute PreCompact
// and retain blocking decisions, but suppress display text and instructions.
// The lifecycle caller, not this reducer, owns native exception handling.
func RunSDKPreCompactHooks(ctx context.Context, runner SDKCompactHookRunner, trigger string, customInstructions *string, isolated bool) (SDKCompactHookOutcome, error) {
	if runner == nil {
		return SDKCompactHookOutcome{}, ErrSDKCompactionHooksUnknown
	}
	results, err := runner(ctx, SDKCompactHookInput{Event: "PreCompact", Trigger: trigger, CustomInstructions: customInstructions})
	if err != nil {
		return SDKCompactHookOutcome{}, err
	}
	outcome := ReduceSDKPreCompactHooks(results, isolated)
	outcome.completedEvent = "PreCompact"
	return outcome, nil
}

// RunSDKPostCompactHooks follows bV. A reported hook failure is display output;
// a runner exception is propagated and cannot be treated as application success.
// The native isolated branch skips the runner entirely.
func RunSDKPostCompactHooks(ctx context.Context, runner SDKCompactHookRunner, trigger, summary string, isolated bool) (SDKCompactHookOutcome, error) {
	if isolated {
		return SDKCompactHookOutcome{completedEvent: "PostCompact"}, nil
	}
	if runner == nil {
		return SDKCompactHookOutcome{}, ErrSDKCompactionHooksUnknown
	}
	results, err := runner(ctx, SDKCompactHookInput{Event: "PostCompact", Trigger: trigger, CompactSummary: summary})
	if err != nil {
		return SDKCompactHookOutcome{}, err
	}
	outcome := ReduceSDKPostCompactHooks(results)
	outcome.completedEvent = "PostCompact"
	return outcome, nil
}

func ReduceSDKPreCompactHooks(results []SDKCompactHookExecution, isolated bool) SDKCompactHookOutcome {
	var instructions, display, blocked []string
	for _, result := range results {
		output := trimInputSpace(result.Output)
		// Native RL filters instructions and blocking decisions independently
		// of cancellation; only the display loop skips cancelled executions.
		if result.Succeeded && !result.Blocked && output != "" {
			instructions = append(instructions, output)
		}
		if result.Blocked {
			message := "[" + result.Command + "]"
			if output != "" {
				message += ": " + output
			}
			blocked = append(blocked, message)
		}
		if !result.Cancelled {
			display = append(display, sdkCompactHookDisplay("PreCompact", result, result.Succeeded && !result.Blocked))
		}
	}
	result := SDKCompactHookOutcome{BlockedBy: strings.Join(blocked, "\n")}
	if !isolated {
		result.NewCustomInstructions = strings.Join(instructions, "\n\n")
		result.UserDisplayMessage = strings.Join(display, "\n")
	}
	return result
}

func ReduceSDKPostCompactHooks(results []SDKCompactHookExecution) SDKCompactHookOutcome {
	var display []string
	for _, result := range results {
		if !result.Cancelled {
			// bV does not consult blocked when choosing success display text.
			display = append(display, sdkCompactHookDisplay("PostCompact", result, result.Succeeded))
		}
	}
	return SDKCompactHookOutcome{UserDisplayMessage: strings.Join(display, "\n")}
}

func sdkCompactHookDisplay(event string, result SDKCompactHookExecution, succeeded bool) string {
	status := "failed"
	if succeeded {
		status = "completed successfully"
	}
	message := event + " [" + result.Command + "] " + status
	if output := trimInputSpace(result.Output); output != "" {
		message += ": " + output
	}
	return message
}
