// Package tasks owns executable SDK agent tasks, not observations of tool calls.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/transcript"
)

var (
	ErrUnavailable           = errors.New("Claude Desktop agent runtime is unavailable")
	ErrInvalid               = errors.New("Claude Desktop agent state is invalid")
	ErrStoppedByUser         = errors.New("agent was stopped by the user and was not resumed")
	ErrNativeObserverRetired = errors.New("agent native response observer is retired")
)

type Store interface {
	Load(string) ([]byte, string, error)
	Save(string, string, []byte) (string, error)
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Invocation is issued only by the task owner. It is not decoded from an HTTP
// request. A child gets its own model request identity and explicit parent.
type Invocation struct {
	AgentID, AgentType, ParentAgentID, ParentPromptID, PromptID string
	Kind, Model                                                 string
	Depth                                                       int
	Messages                                                    []Message
	BeginNativeResponse                                         func() func([]transcript.Message, string) error
}

// Transcript is an owned sidechain capability, never decoded from a request.
// AppendMetaInput records a native isMeta user row with its origin under the
// given UUID; AppendAttachment records a native attachment row.
type Transcript interface {
	AppendInput(json.RawMessage, string, time.Time) error
	AppendMetaInput(content, origin json.RawMessage, uuid, promptID string, at time.Time) error
	AppendAttachment(attachment json.RawMessage, at time.Time) error
	Observe([]transcript.Message, string) error
	VerifyResponse([]byte) error
	Leaf() string
	Failure() error
	Flush() error
	Path() string
	ReadTail(int64) (string, error)
	Close() error
}

type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// MessageID is the id of the assistant message that carried this
	// tool_use block (native messageID telemetry dimension); empty when the
	// call was not parsed from an API response.
	MessageID string `json:"message_id,omitempty"`
}

// Event is emitted after the corresponding real state transition. Error and
// transcript contents must not be copied to health/status responses.
type Event struct {
	ID                                                        string
	Generation                                                uint64
	Kind                                                      string
	TaskID, ToolUseID, Description, AgentType, Prompt, Status string
	Background                                                bool
	Depth                                                     int
	At                                                        time.Time
	Content                                                   json.RawMessage
	Tokens                                                    int64
	ToolUses                                                  int
	DurationMS                                                int64
	UsageKnown                                                bool
	Error                                                     string
	StoppedByUser                                             bool
}

type Options struct {
	Scope         string
	DeliveryScope string
	Store         Store
	Execute       func(context.Context, Invocation) ([]byte, error)
	ResolveModel  func(parent, selected, agentType string) (string, error)
	Observe       func(context.Context, Event) error
	Notify        func(Event) error
	// MessageMain queues a subagent's wrapped message for the main
	// conversation's next turn as a native meta input with its origin.
	MessageMain func(MainDelivery) error
	// MainAgentID is the native main agent identity (the SDK session ID) used
	// for the "main" candidate ref. Refs stay deterministic when it is empty.
	MainAgentID   string
	Now           func() time.Time
	MaxDepth      func() int
	MaxConcurrent int
	// HandbackProvenance is resolved by the owning account and SDK Host.
	HandbackProvenance func() bool
	OpenTranscript     func(agentID, expectedLeaf string) (Transcript, error)
	// TaskOutputMaxLength resolves the native TASK_MAX_OUTPUT_LENGTH value
	// (UTF-16 units) for TaskOutput truncation. The caller parses the
	// environment; values <= 0 (unset or unparsable) select the native
	// default 32000 and values above 160000 are capped. nil keeps the default.
	TaskOutputMaxLength func() int
	// Definitions resolves the tool catalog gates for one request model from
	// the owning account and SDK Host; nil keeps the Desktop worker defaults.
	Definitions func(model string) DefinitionContext
	// ToolSearchOutcome observes one completed ToolSearch call (native
	// tengu_tool_search_outcome) with the tool_use input and the data object;
	// nil disables the observation. It runs for main turns and child loops.
	ToolSearchOutcome func(ctx context.Context, caller Caller, input, data json.RawMessage)
	// ToolExecuted observes every ExecuteTool completion (success or error)
	// for the owned tool family with the native tool name, the tool_use
	// input, the data object (nil on error), the error and the wall-clock
	// duration; nil disables the observation. It runs for main turns and
	// child loops and must not block.
	ToolExecuted func(ctx context.Context, caller Caller, call ToolCall, data json.RawMessage, err error, duration time.Duration)
	// ToolProgress observes every non-heartbeat progress item an owned tool
	// yields during call (native tengu_tool_use_progress); TaskOutput in
	// block mode yields exactly one waiting_for_task item before waiting.
	// nil disables the observation; the hook must not block.
	ToolProgress func(ctx context.Context, caller Caller, call ToolCall, data ToolProgress)
	// SubagentEnd observes the completion of an owned child generation that
	// produced its result (native DMt finalize); nil disables it. It runs on
	// the child's run goroutine before the launching Agent call resolves and
	// must not block.
	SubagentEnd func(ctx context.Context, end SubagentEnd)
}

