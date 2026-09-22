package helps

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func TestClaudeDesktopTaskOutputActualProtectedProjection(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "owned-egress")
	tracker := claudeprompt.NewTracker(nil, claudeprompt.SDKNativeContentOptions{TranscriptStore: store, Version: "2.1.247", Entrypoint: "claude-desktop"})
	t.Cleanup(func() { _ = tracker.Close() })
	s, err := tracker.OpenSidechain("account", "session", "a12345678", "")
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if path == "" || filepath.Ext(path) != ".output" {
		t.Fatal("real output path absent")
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Fatal("admission file is not genuinely empty", err)
	}
	if err := s.AppendInput(json.RawMessage(`"PRIVATE_INITIAL"`), uuid.NewString(), time.Now()); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("long report 😀 <>& ", 5000)
	content, _ := json.Marshal(text)
	var response claudeprompt.Response
	response.EnableNativeContent()
	response.SetNativeRequestID("req_projection")
	response.ObservePayload(append(append([]byte(`{"id":"msg_projection","type":"message","role":"assistant","model":"sonnet","content":[{"type":"text","text":`), content...), []byte(`}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)...), false)
	if s.Observe(response.NativeContentMessages()) != nil || s.Flush() != nil {
		t.Fatal("genuine response projection failed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 || !bytes.Contains(data, []byte("PRIVATE_INITIAL")) || bytes.Contains(data, []byte(`"ciphertext"`)) {
		t.Fatal("output is not complete native JSONL")
	}
	var report struct {
		Message struct{ Content []struct{ Text string } }
	}
	if json.Unmarshal(lines[1], &report) != nil || len(report.Message.Content) != 1 || report.Message.Content[0].Text != text {
		t.Fatal("full long report content was changed or truncated")
	}
	for _, line := range lines {
		var row claudeprompt.SDKNativeMessage
		if json.Unmarshal(line, &row) != nil || row.AgentID != "a12345678" || !row.IsSidechain {
			t.Fatal("wrong sidechain row")
		}
	}
	if got, err := s.ReadTail(8 << 20); err != nil || got != string(data) {
		t.Fatal("owned reader differs from real file", err)
	}
	if got, err := s.ReadTail(1024); err != nil || !strings.Contains(got, "KB of earlier output omitted]\n") || !strings.HasSuffix(got, string(data[len(data)-500:])) {
		t.Fatal("byte-bounded output tail differs", err)
	}
	leaf := s.Leaf()
	if s.Close() != nil || tracker.Close() != nil {
		t.Fatal("close failed")
	}
	fresh := claudeprompt.NewTracker(nil, claudeprompt.SDKNativeContentOptions{TranscriptStore: NewClaudeDesktopTranscriptStore(store.root, "owned-egress")})
	t.Cleanup(func() { _ = fresh.Close() })
	resumed, err := fresh.OpenSidechain("account", "session", "a12345678", leaf)
	if err != nil || resumed.Path() != path {
		t.Fatal("restart lost real output binding", err)
	}
	if err := resumed.AppendInput(json.RawMessage(`"PRIVATE_RESUME"`), uuid.NewString(), time.Now()); err != nil || resumed.Flush() != nil {
		t.Fatal("resumed append failed", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, data) || !bytes.Contains(after, []byte("PRIVATE_RESUME")) {
		t.Fatal("resume rewrote original projection")
	}
	journalFiles, _ := filepath.Glob(filepath.Join(store.root, "sdk-transcripts", "*.enc"))
	if len(journalFiles) != 1 {
		t.Fatal("journal missing")
	}
	journal, _ := os.ReadFile(journalFiles[0])
	if bytes.Contains(journal, []byte("PRIVATE")) {
		t.Fatal("authoritative journal lost encryption")
	}
}

func TestClaudeDesktopTaskOutputTamperPreservesOriginals(t *testing.T) {
	for _, mode := range []string{"extra", "truncated", "missing", "linked", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			store := NewClaudeDesktopTranscriptStore(t.TempDir(), "egress")
			scope := strings.Repeat("b", 64)
			first := syntheticTranscriptLine(uuid.NewString(), "PRIVATE_ORIGINAL")
			revision, err := store.AppendTranscript(scope, "", first)
			if err != nil {
				t.Fatal(err)
			}
			path, err := store.PrepareTaskOutput(scope)
			if err != nil {
				t.Fatal(err)
			}
			journal, _, _ := store.path(scope)
			before, _ := os.ReadFile(journal)
			switch mode {
			case "extra":
				if err := os.WriteFile(path, append(bytes.Clone(first), []byte("corrupt\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.WriteFile(path, first[:5], 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "linked":
				if err := os.Link(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Skip("host does not grant test symlink creation")
				}
			}
			original, _ := os.ReadFile(path)
			if value, err := store.ReadTaskOutput(scope, 1024); err == nil || value != "" {
				t.Fatal("unverified plaintext returned")
			}
			if _, err := store.AppendTranscript(scope, revision, syntheticTranscriptLine(uuid.NewString(), "new")); err == nil {
				t.Fatal("damaged projection silently repaired/appended")
			}
			after, _ := os.ReadFile(journal)
			projection, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || !bytes.Equal(original, projection) {
				t.Fatal("rejected operation modified original evidence")
			}
		})
	}
}
