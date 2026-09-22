package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var agentNamePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")

type task struct {
	record
	cancel            context.CancelFunc
	active            *execution
	last              *execution
	resuming          bool
	persistenceFailed bool
	transcript        Transcript
	nativeInvocation  uint64
	nativeAttempt     uint64
	// parentModel is the launching caller's model (telemetry identity of the
	// Agent call); lastRequestID mirrors the last assistant row's requestId.
	parentModel   string
	lastRequestID string
}

// A foreground caller keeps its own generation's result even if SendMessage
// starts the next generation before that caller is scheduled again.
type execution struct {
	done   chan struct{}
	result record
}

// Runtime is one account/session/query's execution owner. Records survive the
// query; cancellation, goroutines and resume admissions never do.
type Runtime struct {
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	options      Options
	tasks        map[string]*task
	order        []string
	names        map[string]string
	nameOrder    []string
	pins         map[string]sendPin
	launches     map[string]string
	revision     string
	closed       bool
	outbox       []delivery
	nextEvent    uint64
	deliveryWake chan struct{}
	deliveryStop chan struct{}
	deliveryDone chan struct{}
	closeOnce    sync.Once
}

func New(ctx context.Context, options Options) (*Runtime, error) {
	if ctx == nil || options.Scope == "" || options.Store == nil || options.Execute == nil || options.ResolveModel == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxDepth == nil {
		options.MaxDepth = func() int { return 3 }
	}
	if options.MaxConcurrent == 0 {
		options.MaxConcurrent = 20
	}
	if options.MaxConcurrent < 0 {
		cancel()
		return nil, ErrInvalid
	}
	r := &Runtime{ctx: ctx, cancel: cancel, options: options, tasks: map[string]*task{}, names: map[string]string{}, launches: map[string]string{},
		deliveryWake: make(chan struct{}, 1), deliveryStop: make(chan struct{}), deliveryDone: make(chan struct{})}
	if err := r.load(); err != nil {
		for _, t := range r.tasks {
			if t.transcript != nil {
				_ = t.transcript.Close()
			}
		}
		cancel()
		return nil, err
	}
	go r.deliver()
	r.wakeDeliveryLocked()
	return r, nil
}

func (r *Runtime) load() error {
	raw, revision, err := r.options.Store.Load(r.options.Scope)
	if err != nil {
		return err
	}
	r.revision = revision
	if len(raw) == 0 {
		if revision != "" {
			return ErrInvalid
		}
		return nil
	}
	var value persisted
	if json.Unmarshal(raw, &value) != nil || value.Version != 1 || value.Scope != r.options.Scope || len(value.Tasks) > 4096 {
		return ErrInvalid
	}
	for _, item := range value.Tasks {
		if !isAgentID(item.ID) || item.AgentType != "general-purpose" || item.Model == "" ||
			item.Depth < 1 || item.Generation == 0 || item.ParentPromptID == "" ||
			!slices.Contains([]string{"running", "completed", "failed", "killed"}, item.Status) ||
			r.tasks[item.ID] != nil || item.LaunchKey == "" || item.LaunchDigest == "" ||
			(item.Name != "" && (!agentNamePattern.MatchString(item.Name) || item.Name == "main")) ||
			r.launches[item.LaunchKey] != "" || !validPromptID(item.PromptID) || !validPromptID(item.ParentPromptID) {
			return ErrInvalid
		}
		if item.Status == "running" {
			// An interrupted process did not complete the agent. A later
			// SendMessage must acquire a new generation before execution.
			item.Status = "failed"
		}
		t := &task{record: item}
		if r.options.OpenTranscript != nil {
			if item.TranscriptStarted && !item.TranscriptFailed {
				t.transcript, err = r.options.OpenTranscript(item.ID, item.TranscriptLeaf)
				t.TranscriptFailed = err != nil
			} else {
				t.TranscriptFailed = true
			}
		}
		r.tasks[item.ID] = t
		r.order = append(r.order, item.ID)
		r.launches[item.LaunchKey] = item.ID
		r.registerNameLocked(item.Name, item.ID)
	}
	for key, pin := range value.SendMessagePins {
		if key == "" || pin.ID == "" || pin.Name == "" || len(pin.Ref) != refLength {
			return ErrInvalid
		}
	}
	r.pins = value.SendMessagePins
	r.nextEvent, r.outbox = value.NextEvent, value.Outbox
	return r.validateOutbox()
}