type Caller struct {
	AgentID, PromptID, Model string
	Depth                    int
}

type Snapshot struct {
	ID                string `json:"id"`
	Name              string `json:"name,omitempty"`
	AgentType         string `json:"agent_type"`
	Status            string `json:"status"`
	Model             string `json:"model"`
	Background        bool   `json:"is_backgrounded"`
	Depth             int    `json:"spawn_depth"`
	Generation        uint64 `json:"generation"`
	Queued            int    `json:"queued_messages"`
	StoppedByUser     bool   `json:"stopped_by_user"`
	Resuming          bool   `json:"resuming"`
	PersistenceFailed bool   `json:"persistence_failed"`
	DeliveryFailed    bool   `json:"delivery_failed"`
	PendingEvents     int    `json:"pending_events"`
}

// Health exposes counts only. Task IDs, prompts, tool arguments, history and
// error text belong to the protected task store, not management diagnostics.
type Health struct {
	Queries              int `json:"queries"`
	InitializationFailed int `json:"initialization_failed"`
	Total                int `json:"total"`
	Running              int `json:"running"`
	Completed            int `json:"completed"`
	Failed               int `json:"failed"`
	Killed               int `json:"killed"`
	PendingEvents        int `json:"pending_events"`
	PersistenceFailed    int `json:"persistence_failed"`
	DeliveryFailed       int `json:"delivery_failed"`
}

func (h *Health) Observe(rows []Snapshot, initErr error) {
	h.Queries++
	if initErr != nil {
		h.InitializationFailed++
	}
	for _, row := range rows {
		h.Total++
		switch row.Status {
		case "running":
			h.Running++
		case "completed":
			h.Completed++
		case "failed":
			h.Failed++
		case "killed":
			h.Killed++
		}
		h.PendingEvents += row.PendingEvents
		if row.PersistenceFailed {
			h.PersistenceFailed++
		}
		if row.DeliveryFailed {
			h.DeliveryFailed++
		}
	}
}

type record struct {
	ID                string           `json:"id"`
	Name              string           `json:"name,omitempty"`
	ToolUseID         string           `json:"tool_use_id"`
	LaunchKey         string           `json:"launch_key"`
	LaunchDigest      string           `json:"launch_digest"`
	AgentType         string           `json:"agent_type"`
	Description       string           `json:"description"`
	Prompt            string           `json:"prompt"`
	Model             string           `json:"model"`
	ParentAgentID     string           `json:"parent_agent_id,omitempty"`
	ParentPromptID    string           `json:"parent_prompt_id"`
	PromptID          string           `json:"prompt_id"`
	Kind              string           `json:"invocation_kind"`
	Depth             int              `json:"spawn_depth"`
	Background        bool             `json:"is_backgrounded"`
	Status            string           `json:"status"`
	Generation        uint64           `json:"generation"`
	UserStopCount     uint64           `json:"user_stop_count"`
	StoppedByUser     bool             `json:"stopped_by_user"`
	Messages          []Message        `json:"messages"`
	Pending           []pendingMessage `json:"pending_messages,omitempty"`
	Result            json.RawMessage  `json:"result,omitempty"`
	ResultSections    resultSections   `json:"result_sections"`
	StartedAt         time.Time        `json:"started_at"`
	FinishedAt        time.Time        `json:"finished_at,omitempty"`
	Tokens            int64            `json:"tokens"`
	ToolUses          int              `json:"tool_uses"`
	UsageKnown        bool             `json:"usage_known"`
	DeliveryFailed    bool             `json:"delivery_failed,omitempty"`
	Error             string           `json:"error,omitempty"`
	KilledBy          string           `json:"killed_by,omitempty"`
	Notified          bool             `json:"notified,omitempty"`
	TranscriptStarted bool             `json:"transcript_started,omitempty"`
	TranscriptLeaf    string           `json:"transcript_leaf,omitempty"`
	TranscriptFailed  bool             `json:"transcript_failed,omitempty"`
}

type persisted struct {
	Version         int                `json:"version"`
	Scope           string             `json:"scope"`
	Tasks           []record           `json:"tasks"`
	NextEvent       uint64             `json:"next_event,omitempty"`
	Outbox          []delivery         `json:"outbox,omitempty"`
	SendMessagePins map[string]sendPin `json:"send_message_pins,omitempty"`
}
