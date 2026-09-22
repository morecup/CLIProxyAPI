package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ShellTelemetryEvent is a completion reported by the in-process shell owner.
// Metadata contains only the native, content-free execution measurements; raw
// commands and stdout/stderr never cross this boundary.
type ShellTelemetryEvent struct {
	Name     string
	Model    string
	PromptID string
	Metadata json.RawMessage
}

// SetShellTelemetryObserver binds the live query's telemetry consumer. The
// observer is process-local capability state and is never persisted.
func (h *Host) SetShellTelemetryObserver(observer func(context.Context, ShellTelemetryEvent) error) error {
	if h == nil {
		return fmt.Errorf("Claude Desktop SDK host is unavailable")
	}
	h.mu.RLock()
	available := h.ctx.Err() == nil
	h.mu.RUnlock()
	if !available {
		return fmt.Errorf("Claude Desktop SDK host is unavailable")
	}
	h.shellTelemetryMu.Lock()
	h.shellTelemetryObserver = observer
	h.shellTelemetryMu.Unlock()
	return nil
}

// RecordShellTelemetry reports one actually completed shell command. Callers
// must provide the real executor result rather than reconstructing it from a
// later Messages request.
func (h *Host) RecordShellTelemetry(event ShellTelemetryEvent) error {
	if h == nil || strings.TrimSpace(event.Name) == "" || strings.TrimSpace(event.Model) == "" || !json.Valid(event.Metadata) {
		return fmt.Errorf("Claude Desktop shell telemetry event is invalid")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(event.Metadata, &object) != nil || object == nil {
		return fmt.Errorf("Claude Desktop shell telemetry metadata is invalid")
	}
	h.mu.RLock()
	ctx := h.ctx
	available := ctx.Err() == nil
	h.mu.RUnlock()
	if !available {
		return fmt.Errorf("Claude Desktop SDK host is unavailable")
	}
	h.shellTelemetryMu.RLock()
	observer := h.shellTelemetryObserver
	h.shellTelemetryMu.RUnlock()
	if observer == nil {
		return fmt.Errorf("Claude Desktop shell telemetry observer is unavailable")
	}
	event.Name = strings.TrimSpace(event.Name)
	event.Model = strings.TrimSpace(event.Model)
	event.PromptID = strings.TrimSpace(event.PromptID)
	event.Metadata = append(json.RawMessage(nil), event.Metadata...)
	return observer(ctx, event)
}
