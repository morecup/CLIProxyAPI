package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
)

// ClaudeDesktopRemoteInput owns one live query's admitted inputs. Transport
// acknowledgment only means enqueue; it is not successful model execution.
// Initial history is supplied only by a verified protected-history capability.
type ClaudeDesktopRemoteInput struct {
	ctx                         context.Context
	cancel                      context.CancelFunc
	done                        chan struct{}
	wake                        chan struct{}
	ready                       chan struct{}
	start                       sync.Once
	mu                          sync.Mutex
	queue                       []remoteInputMessage
	active                      context.CancelFunc
	model, defaultModel, system string
	history                     []remoteInputMessage
	execute                     func(context.Context, string, []byte) ([]byte, error)
	admit                       func(context.Context) error
	resolveModel                func(string) (string, error)
	persist                     func(context.Context, string, string) error
	resume                      *claudeprompt.SDKRemoteWireResume
	resumeErr                   error
	started                     bool
	restoredWorkerEpoch         int64
	workerRestored              bool
	outcome                     func(error)
	agents                      *claudetasks.Runtime
	agentInitErr                error
}

// Configuration comes from the owned record, never the inbound user envelope.
// Persist must commit a control change before it is exposed to another turn.
type ClaudeDesktopRemoteInputConfiguration struct {
	Model, DefaultModel, System string
	Persist                     func(context.Context, string, string) error
	Resume                      *claudeprompt.SDKRemoteWireResume
	Agents                      *claudetasks.Options
}

type remoteInputMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	UUID             string          `json:"-"`
	TaskNotification bool            `json:"-"`
	// Meta is set for a subagent's SendMessage delivery to this conversation.
	// Content holds the queued value; the wire applies the fresh-turn projection.
	Meta *claudetasks.MainDelivery `json:"-"`
}

func NewClaudeDesktopRemoteInput(ctx context.Context, config ClaudeDesktopRemoteInputConfiguration,
	execute func(context.Context, string, []byte) ([]byte, error),
	admit func(context.Context) error, resolveModel func(string) (string, error), outcome func(error)) *ClaudeDesktopRemoteInput {
	ctx, cancel := context.WithCancel(ctx)
	a := &ClaudeDesktopRemoteInput{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		ready: make(chan struct{}), model: config.Model, defaultModel: config.DefaultModel, system: config.System,
		persist: config.Persist, resume: config.Resume, execute: execute, admit: admit, resolveModel: resolveModel, outcome: outcome}
	if a.defaultModel == "" {
		a.defaultModel = config.Model
	}
	if config.Agents != nil {
		options := *config.Agents
		options.MessageMain = a.enqueueMetaInput
		options.Notify = func(event claudetasks.Event) error {
			// The content belongs to the completed generation. Do not reread
			// a newer run's output while delivering an older notification.
			text, err := claudetasks.FormatNotification(event, "")
			if err != nil {
				return err
			}
			return a.enqueueNotification(text, true)
		}
		a.agents, a.agentInitErr = claudetasks.New(ctx, options)
	}
	go a.run()
	return a
}

