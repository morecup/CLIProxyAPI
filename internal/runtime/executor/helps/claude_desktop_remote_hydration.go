package helps

import (
	"context"
	"errors"
	"time"

	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

// ClaudeDesktopRemoteResumed describes one completed-history adoption: the
// native print-lane resume success. Duration is measured from the start of
// the hydration lane (native m = performance.now() before the session lookup)
// to the moment the adopted history became the owned input.
type ClaudeDesktopRemoteResumed struct {
	Account  string
	Session  string
	Remote   string
	Duration time.Duration
}

// ClaudeDesktopRemoteResumeObserver is invoked on the adoption success path.
// The owning executor installs the telemetry emitter; nil leaves resumes
// unobserved.
var ClaudeDesktopRemoteResumeObserver func(ctx context.Context, resumed ClaudeDesktopRemoteResumed)

// ClaudeDesktopRemoteHydrationPolicy contains decisions made by the owning SDK
// host. Lazy hydration requires a query-owned agent reader and is not enabled by
// this policy until its actual consumer can retain that capability safely.
type ClaudeDesktopRemoteHydrationPolicy struct {
	DeltaEnabled         bool
	SkipSubagentsOnDelta bool
}

// HydrateClaudeDesktopRemote binds the real restoration reader to protected
// hydration and completed-history adoption. Ordinary read failure keeps verified
// local history usable; an epoch conflict must escape to the query owner.
func HydrateClaudeDesktopRemote(ctx context.Context, tracker *claudeprompt.Tracker, account, session, remote string,
	state *claudecontrol.WorkerRestoration, input *ClaudeDesktopRemoteInput, policy ClaudeDesktopRemoteHydrationPolicy, observers ...func(claudeprompt.SDKTranscriptChainEvent)) (claudeprompt.SDKHydrationStatus, error) {
	startedAt := time.Now()
	hydration, err := tracker.PrepareRemoteHydration(account, session, remote)
	if err != nil {
		return claudeprompt.SDKHydrationStatus{ReadFailed: true}, err
	}
	convert := func(read *claudecontrol.InternalEventRead, err error) (*claudeprompt.SDKHydrationRead, error) {
		if read == nil {
			return nil, err
		}
		result := &claudeprompt.SDKHydrationRead{AnchorFallback: read.AnchorFallback}
		for _, event := range read.Events() {
			result.Events = append(result.Events, claudeprompt.SDKHydrationEvent{EventID: event.EventIdentity(), EventIDPresent: event.EventIdentityPresent(), AgentID: event.SessionAgentID, Payload: event.Payload})
		}
		return result, err
	}
	var observeChain func(claudeprompt.SDKTranscriptChainEvent)
	if len(observers) == 1 {
		observeChain = observers[0]
	}
	err = hydration.Run(ctx, claudeprompt.SDKHydrationReaders{
		DeltaEnabled: policy.DeltaEnabled, SkipSubagentsOnDelta: policy.SkipSubagentsOnDelta,
		ObserveChain: observeChain,
		Foreground: func(ctx context.Context, anchor string) (*claudeprompt.SDKHydrationRead, error) {
			return convert(state.ReadInternalEvents(ctx, anchor))
		},
		Subagents: func(ctx context.Context) (*claudeprompt.SDKHydrationRead, error) {
			return convert(state.ReadSubagentInternalEvents(ctx))
		},
	})
	status := hydration.Status()
	var conflict *claudecontrol.WorkerEpochConflict
	if errors.As(err, &conflict) || ctx.Err() != nil {
		return status, err
	}
	if status.ReadFailed {
		return status, nil
	}
	if err != nil && !status.SubagentReadFailed {
		return status, err
	}
	if status.Applied {
		resume, err := hydration.AdoptCompletedHistory()
		if err != nil {
			return status, err
		}
		if resume != nil {
			if errAdopt := input.AdoptHistory(resume); errAdopt != nil {
				return status, errAdopt
			}
			if observe := ClaudeDesktopRemoteResumeObserver; observe != nil {
				observe(ctx, ClaudeDesktopRemoteResumed{Account: account, Session: session, Remote: remote, Duration: time.Since(startedAt)})
			}
			return status, nil
		}
	}
	return status, nil
}
