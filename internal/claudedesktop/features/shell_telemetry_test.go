package features

import (
	"context"
	"encoding/json"
	"testing"
)

func TestShellTelemetryObserverReceivesOwnedCopy(t *testing.T) {
	host := NewHost(nil, "session-shell")
	defer host.Close()
	var captured ShellTelemetryEvent
	var capturedContext context.Context
	if err := host.SetShellTelemetryObserver(func(ctx context.Context, event ShellTelemetryEvent) error {
		capturedContext, captured = ctx, event
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	metadata := json.RawMessage(`{"command_type":"other","exit_code":0}`)
	event := ShellTelemetryEvent{Name: "  tengu_bash_tool_command_executed  ", Model: "  claude-opus-5  ", PromptID: "  prompt-shell  ", Metadata: metadata}
	if err := host.RecordShellTelemetry(event); err != nil {
		t.Fatal(err)
	}
	metadata[2] = 'X'
	if capturedContext != host.Context() || capturedContext.Err() != nil {
		t.Fatal("observer did not receive the live host context")
	}
	if captured.Name != "tengu_bash_tool_command_executed" || captured.Model != "claude-opus-5" || captured.PromptID != "prompt-shell" {
		t.Fatalf("captured event = %+v", captured)
	}
	if string(captured.Metadata) != `{"command_type":"other","exit_code":0}` {
		t.Fatalf("observer metadata changed with caller buffer: %s", captured.Metadata)
	}
}

func TestShellTelemetryObserverRejectsUnownedOrRetiredEvents(t *testing.T) {
	host := NewHost(nil, "session-shell")
	valid := ShellTelemetryEvent{Name: "tengu_bash_tool_command_executed", Model: "claude-opus-5", Metadata: json.RawMessage(`{"exit_code":0}`)}
	if err := host.RecordShellTelemetry(valid); err == nil {
		t.Fatal("event without an observer was accepted")
	}
	if err := host.SetShellTelemetryObserver(func(context.Context, ShellTelemetryEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]ShellTelemetryEvent{
		"name":      {Model: "claude-opus-5", Metadata: json.RawMessage(`{}`)},
		"model":     {Name: "tengu_bash_tool_command_executed", Metadata: json.RawMessage(`{}`)},
		"json":      {Name: "tengu_bash_tool_command_executed", Model: "claude-opus-5", Metadata: json.RawMessage(`not-json`)},
		"nonobject": {Name: "tengu_bash_tool_command_executed", Model: "claude-opus-5", Metadata: json.RawMessage(`[]`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := host.RecordShellTelemetry(event); err == nil {
				t.Fatal("invalid event was accepted")
			}
		})
	}
	host.Close()
	if err := host.RecordShellTelemetry(valid); err == nil {
		t.Fatal("retired host accepted shell telemetry")
	}
	if err := host.SetShellTelemetryObserver(func(context.Context, ShellTelemetryEvent) error { return nil }); err == nil {
		t.Fatal("retired host accepted an observer")
	}
}