// registerNameLocked keeps native latest-wins name binding while remembering
// the first registration order used by candidate listings.
func (r *Runtime) registerNameLocked(name, id string) {
	if name == "" {
		return
	}
	if _, known := r.names[name]; !known {
		r.nameOrder = append(r.nameOrder, name)
	}
	r.names[name] = id
}

func (r *Runtime) saveLocked() error {
	value := persisted{Version: 1, Scope: r.options.Scope, NextEvent: r.nextEvent, Outbox: r.outbox, SendMessagePins: r.pins}
	for _, id := range r.order {
		value.Tasks = append(value.Tasks, r.tasks[id].record)
	}
	raw, err := json.Marshal(value)
	if err == nil && len(raw) > 32<<20 {
		err = ErrInvalid
	}
	if err == nil {
		var next string
		next, err = r.options.Store.Save(r.options.Scope, r.revision, raw)
		if err == nil {
			r.revision = next
		}
	}
	return err
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		for _, t := range r.tasks {
			if t.cancel != nil {
				t.cancel()
			}
		}
		r.mu.Unlock()
		r.wg.Wait()
		r.mu.Lock()
		for _, t := range r.tasks {
			if t.transcript != nil && t.transcript.Close() != nil {
				t.TranscriptFailed = true
			}
		}
		if r.saveLocked() != nil {
			for _, t := range r.tasks {
				t.persistenceFailed = true
			}
		}
		r.mu.Unlock()
		close(r.deliveryStop)
		<-r.deliveryDone
	})
}

func (r *Runtime) Snapshots() []Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Snapshot, 0, len(r.order))
	for _, id := range r.order {
		t := r.tasks[id]
		pending := 0
		for _, item := range r.outbox {
			if item.Event.TaskID == id {
				pending++
			}
		}
		result = append(result, Snapshot{ID: t.ID, Name: t.Name, AgentType: t.AgentType, Status: t.Status, Model: t.Model,
			Background: t.Background, Depth: t.Depth, Generation: t.Generation, Queued: len(t.Pending),
			StoppedByUser: t.StoppedByUser, Resuming: t.resuming, PersistenceFailed: t.persistenceFailed || t.TranscriptFailed || (t.transcript != nil && t.transcript.Failure() != nil), DeliveryFailed: t.DeliveryFailed, PendingEvents: pending})
	}
	return result
}

