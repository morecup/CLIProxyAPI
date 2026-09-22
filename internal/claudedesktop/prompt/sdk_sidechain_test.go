package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sidechainTestStore struct{ sdkReadableTranscriptStore }

func (s *sidechainTestStore) PrepareTaskOutput(scope string) (string, error) {
	return "synthetic/" + scope + ".output", nil
}
func (s *sidechainTestStore) ReadTaskOutput(scope string, _ int64) (string, error) {
	var body []byte
	_, err := s.ReadTranscript(scope, func(lines []byte) error { body = append(body, lines...); return nil })
	return string(body), err
}
func sidechainTestOwner(t *testing.T) (*Tracker, *sidechainTestStore) {
	t.Helper()
	store := &sidechainTestStore{}
	tracker := NewTracker(nil, SDKNativeContentOptions{TranscriptStore: store, Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic"})
	tracker.transcript = pausedTranscriptWriter(t, store)
	return &tracker, store
}
func sidechainRows(t *testing.T, s *SDKSidechain) []SDKNativeMessage {
	t.Helper()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	text, err := s.ReadTail(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	var rows []SDKNativeMessage
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var row SDKNativeMessage
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal("invalid JSONL")
		}
		rows = append(rows, row)
	}
	return rows
}
func TestSDKSidechainOwnedContentLifecycle(t *testing.T) {
	tracker, store := sidechainTestOwner(t)
	session, prompt := uuid.NewString(), uuid.NewString()
	s, err := tracker.OpenSidechain("owned-account", session, "a12345678", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.OpenSidechain("owned-account", session, "a12345678", ""); !errors.Is(err, ErrSDKSessionStale) {
		t.Fatal("duplicate active owner", err)
	}
	at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	if err := s.AppendInput(json.RawMessage(`"PRIVATE_INITIAL"`), prompt, at); err != nil {
		t.Fatal(err)
	}
	user := s.Leaf()
	var response Response
	response.EnableNativeContent()
	response.SetNativeRequestID("actual-request")
	response.ObservePayloadAt([]byte(`{"id":"msg_actual","type":"message","role":"assistant","model":"sonnet","content":[{"type":"thinking","thinking":"PRIVATE_THINKING","signature":"PRIVATE_SIGNATURE"},{"type":"tool_use","id":"tool_actual","name":"TaskOutput","input":{"task_id":"aabcdefgh"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":7}}`), false, at.Add(time.Second))
	if err := s.Observe(response.NativeContentMessages()); err != nil {
		t.Fatal(err)
	}
	assistant := s.Leaf()
	if err := s.AppendInput(json.RawMessage(`[{"type":"tool_result","tool_use_id":"tool_actual","content":"PRIVATE_RESULT"}]`), prompt, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	rows := sidechainRows(t, s)
	if len(rows) != 3 || rows[0].ParentUUID != nil || *rows[1].ParentUUID != user || *rows[2].ParentUUID != assistant || rows[2].SourceToolAssistantUUID != assistant {
		t.Fatal("native chain or tool-result ancestry changed")
	}
	for _, row := range rows {
		if !row.IsSidechain || row.AgentID != "a12345678" || row.SessionID != session || row.Cwd != "C:/synthetic" || row.Version != "2.1.247" {
			t.Fatal("wrong owned metadata")
		}
	}
	if rows[0].PromptID != prompt || rows[1].PromptID != "" || rows[1].RequestID != "actual-request" || !bytes.Contains(rows[1].Message, []byte("PRIVATE_SIGNATURE")) {
		t.Fatal("raw native content/identity lost")
	}
	if len(tracker.prompts) != 0 || len(tracker.nativeContent) != 0 {
		t.Fatal("sidechain polluted main history")
	}
	leaf, path := s.Leaf(), s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Observe(nil, "") == nil {
		t.Fatal("closed sidechain accepted stale callback")
	}
	if _, err := tracker.OpenSidechain("owned-account", session, "a12345678", user); err == nil {
		t.Fatal("unreconciled checkpoint accepted")
	}
	restored, err := tracker.OpenSidechain("owned-account", session, "a12345678", leaf)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Path() != path || len(sidechainRows(t, restored)) != 3 {
		t.Fatal("restore fabricated or lost entries")
	}
	if err := restored.AppendInput(json.RawMessage(`"PRIVATE_RESUME"`), uuid.NewString(), at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sidechainRows(t, restored); len(got) != 4 || *got[3].ParentUUID != leaf {
		t.Fatal("resume lost original leaf")
	}
	store.mu.Lock()
	count := len(store.appends)
	store.mu.Unlock()
	if count != 2 {
		t.Fatal("unexpected backfill/redelivery", count)
	}
}

// A delivered SendMessage records the native queued_command attachment and
// the isMeta user row under the attachment's source UUID with its origin;
// the row keeps raw wrapper characters and chains after the attachment.
func TestSDKSidechainMetaInputAndAttachmentRows(t *testing.T) {
	tracker, _ := sidechainTestOwner(t)
	session, prompt := uuid.NewString(), uuid.NewString()
	if _, err := tracker.OpenSidechain("owned-account", session, "not-an-agent-id", ""); err == nil {
		t.Fatal("foreign agent identity accepted")
	}
	s, err := tracker.OpenSidechain("owned-account", session, "a0123456789abcdef", "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if err := s.AppendInput(json.RawMessage(`"PRIVATE_INITIAL"`), prompt, at); err != nil {
		t.Fatal(err)
	}
	source := uuid.NewString()
	origin := json.RawMessage(`{"kind":"peer","from":"worker","senderTaskId":"a89abcdef0123456","name":"worker","body":"hi <b> & 'q'"}`)
	wrapped := "<agent-message from=\"worker\">\nhi <b> & 'q'\n</agent-message>"
	attachment, _ := json.Marshal(map[string]any{"type": "queued_command", "prompt": wrapped, "source_uuid": source, "origin": origin, "isMeta": true})
	if err := s.AppendAttachment(json.RawMessage(attachment), at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	content, _ := sdkAttachmentJSON("<system-reminder>\nAnother Claude session sent a message while you were working:\n" + wrapped + "\n</system-reminder>")
	for _, bad := range [][2]json.RawMessage{{json.RawMessage(`"x"`), json.RawMessage(`{`)}, {json.RawMessage(`{`), origin}} {
		if err := s.AppendMetaInput(bad[0], bad[1], uuid.NewString(), prompt, at); !errors.Is(err, ErrSDKSessionInvalid) {
			t.Fatal("invalid meta row accepted", err)
		}
	}
	if err := s.AppendAttachment(json.RawMessage(`[`), at); !errors.Is(err, ErrSDKSessionInvalid) {
		t.Fatal("invalid attachment accepted", err)
	}
	if err := s.AppendMetaInput(json.RawMessage(content), origin, source, prompt, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows := sidechainRows(t, s)
	if len(rows) != 3 || rows[1].Type != "attachment" || *rows[1].ParentUUID != rows[0].UUID || rows[1].Message != nil || rows[1].PromptID != "" || !bytes.Equal(rows[1].Attachment, attachment) {
		t.Fatalf("attachment row: %+v", rows[1])
	}
	meta := rows[2]
	if meta.Type != "user" || meta.UUID != source || *meta.ParentUUID != rows[1].UUID || !meta.IsMeta || !bytes.Equal(meta.Origin, origin) || meta.PromptID != prompt || string(meta.Message) != `{"role":"user","content":`+string(content)+`}` {
		t.Fatalf("meta row: %+v", meta)
	}
	for _, row := range rows {
		if !row.IsSidechain || row.AgentID != "a0123456789abcdef" || row.SessionID != session {
			t.Fatal("owned metadata lost on native rows")
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	text, err := s.ReadTail(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"isMeta":true`) || strings.Contains(lines[2], `\u003c`) || !strings.Contains(lines[2], `"origin":`+string(origin)) || strings.Contains(lines[0], `"isMeta"`) {
		t.Fatalf("meta row serialization: %s", lines[2])
	}
}

func TestSDKSidechainLiveMutationAndIsolation(t *testing.T) {
	tracker, _ := sidechainTestOwner(t)
	var children []*SDKSidechain
	for _, identity := range [][3]string{{"a", "s", "a00000001"}, {"a", "s", "a00000002"}, {"b", "s", "a00000001"}, {"a", "other", "a00000001"}} {
		s, err := tracker.OpenSidechain(identity[0], identity[1], identity[2], "")
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, s)
	}
	for i, s := range children {
		if err := s.AppendInput(json.RawMessage(`"private"`), uuid.NewString(), time.Now()); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < i; j++ {
			if s.path == children[j].path || s.scope == children[j].scope {
				t.Fatal("cross-owner output alias")
			}
		}
	}
	s := children[0]
	row := SDKNativeMessage{Type: "assistant", UUID: uuid.NewString(), Timestamp: nativeContentTimestamp(time.Now()), RequestID: "req", Message: json.RawMessage(`{"id":"msg","role":"assistant","content":[{"type":"text","text":"exact"}],"usage":{"output_tokens":1}}`)}
	if err := s.Observe([]SDKNativeMessage{row}, ""); err != nil {
		t.Fatal(err)
	}
	row.Message = json.RawMessage(`{"id":"msg","role":"assistant","content":[{"type":"text","text":"exact"}],"usage":{"output_tokens":2},"stop_reason":"end_turn"}`)
	if err := s.Observe([]SDKNativeMessage{row}, ""); err != nil {
		t.Fatal(err)
	}
	first := sidechainRows(t, s)
	if len(first) != 2 || !bytes.Equal(first[1].Message, row.Message) {
		t.Fatal("deferred serialization missed live mutation")
	}
	row.Message = json.RawMessage(`{"id":"msg","role":"assistant","content":[{"type":"text","text":"exact"}],"usage":{"output_tokens":3},"stop_reason":"end_turn"}`)
	if err := s.Observe([]SDKNativeMessage{row}, ""); err != nil {
		t.Fatal(err)
	}
	second := sidechainRows(t, s)
	if len(second) != 2 || !bytes.Equal(first[1].Message, second[1].Message) {
		t.Fatal("late mutation rewrote/reappended disk row")
	}
	for _, other := range children[1:] {
		if len(sidechainRows(t, other)) != 1 {
			t.Fatal("child response crossed owner")
		}
	}
}

func TestSDKSidechainFailureAndCorruptRecovery(t *testing.T) {
	for _, mode := range []string{"append", "read", "missing-leaf", "foreign-row", "changed-content", "unknown-tool"} {
		t.Run(mode, func(t *testing.T) {
			tracker, store := sidechainTestOwner(t)
			s, err := tracker.OpenSidechain("owner", "session", "a11111111", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AppendInput(json.RawMessage(`"original"`), uuid.NewString(), time.Now()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "append":
				store.before = func(string, []byte) error { return errors.New("private disk diagnostic") }
				if s.Flush() == nil || s.Path() != "" {
					t.Fatal("late write failure hidden")
				}
				return
			case "unknown-tool":
				if s.AppendInput(json.RawMessage(`[{"type":"tool_result","tool_use_id":"foreign","content":"no"}]`), uuid.NewString(), time.Now()) == nil {
					t.Fatal("foreign tool parent accepted")
				}
				return
			case "changed-content":
				row := SDKNativeMessage{Type: "assistant", UUID: uuid.NewString(), Timestamp: nativeContentTimestamp(time.Now()), Message: json.RawMessage(`{"id":"m","role":"assistant","content":"one"}`)}
				if s.Observe([]SDKNativeMessage{row}, "") != nil {
					t.Fatal("initial response failed")
				}
				row.Message = json.RawMessage(`{"id":"m","role":"assistant","content":"two"}`)
				if s.Observe([]SDKNativeMessage{row}, "") == nil {
					t.Fatal("rewritten raw content accepted")
				}
				return
			}
			leaf := s.Leaf()
			if s.Close() != nil {
				t.Fatal("close failed")
			}
			if mode == "read" {
				store.fail = ErrSDKSessionInvalid
			}
			if mode == "missing-leaf" {
				leaf = uuid.NewString()
			}
			if mode == "foreign-row" {
				store.mutate = func(lines []byte) []byte {
					return bytes.ReplaceAll(lines, []byte(`"agentId":"a11111111"`), []byte(`"agentId":"a22222222"`))
				}
			}
			if _, err := tracker.OpenSidechain("owner", "session", "a11111111", leaf); err == nil {
				t.Fatal("bad checkpoint/protected reader accepted")
			}
		})
	}
}

func TestSDKSidechainPinnedNativeChainGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-sidechain-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Identity struct{ Session, Agent, Prompt, Version, Entrypoint, Cwd string }
		Vectors  []struct {
			Name           string
			Rows, Expected []SDKNativeMessage
		}
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Vectors) != 5 {
		t.Fatal("invalid native sidechain fixture")
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			tracker, _ := sidechainTestOwner(t)
			tracker.nativeOptions.Cwd = fixture.Identity.Cwd
			s, err := tracker.OpenSidechain("synthetic-account", fixture.Identity.Session, fixture.Identity.Agent, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range vector.Rows {
				if row.Type == "user" {
					row.PromptID = fixture.Identity.Prompt
				}
				s.mu.Lock()
				err := s.appendLocked(row)
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			got := sidechainRows(t, s)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(vector.Expected)
			var actual, expected any
			if json.Unmarshal(a, &actual) != nil || json.Unmarshal(b, &expected) != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatal("native source chain fields differ", string(a), string(b))
			}
		})
	}
}