func (a *ClaudeDesktopRemoteInput) Start() error {
	a.start.Do(func() {
		_ = a.Restore()
		a.mu.Lock()
		a.started = a.resumeErr == nil
		a.mu.Unlock()
		close(a.ready)
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resumeErr
}

// Restore runs before bridge attachment can append its new lifecycle metadata.
// The actor is still paused and no worker input can invoke inference yet.
func (a *ClaudeDesktopRemoteInput) Restore() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resume == nil {
		return a.resumeErr
	}
	rows, err := a.resume.Read()
	a.resume, a.resumeErr = nil, err
	if err == nil {
		for _, raw := range rows {
			var row remoteInputMessage
			if err := json.Unmarshal(raw, &row); err != nil {
				a.resumeErr = err
				break
			}
			a.history = append(a.history, row)
		}
	}
	if a.resumeErr != nil {
		a.cancel()
	}
	return a.resumeErr
}
func (a *ClaudeDesktopRemoteInput) Stop()                 { a.cancel() }
func (a *ClaudeDesktopRemoteInput) Done() <-chan struct{} { return a.done }

func (a *ClaudeDesktopRemoteInput) AgentTasks() ([]claudetasks.Snapshot, error) {
	return a.agents.Snapshots(), a.agentInitErr
}

func (a *ClaudeDesktopRemoteInput) enqueueNotification(text string, taskNotification bool) error {
	raw, _ := json.Marshal(text)
	return a.enqueueOwned(remoteInputMessage{Role: "user", Content: raw, UUID: uuid.NewString(), TaskNotification: taskNotification})
}

// enqueueMetaInput admits a subagent's SendMessage to this conversation as
// the native isMeta prompt: the queued value is projected when its turn runs.
func (a *ClaudeDesktopRemoteInput) enqueueMetaInput(delivery claudetasks.MainDelivery) error {
	if delivery.Text == "" || !json.Valid(delivery.Origin) {
		return claudetasks.ErrInvalid
	}
	raw, _ := json.Marshal(delivery.Text)
	copied := claudetasks.MainDelivery{Text: delivery.Text, Origin: append(json.RawMessage(nil), delivery.Origin...)}
	return a.enqueueOwned(remoteInputMessage{Role: "user", Content: raw, UUID: uuid.NewString(), Meta: &copied})
}

func (a *ClaudeDesktopRemoteInput) enqueueOwned(message remoteInputMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	a.queue = append(a.queue, message)
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

// AdoptHistory replaces only the paused actor's wire projection. The tracker
// capability revalidates durable adoption before any history is published.
func (a *ClaudeDesktopRemoteInput) AdoptHistory(resume *claudeprompt.SDKRemoteWireResume) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started || a.ctx.Err() != nil || resume == nil {
		return claudeprompt.ErrSDKSessionActive
	}
	rows, err := resume.Read()
	if err != nil {
		return err
	}
	var history []remoteInputMessage
	for _, raw := range rows {
		var row remoteInputMessage
		if json.Unmarshal(raw, &row) != nil {
			return claudeprompt.ErrSDKSessionInvalid
		}
		history = append(history, row)
	}
	a.history, a.resume, a.resumeErr = history, nil, nil
	return nil
}

// Only payload.message.content becomes input. A remote envelope cannot inject
// request configuration, headers, system prompts, or another account's history.
func (a *ClaudeDesktopRemoteInput) Enqueue(ctx context.Context, raw json.RawMessage) error {
	var value struct {
		Type    string             `json:"type"`
		UUID    string             `json:"uuid"`
		Message remoteInputMessage `json:"message"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Type != "user" || value.Message.Role != "user" || !validRemoteContent(value.Message.Content) {
		return errors.New("Claude Desktop remote user message is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	if a.admit != nil {
		if err := a.admit(ctx); err != nil {
			return err
		}
	}
	value.Message.UUID = value.UUID
	a.queue = append(a.queue, value.Message)
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

func validRemoteContent(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil && string(raw) != "null" {
		return text != ""
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &value) != nil || value.Type == "" {
			return false
		}
	}
	return true
}

func (a *ClaudeDesktopRemoteInput) Control(ctx context.Context, raw json.RawMessage) (any, error) {
	var value struct {
		Request struct {
			Subtype      string          `json:"subtype"`
			Model        json.RawMessage `json:"model"`
			System       json.RawMessage `json:"system_prompt"`
			CancelQueued bool            `json:"cancel_queued"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil, errors.New("Claude Desktop control request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	switch value.Request.Subtype {
	case "interrupt":
		if a.active != nil {
			a.active()
		}
		ids := make([]string, 0, len(a.queue))
		for _, queued := range a.queue {
			if queued.UUID != "" {
				ids = append(ids, queued.UUID)
			}
		}
		if value.Request.CancelQueued {
			a.queue = nil
			return map[string]any{"still_queued": []string{}, "cancelled": ids}, nil
		}
		return map[string]any{"still_queued": ids}, nil
	case "set_model":
		model := a.defaultModel
		if len(value.Request.Model) != 0 && string(value.Request.Model) != "null" {
			if json.Unmarshal(value.Request.Model, &model) != nil {
				return nil, errors.New("set_model: model must be a string")
			}
		}
		system := a.system
		if len(value.Request.System) != 0 {
			if json.Unmarshal(value.Request.System, &system) != nil || system == "" || string(value.Request.System) == "null" {
				return nil, errors.New("set_model: system_prompt must be a non-empty string when present")
			}
		}
		if model == "default" {
			model = a.defaultModel
		}
		if a.resolveModel != nil {
			resolved, err := a.resolveModel(model)
			if err != nil {
				return nil, err
			}
			model = resolved
		}
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("set_model: model is unavailable")
		}
		if a.persist != nil {
			if err := a.persist(ctx, model, system); err != nil {
				return nil, err
			}
		}
		a.model, a.system = model, system
		return nil, nil
	default:
		return nil, errors.New("Claude Desktop remote control operation is not implemented")
	}
}

func (a *ClaudeDesktopRemoteInput) run() {
	defer close(a.done)
	defer func() { a.mu.Lock(); a.queue, a.history, a.active = nil, nil, nil; a.system = ""; a.mu.Unlock() }()
	defer a.agents.Close()
	select {
	case <-a.ctx.Done():
		return
	case <-a.ready:
	}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.wake:
		}
		for {
			a.mu.Lock()
			if a.ctx.Err() != nil || len(a.queue) == 0 {
				a.mu.Unlock()
				break
			}
			input := a.queue[0]
			a.queue[0] = remoteInputMessage{}
			a.queue = a.queue[1:]
			a.history = append(a.history, input)
			ctx, cancel := context.WithCancel(a.ctx)
			a.active = cancel
			model := a.model
			system := a.system
			a.mu.Unlock()
			caller := claudetasks.Caller{PromptID: uuid.NewString(), Model: model}
			ctx = claudetasks.WithCaller(ctx, caller)
			if input.TaskNotification {
				var text string
				_ = json.Unmarshal(input.Content, &text)
				ctx = claudeprompt.WithSDKTaskNotification(ctx, text)
			} else if input.Meta != nil {
				ctx = claudeprompt.WithSDKMetaInput(ctx, claudeprompt.SDKMetaInput{Text: input.Meta.Text, Origin: input.Meta.Origin})
			}
			err := a.runTurn(ctx, model, system, caller)
			a.mu.Lock()
			if err == nil {
				err = ctx.Err()
			}
			a.active = nil
			a.mu.Unlock()
			cancel()
			if a.outcome != nil {
				a.outcome(err)
			}
		}
	}
}

