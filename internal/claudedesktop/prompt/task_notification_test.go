package prompt

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
)

func TestSDKTaskNotificationMatchesPinnedNative(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-task-notification-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SDKSHA string `json:"sdk_sha256"`
		Cases  []struct {
			Name, Status, Description, Result, Error, Notification, Wire string
			StoppedByUser                                                bool   `json:"stopped_by_user"`
			OutputFile                                                   string `json:"output_file"`
			PromptLength                                                 int    `json:"prompt_length"`
			Usage                                                        *struct {
				TotalTokens int64
				ToolUses    int
				DurationMs  int64
			}
		}
		WireCases []struct{ Input, Output string } `json:"wire_cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.SDKSHA != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(fixture.Cases) != 8 || len(fixture.WireCases) != 5 {
		t.Fatal("native notification fixture changed")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			content, _ := json.Marshal([]map[string]string{{"type": "text", "text": tc.Result}})
			event := claudetasks.Event{Kind: "finished", TaskID: "a0123456789abcdef", ToolUseID: "tool_synthetic", Description: tc.Description, Status: tc.Status, Content: content, Error: tc.Error, StoppedByUser: tc.StoppedByUser}
			if tc.Usage != nil {
				event.UsageKnown, event.Tokens, event.ToolUses, event.DurationMS = true, tc.Usage.TotalTokens, tc.Usage.ToolUses, tc.Usage.DurationMs
			}
			notification, err := claudetasks.FormatNotification(event, tc.OutputFile)
			if err != nil || notification != tc.Notification {
				t.Fatalf("native notification mismatch: %v\n%s\nwant\n%s", err, notification, tc.Notification)
			}
			wire := SDKTaskNotificationWire(notification)
			if wire != tc.Wire || SDKTaskNotificationWire(wire) != wire {
				t.Fatal("native provenance wrapper mismatch")
			}
			body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": wire}}})
			ctx := WithSDKTaskNotification(t.Context(), notification)
			got := ObserveSubmissionContext(ctx, body, time.Now())
			if !got.Known || got.Length != tc.PromptLength {
				t.Fatal("wire prefix contaminated input telemetry", got)
			}
			human := ObserveSubmissionContext(t.Context(), body, time.Now())
			if !human.Known || human.Length != len(utf16.Encode([]rune(wire))) {
				t.Fatal("user text acquired internal provenance")
			}
			if SDKTaskNotificationFromContext(WithoutSDKTaskNotification(ctx)) != nil {
				t.Fatal("notification leaked into tool continuation")
			}
		})
	}
	for _, tc := range fixture.WireCases {
		if got := SDKTaskNotificationWire(tc.Input); got != tc.Output {
			t.Fatalf("native closing tag normalization: %q != %q", got, tc.Output)
		}
	}
}

// A subagent's SendMessage to main is recorded as the native isMeta user row
// carrying its peer origin; the fresh-turn wire is the row content and the
// wire restoration keeps that content without exposing the origin.
func TestSDKMetaInputNativeContentProjection(t *testing.T) {
	structural, content, transcript := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}, &sdkReadableTranscriptStore{}
	tracker := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript, Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic"})
	tracker.transcript = pausedTranscriptWriter(t, transcript)
	at := time.Now().UTC().Truncate(time.Millisecond)
	wrapped := "<agent-message from=\"worker\">\nsynthetic peer <report>\n</agent-message>"
	meta := SDKMetaInput{Text: wrapped, Origin: json.RawMessage(`{"kind":"peer","from":"worker","senderTaskId":"a0123456789abcdef","name":"worker","body":"synthetic peer <report>"}`)}
	wire := claudetasks.FreshTurnWire(claudetasks.MainDelivery{Text: meta.Text, Origin: meta.Origin})
	if !strings.HasPrefix(wire, "Another Claude session sent a message:\n"+wrapped+"\n\n") {
		t.Fatalf("fresh-turn wire: %q", wire)
	}
	body, _ := sdkAttachmentJSON(map[string]any{"model": "synthetic", "messages": []map[string]string{{"role": "user", "content": wire}}})
	ctx := WithSDKMetaInput(t.Context(), meta)
	observed := ObserveSubmissionContext(ctx, body, at)
	if !observed.Known || !observed.IsMeta || observed.Length != len(utf16.Encode([]rune(wrapped))) {
		t.Fatal("meta input measured the projection instead of the queued value", observed)
	}
	if human := ObserveSubmissionContext(t.Context(), body, at); human.IsMeta || human.Length != len(utf16.Encode([]rune(wire))) {
		t.Fatal("user text acquired meta provenance", human)
	}
	if SDKMetaInputFromContext(WithoutSDKInputProvenance(ctx)) != nil || SDKTaskNotificationFromContext(WithoutSDKInputProvenance(WithSDKTaskNotification(ctx, "n"))) != nil {
		t.Fatal("meta provenance leaked into tool continuation")
	}
	in := sdkStateTestInput(string(body), at)
	in.SessionID = uuid.NewString()
	in.MetaInput = &meta
	request := tracker.Begin(in)
	request.ObserveSDKQuery(body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"type":"message","id":"msg_meta","model":"synthetic","role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`), false, at.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(at.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
	snapshot := tracker.NativeContent(in.AccountID, in.SessionID)
	if snapshot.IncompleteReason != "" || snapshot.PersistenceError || len(snapshot.Messages) != 2 {
		t.Fatal("native meta input not recorded", snapshot.IncompleteReason)
	}
	row := snapshot.Messages[0]
	var message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if !row.IsMeta || string(row.Origin) != string(meta.Origin) || json.Unmarshal(row.Message, &message) != nil || message.Role != "user" || message.Content != wire {
		t.Fatalf("native meta row: isMeta=%v origin=%s message=%s", row.IsMeta, row.Origin, row.Message)
	}
	if snapshot.Messages[1].IsMeta || len(snapshot.Messages[1].Origin) != 0 {
		t.Fatal("assistant row inherited meta provenance")
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	resume, err := restarted.PrepareRemoteWireResume(in.AccountID, in.SessionID)
	if err != nil {
		t.Fatal("meta restart failed", err)
	}
	rows, err := resume.Read()
	if err != nil || len(rows) != 2 {
		t.Fatal("meta wire restoration failed", err)
	}
	var restored struct{ Content string }
	if json.Unmarshal(rows[0], &restored) != nil || restored.Content != wire || bytes.Contains(rows[0], []byte(`"origin"`)) || bytes.Contains(rows[0], []byte(`"isMeta"`)) {
		t.Fatal("raw provenance leaked or meta projection lost on restart")
	}
}

func TestSDKTaskNotificationNativeContentRestartProjection(t *testing.T) {
	structural, content, transcript := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}, &sdkReadableTranscriptStore{}
	tracker := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript, Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic"})
	tracker.transcript = pausedTranscriptWriter(t, transcript)
	at := time.Now().UTC().Truncate(time.Millisecond)
	text := "<task-notification>synthetic child result</task-notification>"
	wire := SDKTaskNotificationWire(text)
	body, _ := json.Marshal(map[string]any{"model": "synthetic", "messages": []map[string]string{{"role": "user", "content": wire}}})
	in := sdkStateTestInput(string(body), at)
	in.SessionID = uuid.NewString()
	in.TaskNotification = &text
	request := tracker.Begin(in)
	request.ObserveSDKQuery(body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"type":"message","id":"msg_notification","model":"synthetic","role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`), false, at.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(at.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
	snapshot := tracker.NativeContent(in.AccountID, in.SessionID)
	if snapshot.IncompleteReason != "" || snapshot.PersistenceError || len(snapshot.Messages) != 2 {
		t.Fatal("native notification not recorded", snapshot.IncompleteReason)
	}
	row := snapshot.Messages[0]
	if string(row.Origin) != `{"kind":"task-notification"}` || bytes.Contains(row.Message, []byte("SYSTEM NOTIFICATION")) {
		t.Fatal("native transcript lost raw origin/content")
	}
	row.Origin[0] = 'X'
	if tracker.NativeContent(in.AccountID, in.SessionID).Messages[0].Origin[0] != '{' {
		t.Fatal("snapshot mutated owned origin")
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	resume, err := restarted.PrepareRemoteWireResume(in.AccountID, in.SessionID)
	if err != nil {
		t.Fatal("notification restart failed", err)
	}
	rows, err := resume.Read()
	if err != nil || len(rows) != 2 {
		t.Fatal("notification wire restoration failed", err)
	}
	var restored struct{ Content string }
	if json.Unmarshal(rows[0], &restored) != nil || restored.Content != wire || bytes.Contains(rows[0], []byte(`"origin"`)) {
		t.Fatal("raw provenance leaked or notification wrapper lost on restart")
	}
}
