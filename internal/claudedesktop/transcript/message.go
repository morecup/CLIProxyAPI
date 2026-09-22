// Package transcript contains private native content shared by independent
// main-query and sidechain owners. It performs no host operations.
package transcript

import "encoding/json"

// Message retains actual content and native per-yield identity. Never log or
// include it in telemetry or management diagnostics.
type Message struct {
	ParentUUID              *string         `json:"parentUuid"`
	LogicalParentUUID       *string         `json:"logicalParentUuid,omitempty"`
	IsSidechain             bool            `json:"isSidechain"`
	AgentID                 string          `json:"agentId,omitempty"`
	Type                    string          `json:"type"`
	Subtype                 string          `json:"subtype,omitempty"`
	UUID                    string          `json:"uuid"`
	Timestamp               string          `json:"timestamp"`
	Message                 json.RawMessage `json:"message,omitempty"`
	IsMeta                  bool            `json:"isMeta,omitempty"`
	RequestID               string          `json:"requestId,omitempty"`
	Attachment              json.RawMessage `json:"attachment,omitempty"`
	PromptID                string          `json:"promptId,omitempty"`
	Origin                  json.RawMessage `json:"origin,omitempty"`
	SourceToolAssistantUUID string          `json:"sourceToolAssistantUUID,omitempty"`
	IsCompactSummary        bool            `json:"isCompactSummary,omitempty"`
	SessionID               string          `json:"sessionId"`
	Version                 string          `json:"version,omitempty"`
	Entrypoint              string          `json:"entrypoint,omitempty"`
	Cwd                     string          `json:"cwd,omitempty"`
	SessionKind             string          `json:"sessionKind,omitempty"`
	UserType                string          `json:"userType,omitempty"`
	GitBranch               *string         `json:"gitBranch,omitempty"`
	Slug                    *string         `json:"slug,omitempty"`
}