// runTurn consumes actual tool results before continuing inference. The actor
// still serializes main turns; background tasks belong to its query lifetime.
func (a *ClaudeDesktopRemoteInput) runTurn(ctx context.Context, model, system string, caller claudetasks.Caller) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		history := append([]remoteInputMessage(nil), a.history...)
		rows := make([]json.RawMessage, len(history))
		var err error
		for i := range history {
			if history[i].TaskNotification {
				var text string
				_ = json.Unmarshal(history[i].Content, &text)
				history[i].Content, _ = json.Marshal(claudeprompt.SDKTaskNotificationWire(text))
			} else if history[i].Meta != nil {
				history[i].Content, _ = json.Marshal(claudetasks.FreshTurnWire(*history[i].Meta))
			}
			// remoteInputMessage serializes only the API row: role and content.
			if rows[i], err = json.Marshal(history[i]); err != nil {
				a.mu.Unlock()
				return err
			}
		}
		request := map[string]any{"model": model, "stream": true, "messages": rows}
		if system != "" {
			request["system"] = system
		}
		turnCtx, timing := WithClaudeDesktopAttachmentTiming(ctx)
		if a.agents != nil {
			prepared, err := ClaudeDesktopOwnedRequestToolsForRuntime(a.agents, model).Timed(timing).Prepare(rows)
			if err != nil {
				a.mu.Unlock()
				return err
			}
			if prepared.Announced >= 0 && prepared.Announced < len(a.history) {
				// The announcement is part of the sent wire history. Persist the
				// merged row (already projected, so the provenance flags are
				// consumed) so the next build recovers it and stays silent.
				a.history[prepared.Announced] = remoteInputMessage{Role: "user", Content: prepared.AnnouncedContent, UUID: a.history[prepared.Announced].UUID}
			}
			request["messages"], request["tools"] = prepared.Messages, prepared.Tools
		}
		body, err := json.Marshal(request)
		a.mu.Unlock()
		if err != nil {
			return err
		}
		response, err := a.execute(turnCtx, model, body)
		if err != nil {
			return err
		}
		// A subsequent tool-result request is not another notification or meta input.
		ctx = claudeprompt.WithoutSDKInputProvenance(ctx)
		message, calls, err := claudetasks.ParseResponse(response)
		if err != nil {
			return err
		}
		a.mu.Lock()
		if ctx.Err() != nil {
			a.mu.Unlock()
			return ctx.Err()
		}
		a.history = append(a.history, remoteInputMessage{Role: message.Role, Content: message.Content})
		a.mu.Unlock()
		if len(calls) == 0 {
			return nil
		}
		results := make([]json.RawMessage, 0, len(calls))
		for _, call := range calls {
			var data json.RawMessage
			err := ctx.Err()
			if err == nil {
				if a.agents == nil {
					err = a.agentInitErr
					if err == nil {
						err = claudetasks.ErrUnavailable
					}
				} else {
					data, err = a.agents.ExecuteTool(ctx, caller, call)
				}
			}
			results = append(results, a.agents.ToolResult(call, data, err))
		}
		content, _ := json.Marshal(results)
		a.mu.Lock()
		a.history = append(a.history, remoteInputMessage{Role: "user", Content: content})
		a.mu.Unlock()
	}
}

