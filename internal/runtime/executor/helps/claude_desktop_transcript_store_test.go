package helps

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func syntheticTranscriptLine(id, text string) []byte {
	return []byte(`{"type":"user","uuid":"` + id + `","message":{"role":"user","content":"` + text + `"}}` + "\n")
}

func TestClaudeDesktopTranscriptProtectedAppendRoundTrip(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "synthetic-egress")
	scope := strings.Repeat("a", 64)
	firstID, secondID := uuid.NewString(), uuid.NewString()
	first := syntheticTranscriptLine(firstID, "PRIVATE_FIRST")
	second := syntheticTranscriptLine(secondID, "PRIVATE_SECOND")
	revision, err := store.AppendTranscript(scope, "", first)
	if err != nil {
		t.Fatal(err)
	}
	path, binding, _ := store.path(scope)
	prefix, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.AppendTranscript(scope, revision, second)
	if err != nil {
		t.Fatal(err)
	}
	all, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(all, prefix) || bytes.Equal(all, prefix) || bytes.Contains(all, []byte("PRIVATE_")) || bytes.Contains(all, []byte(firstID)) {
		t.Fatal("append rewrote prior bytes or exposed private content")
	}
	var lines []byte
	index, err := readDesktopTranscript(path, binding, func(value []byte) { lines = append(lines, value...) })
	if err != nil || index.Revision != next || !reflect.DeepEqual(index.UUIDs, []string{firstID, secondID}) || !bytes.Equal(lines, append(first, second...)) {
		t.Fatal("protected JSONL could not be restored exactly", err)
	}
	restored, err := NewClaudeDesktopTranscriptStore(store.root, "synthetic-egress").LoadTranscript(scope)
	if err != nil || !reflect.DeepEqual(index, restored) {
		t.Fatal("reconstruction lost durable index", err)
	}
	if _, err := store.AppendTranscript(scope, next, first); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
		t.Fatal("duplicate durable UUID appended", err)
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, all) {
		t.Fatal("rejected append modified existing log")
	}
}

func TestClaudeDesktopTranscriptContentReaderUsesProtectedBoundary(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "synthetic-egress")
	scope := strings.Repeat("f", 64)
	first := syntheticTranscriptLine(uuid.NewString(), "PRIVATE_FIRST")
	second := syntheticTranscriptLine(uuid.NewString(), "PRIVATE_SECOND")
	revision, err := store.AppendTranscript(scope, "", first)
	if err != nil {
		t.Fatal(err)
	}
	revision, err = store.AppendTranscript(scope, revision, second)
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	index, err := store.ReadTranscript(scope, func(lines []byte) error { content = append(content, lines...); return nil })
	if err != nil || index.Revision != revision || !bytes.Equal(content, append(first, second...)) {
		t.Fatal("public protected reader did not restore exact append groups", err)
	}
	privateFailure := errors.New("PRIVATE_VISITOR_FAILURE")
	index, err = store.ReadTranscript(scope, func([]byte) error { return privateFailure })
	if !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) || strings.Contains(err.Error(), "PRIVATE") || index.Path != "" || len(index.UUIDs) != 0 || index.Revision != "" {
		t.Fatal("failed provisional reader exposed caller error or partial index")
	}
	if _, err := store.AppendTranscript(scope, revision, syntheticTranscriptLine(uuid.NewString(), "after-reader-failure")); err != nil {
		t.Fatal("reader failure retained the append lock", err)
	}
	path, _, _ := store.path(scope)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, errWrite := file.WriteString(`{"torn":`)
	errClose := file.Close()
	if errWrite != nil || errClose != nil {
		t.Fatal(errWrite, errClose)
	}
	visits := 0
	index, err = store.ReadTranscript(scope, func([]byte) error { visits++; return nil })
	if !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) || visits != 3 || len(index.UUIDs) != 0 || index.Revision != "" {
		t.Fatal("a verified prefix authorized the corrupted tail", err)
	}
}

func TestClaudeDesktopTranscriptATISMetadataHasNoUUIDAndPreservesBytes(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "synthetic-egress")
	scope, session := strings.Repeat("e", 64), uuid.NewString()
	userID := uuid.NewString()
	first := syntheticTranscriptLine(userID, "synthetic input")
	revision, err := store.AppendTranscript(scope, "", first)
	if err != nil {
		t.Fatal(err)
	}
	path, binding, _ := store.path(scope)
	prefix, _ := os.ReadFile(path)
	metadata := []byte(`{"type":"atis-latch","atis":"","sessionId":"` + session + `"}` + "\n")
	for range 2 {
		revision, err = store.AppendTranscript(scope, revision, metadata)
		if err != nil {
			t.Fatal("metadata was rejected or UUID-deduped", err)
		}
	}
	encoded, _ := os.ReadFile(path)
	if !bytes.HasPrefix(encoded, prefix) || bytes.Contains(encoded, []byte(session)) || bytes.Contains(encoded, []byte("atis-latch")) {
		t.Fatal("metadata rewrote historical frames or escaped protected storage")
	}
	var restored []byte
	index, err := readDesktopTranscript(path, binding, func(lines []byte) { restored = append(restored, lines...) })
	if err != nil || index.Path != path || !reflect.DeepEqual(index.UUIDs, []string{userID}) || !bytes.Equal(restored, append(append(first, metadata...), metadata...)) {
		t.Fatal("metadata contaminated the UUID index or exact transcript bytes", err)
	}
	for _, invalid := range []string{
		`{"type":"atis-latch","atis":null,"sessionId":"session"}`,
		`{"type":"atis-latch","atis":42,"sessionId":"session"}`,
		`{"type":"atis-latch","sessionId":"session"}`,
		`{"type":"atis-latch","atis":"","sessionId":""}`,
		`{"type":"atis-latch","atis":"","sessionId":"session","uuid":"foreign"}`,
		`{"Type":"atis-latch","atis":"","sessionId":"session"}`,
	} {
		if _, err := store.AppendTranscript(scope, revision, []byte(invalid+"\n")); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
			t.Fatal("invalid metadata passed the protected transcript gate", err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, encoded) {
		t.Fatal("invalid metadata modified the prior log")
	}
}

