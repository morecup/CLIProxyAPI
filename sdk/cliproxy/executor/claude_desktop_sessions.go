package executor

import (
	"context"
	"encoding/json"
	"time"
)

type ClaudeDesktopLocalStart struct {
	Model   string `json:"model"`
	Folder  string `json:"folder"`
	Message string `json:"message"`
}

type ClaudeDesktopLocalInput struct {
	ExpectedGeneration string `json:"expected_generation"`
	Message            string `json:"message"`
}

type ClaudeDesktopLocalMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ClaudeDesktopLocalView struct {
	InitialMessage string                      `json:"initial_message,omitempty"`
	Session        ClaudeDesktopSession        `json:"session"`
	Model          string                      `json:"model"`
	Folder         string                      `json:"folder"`
	Messages       []ClaudeDesktopLocalMessage `json:"messages"`
	Busy           bool                        `json:"busy"`
	LastError      string                      `json:"last_error,omitempty"`
	PromptID       string                      `json:"prompt_id,omitempty"`
	AssistantID    string                      `json:"assistant_id,omitempty"`
}

// UI observations are explicit browser measurements, never inferred from an
// upstream first byte or an API attachment. Account and model identity are not
// caller-supplied telemetry properties. Kinds are deliberately narrow:
// input_ready, first_text, switch_started, switch_painted,
// sidebar_session_opened, transcript_open_settled,
// pending_turn_stuck_idle, sessions_watch_demand_suppressed, and
// sessions_watch_demand_restored. Watch-demand duration and tags are owned by
// the server; the browser may only report a real visibility transition.
type ClaudeDesktopUIObservation struct {
	Kind               string             `json:"kind"`
	ViewID             string             `json:"view_id"`
	SessionID          string             `json:"session_id,omitempty"`
	ExpectedGeneration string             `json:"expected_generation,omitempty"`
	PromptID           string             `json:"prompt_id,omitempty"`
	AssistantID        string             `json:"assistant_id,omitempty"`
	SwitchID           string             `json:"switch_id,omitempty"`
	Metrics            map[string]float64 `json:"metrics"`
	WasHidden          bool               `json:"was_hidden"`
	CacheHit           bool               `json:"cache_hit"`
}

type ClaudeDesktopRendererRoute string

const (
	ClaudeDesktopRendererRouteNew     ClaudeDesktopRendererRoute = "new"
	ClaudeDesktopRendererRouteShell   ClaudeDesktopRendererRoute = "shell"
	ClaudeDesktopRendererRouteSession ClaudeDesktopRendererRoute = "session"
)

type ClaudeDesktopRendererTelemetryObservation struct {
	Kind            string                     `json:"kind"`
	Route           ClaudeDesktopRendererRoute `json:"route,omitempty"`
	SessionID       string                     `json:"session_id,omitempty"`
	PromptID        string                     `json:"prompt_id,omitempty"`
	ClientRequestID string                     `json:"client_request_id,omitempty"`
	Properties      map[string]any             `json:"properties"`
}

type ClaudeDesktopMainProcessTelemetryObservation struct {
	Kind            string         `json:"kind"`
	SessionID       string         `json:"session_id,omitempty"`
	ClientRequestID string         `json:"client_request_id,omitempty"`
	Metadata        map[string]any `json:"metadata"`
}

type ClaudeDesktopSDKTelemetryObservation struct {
	Kind            string         `json:"kind"`
	SessionID       string         `json:"session_id"`
	Model           string         `json:"model"`
	PromptID        string         `json:"prompt_id,omitempty"`
	ClientRequestID string         `json:"client_request_id,omitempty"`
	SkillName       string         `json:"skill_name,omitempty"`
	Metadata        map[string]any `json:"metadata"`
}

type ClaudeDesktopPerformanceTelemetryObservation struct {
	Kind            string         `json:"kind"`
	SessionID       string         `json:"session_id,omitempty"`
	ClientRequestID string         `json:"client_request_id,omitempty"`
	Data            map[string]any `json:"data"`
}

