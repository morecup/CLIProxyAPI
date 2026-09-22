package helps

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type claudeSSEChunkReader struct {
	chunks     []string
	beforeRead func()
	terminal   error
}

func (r *claudeSSEChunkReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
	}
	if len(r.chunks) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestReadClaudeSSEObservesBeforeNextReadAndPreservesWire(t *testing.T) {
	var lines []string
	reader := &claudeSSEChunkReader{chunks: []string{"data: first\r\n", "\r\ndata: second\n", "data: tail"}}
	reader.beforeRead = func() {
		if len(reader.chunks) == 2 && len(lines) != 1 {
			t.Fatal("first line was not observed before the next read")
		}
	}
	body, err := ReadClaudeSSEWithObserver(reader, func(line []byte) error { lines = append(lines, string(line)); return nil })
	if err != nil || string(body) != "data: first\r\n\r\ndata: second\ndata: tail" || strings.Join(lines, "|") != "data: first||data: second|data: tail" {
		t.Fatalf("body=%q lines=%q err=%v", body, lines, err)
	}
}

func TestReadClaudeSSEPreservesLateReadError(t *testing.T) {
	want := errors.New("synthetic read failure")
	reader := &claudeSSEChunkReader{chunks: []string{"data: complete\n", "data: incomplete"}, terminal: want}
	var lines []string
	body, err := ReadClaudeSSEWithObserver(reader, func(line []byte) error { lines = append(lines, string(line)); return nil })
	if !errors.Is(err, want) || string(body) != "data: complete\ndata: incomplete" || len(lines) != 1 {
		t.Fatalf("body=%q lines=%q err=%v", body, lines, err)
	}
}

func TestReadClaudeSSEPropagatesObserverError(t *testing.T) {
	want := errors.New("synthetic observer failure")
	_, err := ReadClaudeSSEWithObserver(strings.NewReader("data: first\n"), func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestReadClaudeSSENilObserverPreservesPayload(t *testing.T) {
	body, err := ReadClaudeSSEWithObserver(strings.NewReader("unaltered\r\n"), nil)
	if err != nil || string(body) != "unaltered\r\n" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}