func (r *Runtime) ExecuteTool(ctx context.Context, caller Caller, call ToolCall) (json.RawMessage, error) {
	if r == nil || ctx == nil || call.ID == "" {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.options.ToolExecuted == nil {
		return r.executeTool(ctx, caller, call)
	}
	startedAt := time.Now()
	data, err := r.executeTool(ctx, caller, call)
	r.options.ToolExecuted(ctx, caller, call, data, err, time.Since(startedAt))
	return data, err
}

func (r *Runtime) executeTool(ctx context.Context, caller Caller, call ToolCall) (json.RawMessage, error) {
	// Native lookup accepts a tool's aliases (Task, KillShell, BashOutput...)
	// and dispatches to the same implementation.
	name, _ := CanonicalToolName(call.Name)
	switch name {
	case "Agent":
		return r.launch(ctx, caller, call)
	case "SendMessage":
		return r.sendMessage(ctx, caller, call.Input)
	case "TaskStop":
		return r.stopTool(ctx, caller, call.Input)
	case "TaskOutput":
		return r.outputTool(withProgressCall(ctx, caller, call), caller, call.Input)
	case ToolSearchName:
		// ToolSearch has no aliases; it searches the pool the caller's model
		// gates resolve, like the request tools do.
		data, err := toolSearch(r.definitionContext(caller.Model), call.Input)
		if err == nil && r.options.ToolSearchOutcome != nil {
			r.options.ToolSearchOutcome(ctx, caller, call.Input, data)
		}
		return data, err
	default:
		// Tool implementations are capabilities. No prompt or recorded tool
		// name authorizes filesystem, shell, network or plugin execution.
		return nil, fmt.Errorf("tool %q has no authorized implementation in this query", call.Name)
	}
}

func (r *Runtime) launch(ctx context.Context, caller Caller, call ToolCall) (json.RawMessage, error) {
	var input struct {
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
		Name        string `json:"name"`
		Type        string `json:"subagent_type"`
		Model       string `json:"model"`
		Background  *bool  `json:"run_in_background"`
		Isolation   string `json:"isolation"`
		CWD         string `json:"cwd"`
	}
	if json.Unmarshal(call.Input, &input) != nil || input.Prompt == "" || input.Description == "" ||
		(input.Name != "" && (!agentNamePattern.MatchString(input.Name) || input.Name == "main")) {
		return nil, errors.New("Agent requires a prompt, description and a valid optional name")
	}
	if input.Type == "" {
		input.Type = "general-purpose"
	}
	if input.Type != "general-purpose" {
		return nil, fmt.Errorf("Agent type %q is not available in this query", input.Type)
	}
	if input.Isolation != "" || input.CWD != "" {
		return nil, errors.New("Agent isolation and cwd require an owned workspace capability")
	}
	if caller.AgentID != "" && input.Name != "" {
		return nil, errors.New("nested agents cannot register named teammates")
	}
	if _, err := uuid.Parse(caller.PromptID); err != nil {
		return nil, ErrInvalid
	}
	if caller.Depth >= r.options.MaxDepth() {
		return nil, errors.New("Subagent nesting limit reached; complete the task directly")
	}
	model, err := r.options.ResolveModel(caller.Model, input.Model, input.Type)
	if err != nil {
		return nil, err
	}
	background := input.Background == nil || *input.Background
	description := strings.Join(strings.Fields(input.Description), " ")
	sum := sha256.Sum256(call.Input)
	key := caller.AgentID + "\x00" + caller.PromptID + "\x00" + call.ID
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil || ctx.Err() != nil {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	if id := r.launches[key]; id != "" {
		t := r.tasks[id]
		if t.LaunchDigest != hex.EncodeToString(sum[:]) {
			r.mu.Unlock()
			return nil, ErrInvalid
		}
		run := t.last
		r.mu.Unlock()
		return r.launchResult(ctx, t.ID, background, run)
	}
	running := 0
	for _, current := range r.tasks {
		if current.active != nil || current.Status == "running" || current.resuming {
			running++
		}
	}
	if running >= r.options.MaxConcurrent {
		r.mu.Unlock()
		return nil, errors.New("Concurrent subagent limit reached")
	}
	id := newAgentID()
	for id != "" && r.tasks[id] != nil {
		id = newAgentID()
	}
	if id == "" {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	t := &task{record: record{ID: id, Name: input.Name, ToolUseID: call.ID, LaunchKey: key, LaunchDigest: hex.EncodeToString(sum[:]),
		AgentType: input.Type, Description: description, Prompt: input.Prompt, Model: model, ParentAgentID: caller.AgentID,
		ParentPromptID: caller.PromptID, PromptID: uuid.NewString(), Kind: "spawn", Depth: caller.Depth + 1,
		Background: background, Status: "running", Generation: 1, UsageKnown: true, StartedAt: r.options.Now(),
		Messages: []Message{textMessage(input.Prompt)}}, parentModel: caller.Model}
	r.tasks[id] = t
	r.order = append(r.order, id)
	if r.options.OpenTranscript != nil {
		t.transcript, err = r.options.OpenTranscript(id, "")
		t.TranscriptStarted = true
		t.TranscriptFailed = err != nil
		r.appendTranscriptInputLocked(t, t.Messages[0], t.StartedAt)
	}
	r.enqueueEventLocked(t, "started", false)
	if err := r.saveLocked(); err != nil {
		r.outbox = r.outbox[:len(r.outbox)-1]
		if t.transcript != nil {
			_ = t.transcript.Close()
		}
		delete(r.tasks, id)
		r.order = r.order[:len(r.order)-1]
		r.mu.Unlock()
		return nil, err
	}
	r.launches[key] = id
	r.registerNameLocked(t.Name, id)
	run := r.startLocked(ctx, t)
	r.mu.Unlock()
	return r.launchResult(ctx, id, background, run)
}

func (r *Runtime) startLocked(caller context.Context, t *task) *execution {
	ctx, cancel := context.WithCancel(r.ctx)
	unwatch := func() bool { return true }
	if !t.Background {
		unwatch = context.AfterFunc(caller, cancel)
		if caller.Err() != nil {
			cancel()
		}
	}
	run := &execution{done: make(chan struct{})}
	t.cancel, t.active, t.last = cancel, run, run
	generation, id := t.Generation, t.ID
	r.wakeDeliveryLocked()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(run.done)
		defer cancel()
		defer unwatch()
		r.run(ctx, id, generation, run)
	}()
	return run
}

func (r *Runtime) launchResult(ctx context.Context, id string, background bool, run *execution) (json.RawMessage, error) {
	if !background && run != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-run.done:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil {
		return nil, ErrUnavailable
	}
	if background {
		path := ""
		if t.transcript != nil && !t.TranscriptFailed {
			path = t.transcript.Path()
		}
		return marshal(map[string]any{"status": "async_launched", "isAsync": true, "agentId": t.ID, "description": t.Description,
			"resolvedModel": t.Model, "prompt": t.Prompt, "canReadOutputFile": path != "", "outputFile": path})
	}
	result := t.record
	if run != nil {
		result = run.result
	}
	if result.Status != "completed" {
		return nil, fmt.Errorf("Agent %s stopped with status %s", id, result.Status)
	}
	return marshal(map[string]any{"status": "completed", "agentId": id, "agentType": result.AgentType, "prompt": result.Prompt,
		"content": json.RawMessage(result.Result), "resolvedModel": result.Model, "totalToolUseCount": result.ToolUses,
		"harnessNoteCount": result.ResultSections.NoteCount, "harnessTailCount": result.ResultSections.TailCount, "harnessSectionHash": result.ResultSections.Hash,
		"totalDurationMs": result.FinishedAt.Sub(result.StartedAt).Milliseconds(), "totalTokens": result.Tokens})
}

func (r *Runtime) Stop(id string, byUser bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil {
		return errors.New("task not found in this session")
	}
	if r.closed {
		return ErrUnavailable
	}
	if byUser {
		t.StoppedByUser = true
		t.UserStopCount++
	}
	if t.Status == "running" || t.resuming {
		if byUser {
			t.KilledBy = "user"
		} else {
			t.KilledBy = "parent"
		}
	}
	if t.cancel != nil {
		t.cancel()
	}
	t.Pending = nil
	if t.Status == "running" || t.resuming {
		t.Status = "killed"
	}
	err := r.saveLocked()
	if err != nil {
		t.persistenceFailed = true
	}
	return err
}

func eventOf(t *task, kind string, at time.Time) Event {
	return Event{ID: uuid.NewString(), Generation: t.Generation, Kind: kind, TaskID: t.ID, ToolUseID: t.ToolUseID, Description: t.Description, AgentType: t.AgentType,
		Prompt: t.Prompt, Status: t.Status, Background: t.Background, Depth: t.Depth, At: at,
		Content: bytes.Clone(t.Result), Tokens: t.Tokens, ToolUses: t.ToolUses,
		DurationMS: at.Sub(t.StartedAt).Milliseconds(), UsageKnown: t.UsageKnown,
		Error: t.Error, StoppedByUser: t.StoppedByUser}
}

func validPromptID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil
}

func marshal(value any) (json.RawMessage, error) { raw, err := json.Marshal(value); return raw, err }
func textMessage(text string) Message {
	raw, _ := json.Marshal(text)
	return Message{Role: "user", Content: raw}
}
func cloneMessages(input []Message) []Message {
	result := make([]Message, len(input))
	for i, row := range input {
		result[i] = Message{Role: row.Role, Content: bytes.Clone(row.Content)}
	}
	return result
}
