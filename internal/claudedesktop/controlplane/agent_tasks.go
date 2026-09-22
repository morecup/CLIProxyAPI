package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
)

// RecordAgentTask is called by actual task transitions, never response parsing.
// The exact live Desktop/query owner is rechecked before every worker write.
func (m *Manager) RecordAgentTask(ctx context.Context, desktopID, queryID string, event claudetasks.Event) error {
	if m == nil || desktopID == "" || queryID == "" {
		return errors.New("agent task query is unavailable")
	}
	m.mu.Lock()
	session := m.sessions["query:"+queryID]
	m.mu.Unlock()
	if session == nil || session.desktopID != desktopID || session.queryID != queryID {
		return errors.New("agent task query owner changed")
	}
	ctx, release := session.operationContext(ctx)
	defer release()
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if event.TaskID == "" || event.Depth < 1 || event.ID == "" {
		return errors.New("agent task identity is missing")
	}
	switch event.Kind {
	case "started":
		return session.postWorkerEventLocked(ctx, taskStartedPayload{
			Type: "system", Subtype: "task_started", TaskID: event.TaskID, ToolUseID: event.ToolUseID,
			Description: event.Description, SubagentType: event.AgentType, IsBackgrounded: event.Background,
			SpawnDepth: event.Depth, TaskType: "local_agent", Prompt: event.Prompt, UUID: event.ID, SessionID: session.state.RemoteSessionID,
		})
	case "finished":
		updated := taskUpdatedPayload{Type: "system", Subtype: "task_updated", TaskID: event.TaskID, UUID: event.ID, SessionID: session.state.RemoteSessionID}
		updated.Patch.Status, updated.Patch.EndTime = event.Status, event.At.UnixMilli()
		if err := session.postWorkerEventLocked(ctx, updated); err != nil {
			return err
		}
		if !event.Background {
			return nil
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Content, &blocks)
		var text []string
		for _, block := range blocks {
			if block.Type == "text" {
				text = append(text, block.Text)
			}
		}
		// No empty .output file is created to impersonate task execution.
		// Output-file projection and full native notification framing require
		// their real producers; retained content is served by owned TaskOutput.
		payload := struct {
			Type      string                 `json:"type"`
			Subtype   string                 `json:"subtype"`
			TaskID    string                 `json:"task_id"`
			ToolUseID string                 `json:"tool_use_id"`
			Status    string                 `json:"status"`
			Summary   string                 `json:"summary"`
			Usage     *taskNotificationUsage `json:"usage,omitempty"`
			UUID      string                 `json:"uuid"`
			SessionID string                 `json:"session_id"`
		}{Type: "system", Subtype: "task_notification", TaskID: event.TaskID, ToolUseID: event.ToolUseID, Status: event.Status,
			Summary: strings.Join(text, "\n"), UUID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(event.ID+":notification")).String(), SessionID: session.state.RemoteSessionID}
		if event.UsageKnown {
			payload.Usage = &taskNotificationUsage{TotalTokens: int(event.Tokens), ToolUses: event.ToolUses, DurationMS: int(event.DurationMS)}
		}
		return session.postWorkerEventLocked(ctx, payload)
	default:
		return errors.New("agent task transition is invalid")
	}
}