type ClaudeDesktopCrashTelemetryObservation struct {
	Filename string         `json:"filename"`
	Data     []byte         `json:"data"`
	Metadata map[string]any `json:"metadata"`
}

// ClaudeDesktopTelemetryObservationController is deliberately source-split.
// No method accepts an endpoint role or an event name; every kind is resolved
// through the version-bound Desktop Code catalog.
type ClaudeDesktopTelemetryObservationController interface {
	ObserveDesktopRendererTelemetry(context.Context, string, ClaudeDesktopRendererTelemetryObservation) error
	ObserveDesktopMainProcessTelemetry(context.Context, string, ClaudeDesktopMainProcessTelemetryObservation) error
	ObserveDesktopSDKTelemetry(context.Context, string, ClaudeDesktopSDKTelemetryObservation) error
	ObserveDesktopPerformanceTelemetry(context.Context, string, ClaudeDesktopPerformanceTelemetryObservation) error
	ObserveDesktopCrashTelemetry(context.Context, string, ClaudeDesktopCrashTelemetryObservation) error
}

type ClaudeDesktopLocalController interface {
	StartDesktopLocalSession(context.Context, string, ClaudeDesktopLocalStart) (ClaudeDesktopLocalView, error)
	GetDesktopLocalSession(string, string) (ClaudeDesktopLocalView, error)
	SendDesktopLocalMessage(context.Context, string, string, ClaudeDesktopLocalInput) (ClaudeDesktopLocalView, error)
	ResumeDesktopLocalSession(context.Context, string, ClaudeDesktopSessionResume) (ClaudeDesktopLocalView, error)
	ObserveDesktopSessionUI(context.Context, string, ClaudeDesktopUIObservation) error
}

// ClaudeDesktopSession separates durable app identity from the SDK transcript
// and the currently running query. A query ID is not a reusable session ID.
type ClaudeDesktopSession struct {
	ID                string    `json:"id"`
	Generation        string    `json:"generation"`
	SDKSessionID      string    `json:"sdk_session_id,omitempty"`
	QueryID           string    `json:"query_id,omitempty"`
	Running           bool      `json:"running"`
	CreatedAt         time.Time `json:"created_at"`
	RemoteState       string    `json:"remote_state,omitempty"`
	LocalConversation bool      `json:"local_conversation,omitempty"`
}

// Resume uses a durable observation, unlike an attachment retry on a live query.
// Neither caller history nor a replacement model/system is accepted here.
type ClaudeDesktopSessionResume struct {
	SessionID          string `json:"session_id"`
	ExpectedGeneration string `json:"expected_generation"`
}

type ClaudeDesktopResumeController interface {
	ResumeDesktopSession(context.Context, string, ClaudeDesktopSessionResume) (ClaudeDesktopSession, error)
}

// ClaudeDesktopSessionStop is an explicit Desktop operation. Neither closing a
// downstream connection nor sending slash-command text constructs this request.
type ClaudeDesktopSessionStop struct {
	SessionID       string `json:"session_id"`
	ExpectedQueryID string `json:"expected_query_id"`
}

type ClaudeDesktopSessionController interface {
	ListDesktopSessions(authID string) ([]ClaudeDesktopSession, error)
	StopDesktopSession(context.Context, string, ClaudeDesktopSessionStop) (ClaudeDesktopSession, error)
}

// ClaudeDesktopSessionHeartbeatController accepts only a visible-page tick.
// Session counts and worker classifications are server-owned.
type ClaudeDesktopSessionHeartbeatController interface {
	CheckDesktopSessionHeartbeats(context.Context, string) error
}

// Remote creation deliberately has no existing local session or user message.
type ClaudeDesktopRemoteStart struct {
	RemoteSessionID string `json:"remote_session_id"`
	Folder          string `json:"folder"`
	Model           string `json:"model"`
}

type ClaudeDesktopRemoteController interface {
	StartDesktopRemoteSession(context.Context, string, ClaudeDesktopRemoteStart) (ClaudeDesktopSession, error)
	AttachDesktopRemoteSession(context.Context, string, ClaudeDesktopSessionStop) (ClaudeDesktopSession, error)
}
