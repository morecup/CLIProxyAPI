package prompt

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrSDKCompactionRestorationUnknown = errors.New("unresolved native compaction restoration operations")

// SDKCompactionRestorationOps are scoped operations on a proxy-owned native
// context. They must be bound by the context owner, never by request JSON.
// Nil operations are missing facts; no default empty hooks/files are installed.
// The first five stages and SessionStart return native messages, not API rows.
// DerivedContext and RemoteNotice return unwrapped attachment payloads: native
// s1e wraps both together only after the asynchronous remote notice resolves.
type SDKCompactionRestorationOps struct {
	ReadFiles      func(context.Context) ([]json.RawMessage, error)
	LocalTasks     func(context.Context) ([]json.RawMessage, error)
	PlanFile       func(context.Context) (json.RawMessage, error)
	PlanMode       func(context.Context) (json.RawMessage, error)
	InvokedSkills  func(context.Context) (json.RawMessage, error)
	DerivedContext func(context.Context) ([]json.RawMessage, error)
	RemoteNotice   func(context.Context) ([]json.RawMessage, error)
	WrapAttachment func(json.RawMessage) (json.RawMessage, error)
	SessionStart   func(context.Context) ([]json.RawMessage, error)
	OnSessionStart func()
}

type SDKCompactionRestoration struct {
	resolved    bool
	Attachments []json.RawMessage `json:"-"`
	HookResults []json.RawMessage `json:"-"`
	// Native PZo reports diagnostics but can still supply a plan-mode fallback.
	// These errors are not success events, and must not be serialized with text.
	RestoreError  error `json:"-"`
	FallbackError error `json:"-"`
}

// RestoreSDKCompactionContext executes the reviewed s1e/PZo orchestration.
// Context reset, the compact boundary, PostCompact and final application are
// distinct stages owned by the caller. This function emits no success event.
// DerivedContext must implement qhe/B1e/xPt/IPt/QQ in their reviewed order.
func RestoreSDKCompactionContext(ctx context.Context, ops SDKCompactionRestorationOps, isolated, remoteEnabled bool) (SDKCompactionRestoration, error) {
	if ctx == nil || ops.ReadFiles == nil || ops.PlanFile == nil || ops.PlanMode == nil || ops.InvokedSkills == nil || ops.DerivedContext == nil || ops.WrapAttachment == nil ||
		(!isolated && (ops.LocalTasks == nil || ops.SessionStart == nil)) || (!isolated && remoteEnabled && ops.RemoteNotice == nil) {
		return SDKCompactionRestoration{}, ErrSDKCompactionRestorationUnknown
	}
	result, err := restoreSDKCompactionContext(ctx, ops, isolated, remoteEnabled)
	if err == nil {
		result.resolved = true
		return result, nil
	}
	// A missing operation was rejected before any work. By contrast, a real
	// operation failure follows PZo: discard the partial restoration and try
	// only the plan-mode notice, then continue to the caller's PostCompact.
	fallback, fallbackErr := ops.PlanMode(ctx)
	result = SDKCompactionRestoration{resolved: true, RestoreError: err, FallbackError: fallbackErr}
	if fallbackErr == nil && len(fallback) != 0 {
		result.Attachments = []json.RawMessage{append(json.RawMessage(nil), fallback...)}
	}
	return result, nil
}

func restoreSDKCompactionContext(ctx context.Context, ops SDKCompactionRestorationOps, isolated, remoteEnabled bool) (SDKCompactionRestoration, error) {
	type completed struct {
		index int
		rows  []json.RawMessage
		err   error
	}
	count := 1
	if !isolated {
		count++
	}
	// Promise.all preserves input order, not completion order, and rejects on
	// the first rejection without cancelling the other operation. The buffered
	// channel lets that still-owned operation finish without a blocked send.
	done := make(chan completed, count)
	go func() { rows, err := ops.ReadFiles(ctx); done <- completed{0, rows, err} }()
	if !isolated {
		go func() { rows, err := ops.LocalTasks(ctx); done <- completed{1, rows, err} }()
	}
	var initial [2][]json.RawMessage
	for range count {
		value := <-done
		if value.err != nil {
			return SDKCompactionRestoration{}, value.err
		}
		initial[value.index] = value.rows
	}
	result := SDKCompactionRestoration{}
	appendRows := func(rows []json.RawMessage) {
		for _, row := range rows {
			result.Attachments = append(result.Attachments, append(json.RawMessage(nil), row...))
		}
	}
	appendRows(initial[0])
	appendRows(initial[1])
	for _, operation := range []func(context.Context) (json.RawMessage, error){ops.PlanFile, ops.PlanMode, ops.InvokedSkills} {
		row, err := operation(ctx)
		if err != nil {
			return SDKCompactionRestoration{}, err
		}
		if len(row) != 0 {
			appendRows([]json.RawMessage{row})
		}
	}
	derived, err := ops.DerivedContext(ctx)
	if err != nil {
		return SDKCompactionRestoration{}, err
	}
	if !isolated && remoteEnabled {
		remote, errRemote := ops.RemoteNotice(ctx)
		if errRemote != nil {
			return SDKCompactionRestoration{}, errRemote
		}
		derived = append(derived, remote...)
	}
	for _, attachment := range derived {
		wrapped, errWrap := ops.WrapAttachment(attachment)
		if errWrap != nil {
			return SDKCompactionRestoration{}, errWrap
		}
		appendRows([]json.RawMessage{wrapped})
	}
	if !isolated {
		if ops.OnSessionStart != nil {
			ops.OnSessionStart()
		}
		hooks, errHooks := ops.SessionStart(ctx)
		if errHooks != nil {
			return SDKCompactionRestoration{}, errHooks
		}
		for _, row := range hooks {
			result.HookResults = append(result.HookResults, append(json.RawMessage(nil), row...))
		}
	}
	return result, nil
}