// ClaudeDesktopOwnedRequestTools is the native request-build order of one
// owned request (a main turn or a child generation): the tool_reference filter
// (fQs) rewrites the rows, the deferred_tools_delta reminder is computed
// against the filtered rows and merged into the trailing user row the way
// consecutive native user rows merge, then the request tools are computed
// from the merged rows. Discovery reads tool_reference blocks only, so the
// announcement text never changes the tools.
type ClaudeDesktopOwnedRequestTools struct {
	FilterToolReferences  func([]json.RawMessage) []json.RawMessage
	DeferredToolsReminder func([]json.RawMessage) string
	RequestTools          func([]json.RawMessage) []json.RawMessage
}

// ClaudeDesktopOwnedRequest is one prepared owned request body: the rows to
// send, the tools array and, when the reminder was merged, the index of the
// user row that received it together with its merged content.
type ClaudeDesktopOwnedRequest struct {
	Messages         []json.RawMessage
	Tools            []json.RawMessage
	Announced        int
	AnnouncedContent json.RawMessage
}

// ClaudeDesktopOwnedRequestToolsForRuntime resolves the gates through the
// task runtime's own definition context for the request model.
func ClaudeDesktopOwnedRequestToolsForRuntime(r *claudetasks.Runtime, model string) ClaudeDesktopOwnedRequestTools {
	return ClaudeDesktopOwnedRequestTools{
		FilterToolReferences:  func(rows []json.RawMessage) []json.RawMessage { return r.FilterToolReferences(model, rows) },
		DeferredToolsReminder: func(rows []json.RawMessage) string { return r.DeferredToolsReminder(model, rows) },
		RequestTools:          func(rows []json.RawMessage) []json.RawMessage { return r.RequestTools(model, rows) },
	}
}

// ClaudeDesktopOwnedRequestToolsForContext uses one resolved definition
// context, as a child generation does for its own model.
func ClaudeDesktopOwnedRequestToolsForContext(ctx claudetasks.DefinitionContext) ClaudeDesktopOwnedRequestTools {
	return ClaudeDesktopOwnedRequestTools{
		FilterToolReferences:  func(rows []json.RawMessage) []json.RawMessage { return claudetasks.FilterToolReferences(ctx, rows) },
		DeferredToolsReminder: func(rows []json.RawMessage) string { return claudetasks.DeferredToolsReminder(ctx, rows) },
		RequestTools:          func(rows []json.RawMessage) []json.RawMessage { return claudetasks.RequestTools(ctx, rows) },
	}
}

// ClaudeDesktopAttachmentTiming records the measured run of the deferred-tools
// reminder generator of one owned request build (the native fi() timing
// around the deferred_tools_delta generator). It is installed on the request
// context before the build so the executor's telemetry hooks can read it.
type ClaudeDesktopAttachmentTiming struct {
	mu       sync.Mutex
	measured bool
	duration time.Duration
	reminder string
}