func TestClaudeDesktopTranscriptBindingAndInvalidScope(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("a", 64)
	store := NewClaudeDesktopTranscriptStore(root, "egress-one")
	if _, err := store.AppendTranscript(scope, "", syntheticTranscriptLine(uuid.NewString(), "PRIVATE")); err != nil {
		t.Fatal(err)
	}
	path, _, _ := store.path(scope)
	original, _ := os.ReadFile(path)
	for _, tc := range []struct{ scope, egress string }{{strings.Repeat("b", 64), "egress-one"}, {scope, "egress-two"}} {
		other := NewClaudeDesktopTranscriptStore(root, tc.egress)
		index, err := other.LoadTranscript(tc.scope)
		if err != nil || len(index.UUIDs) != 0 {
			t.Fatal("transcript crossed scope", err)
		}
		otherPath, _, _ := other.path(tc.scope)
		if err := os.WriteFile(otherPath, original, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := other.LoadTranscript(tc.scope); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
			t.Fatal("copied frame bypassed binding", err)
		}
	}
	for _, value := range []string{"", "../escape", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := store.LoadTranscript(value); err == nil {
			t.Fatal("unsafe scope accepted")
		}
	}
}

func TestClaudeDesktopTranscriptCASPreservesConcurrentWriter(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "")
	scope := strings.Repeat("c", 64)
	revision, err := store.AppendTranscript(scope, "", syntheticTranscriptLine(uuid.NewString(), "first"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, stale := 0, 0
	for range 6 {
		wg.Go(func() {
			_, err := NewClaudeDesktopTranscriptStore(store.root, "").AppendTranscript(scope, revision, syntheticTranscriptLine(uuid.NewString(), "next"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else if errors.Is(err, claudeprompt.ErrSDKSessionStale) {
				stale++
			} else {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if succeeded != 1 || stale != 5 {
		t.Fatal("stale writer appended", succeeded, stale)
	}
	index, err := store.LoadTranscript(scope)
	if err != nil || len(index.UUIDs) != 2 {
		t.Fatal("concurrent append damaged transcript", err)
	}
}

func TestClaudeDesktopTranscriptCorruptTornOrMissingOriginal(t *testing.T) {
	for _, kind := range []string{"corrupt", "torn", "missing"} {
		t.Run(kind, func(t *testing.T) {
			store := NewClaudeDesktopTranscriptStore(t.TempDir(), "")
			scope := strings.Repeat("d", 64)
			revision, err := store.AppendTranscript(scope, "", syntheticTranscriptLine(uuid.NewString(), "first"))
			if err != nil {
				t.Fatal(err)
			}
			path, _, _ := store.path(scope)
			original, _ := os.ReadFile(path)
			var damaged []byte
			switch kind {
			case "corrupt":
				damaged = []byte("synthetic-corruption\n")
			case "torn":
				damaged = append(original, []byte(`{"version":1`)...)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if damaged != nil {
				if err := os.WriteFile(path, damaged, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.AppendTranscript(scope, revision, syntheticTranscriptLine(uuid.NewString(), "next"))
			if kind == "missing" {
				if !errors.Is(err, claudeprompt.ErrSDKSessionStale) {
					t.Fatal("stale writer recreated missing log", err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing log recreated")
				}
			} else {
				if !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
					t.Fatal("damaged frame accepted", err)
				}
				actual, _ := os.ReadFile(path)
				if !bytes.Equal(actual, damaged) {
					t.Fatal("corrupt original was rewritten")
				}
			}
		})
	}
}

func TestClaudeDesktopTranscriptStorageFailureAndMalformedLines(t *testing.T) {
	root := t.TempDir()
	store := NewClaudeDesktopTranscriptStore(root, "")
	scope := strings.Repeat("e", 64)
	for _, line := range [][]byte{nil, []byte("{}\n"), []byte("{not-json}\n"), []byte(`{"type":"user","uuid":"not-a-uuid"}` + "\n"), []byte(`{"type":"user"}`)} {
		if _, err := store.AppendTranscript(scope, "", line); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
			t.Fatal("malformed line accepted", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "sdk-transcripts"), []byte("synthetic directory blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendTranscript(scope, "", syntheticTranscriptLine(uuid.NewString(), "secret")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("write failure hidden or leaked content", err)
	}
}