// Result returns the measured duration and the reminder the generator
// produced ("" when nothing changed); ok is false when no generator ran.
func (t *ClaudeDesktopAttachmentTiming) Result() (duration time.Duration, reminder string, ok bool) {
	if t == nil {
		return 0, "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.duration, t.reminder, t.measured
}

type claudeDesktopAttachmentTimingKey struct{}

// WithClaudeDesktopAttachmentTiming installs a fresh recorder for one request build.
func WithClaudeDesktopAttachmentTiming(ctx context.Context) (context.Context, *ClaudeDesktopAttachmentTiming) {
	timing := &ClaudeDesktopAttachmentTiming{}
	return context.WithValue(ctx, claudeDesktopAttachmentTimingKey{}, timing), timing
}

// ClaudeDesktopAttachmentTimingFromContext returns the recorder of this request build, if any.
func ClaudeDesktopAttachmentTimingFromContext(ctx context.Context) *ClaudeDesktopAttachmentTiming {
	if ctx == nil {
		return nil
	}
	timing, _ := ctx.Value(claudeDesktopAttachmentTimingKey{}).(*ClaudeDesktopAttachmentTiming)
	return timing
}

// Timed wraps the deferred-tools generator so its real run is measured into timing.
func (p ClaudeDesktopOwnedRequestTools) Timed(timing *ClaudeDesktopAttachmentTiming) ClaudeDesktopOwnedRequestTools {
	if timing == nil || p.DeferredToolsReminder == nil {
		return p
	}
	generator := p.DeferredToolsReminder
	p.DeferredToolsReminder = func(rows []json.RawMessage) string {
		started := time.Now()
		reminder := generator(rows)
		elapsed := time.Since(started)
		timing.mu.Lock()
		timing.measured, timing.duration, timing.reminder = true, elapsed, reminder
		timing.mu.Unlock()
		return reminder
	}
	return p
}

// Prepare builds the owned request from API-shaped rows. A reminder that
// cannot follow the trailing row (no row, a non-user row, or a merge the wire
// merge does not define) fails the request instead of being dropped: the
// model would otherwise be offered ToolSearch without the names it can fetch.
func (p ClaudeDesktopOwnedRequestTools) Prepare(rows []json.RawMessage) (ClaudeDesktopOwnedRequest, error) {
	result := ClaudeDesktopOwnedRequest{Messages: rows, Announced: -1}
	if p.FilterToolReferences != nil {
		result.Messages = p.FilterToolReferences(rows)
	}
	if p.DeferredToolsReminder != nil {
		if reminder := p.DeferredToolsReminder(result.Messages); reminder != "" {
			last := len(result.Messages) - 1
			if last < 0 {
				return ClaudeDesktopOwnedRequest{}, errors.New("Claude Desktop deferred-tools reminder has no request row to follow")
			}
			var row struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(result.Messages[last], &row); err != nil || row.Role != "user" {
				return ClaudeDesktopOwnedRequest{}, fmt.Errorf("Claude Desktop deferred-tools reminder requires a trailing user row, found role %q", row.Role)
			}
			text, _ := json.Marshal(reminder)
			content, err := claudeprompt.MergeSDKWireUserContent(row.Content, text)
			if err != nil {
				return ClaudeDesktopOwnedRequest{}, fmt.Errorf("Claude Desktop deferred-tools reminder does not merge into the trailing user row: %w", err)
			}
			merged, err := json.Marshal(struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}{row.Role, content})
			if err != nil {
				return ClaudeDesktopOwnedRequest{}, err
			}
			messages := make([]json.RawMessage, len(result.Messages))
			copy(messages, result.Messages)
			messages[last] = merged
			result.Messages, result.Announced, result.AnnouncedContent = messages, last, content
		}
	}
	if p.RequestTools != nil {
		result.Tools = p.RequestTools(result.Messages)
	}
	return result, nil
}

// ClaudeDesktopDeferredAnnouncements remembers, per subagent, the input row
// that carries the deferred-tools announcement. Natively that attachment row
// is part of the subagent's persisted message history. Here the task runtime
// persists a child's assistant and tool-result rows but hands the executor a
// fresh copy of its input rows for every request, so the executor restores the
// announced row before the next reminder is computed; the bodies of one
// generation then stay consistent and the reminder is announced once. A row
// that no longer matches (another generation rebuilt the history) is not
// restored and the reminder is computed again against the actual rows.
type ClaudeDesktopDeferredAnnouncements struct {
	mu   sync.Mutex
	rows map[string]announcedRow
}

type announcedRow struct {
	index            int
	original, merged json.RawMessage
}

// Prepare restores the remembered announcement for agentID, builds the owned
// request and remembers a newly merged announcement.
func (m *ClaudeDesktopDeferredAnnouncements) Prepare(agentID string, tools ClaudeDesktopOwnedRequestTools, rows []json.RawMessage) (ClaudeDesktopOwnedRequest, error) {
	m.mu.Lock()
	known, ok := m.rows[agentID]
	m.mu.Unlock()
	if ok && known.index < len(rows) && bytes.Equal(rows[known.index], known.original) {
		restored := make([]json.RawMessage, len(rows))
		copy(restored, rows)
		restored[known.index] = known.merged
		rows = restored
	}
	result, err := tools.Prepare(rows)
	if err != nil || result.Announced < 0 || result.Announced >= len(rows) {
		return result, err
	}
	m.mu.Lock()
	if m.rows == nil {
		m.rows = map[string]announcedRow{}
	}
	m.rows[agentID] = announcedRow{index: result.Announced, original: bytes.Clone(rows[result.Announced]), merged: bytes.Clone(result.Messages[result.Announced])}
	m.mu.Unlock()
	return result, nil
}
