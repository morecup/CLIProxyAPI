package helps

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

// ClaudeDesktopTranscriptStore retains append groups in independent protected
// frames. Decrypted payloads are JSONL; the at-rest envelope is intentionally
// not a plaintext Desktop file. Existing bytes are never rewritten on append.
// Native relocation, removal/GC and remote storage are separate operations.
type ClaudeDesktopTranscriptStore struct {
	root, egress string
	outputs      sync.Map
}

type desktopTranscriptFrame struct {
	Version  int    `json:"version"`
	Binding  string `json:"binding"`
	Previous string `json:"previous"`
	Revision string `json:"revision"`
	Lines    []byte `json:"lines"`
}

func NewClaudeDesktopTranscriptStore(root, egress string) *ClaudeDesktopTranscriptStore {
	return &ClaudeDesktopTranscriptStore{root: strings.TrimSpace(root), egress: egress}
}

func (s *ClaudeDesktopTranscriptStore) path(scope string) (string, string, error) {
	raw, err := hex.DecodeString(scope)
	if s == nil || s.root == "" || err != nil || len(raw) != sha256.Size {
		return "", "", claudeprompt.ErrSDKSessionUnavailable
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", "", claudeprompt.ErrSDKSessionUnavailable
	}
	identity, _ := json.Marshal([]string{"desktop-sdk-transcript-v1", s.egress, scope})
	sum := sha256.Sum256(identity)
	binding := hex.EncodeToString(sum[:])
	return filepath.Join(root, "sdk-transcripts", binding+".jsonl.enc"), binding, nil
}

func (s *ClaudeDesktopTranscriptStore) LoadTranscript(scope string) (claudeprompt.SDKTranscriptIndex, error) {
	path, binding, err := s.path(scope)
	if err != nil {
		return claudeprompt.SDKTranscriptIndex{}, err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	defer lock.Unlock()
	index, err := readDesktopTranscript(path, binding, nil)
	index.Path = path
	return index, err
}

// ReadTranscript is the content-bearing restoration boundary. A visitor's
// prefix is not a usable history unless every remaining protected frame and
// the final file close succeed. No plaintext is retained by this store.
func (s *ClaudeDesktopTranscriptStore) ReadTranscript(scope string, visit func([]byte) error) (claudeprompt.SDKTranscriptIndex, error) {
	path, binding, err := s.path(scope)
	if err != nil {
		return claudeprompt.SDKTranscriptIndex{}, err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	defer lock.Unlock()
	index, err := readDesktopTranscriptContent(path, binding, visit)
	if err != nil {
		return claudeprompt.SDKTranscriptIndex{}, err
	}
	return index, nil
}

func (s *ClaudeDesktopTranscriptStore) AppendTranscript(scope, revision string, lines []byte) (string, error) {
	if len(lines) == 0 || len(lines) > claudeprompt.MaxSDKSessionStateBytes {
		return "", claudeprompt.ErrSDKSessionInvalid
	}
	path, binding, err := s.path(scope)
	if err != nil {
		return "", err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	defer lock.Unlock()
	index, err := readDesktopTranscript(path, binding, nil)
	if err != nil {
		return "", err
	}
	if index.Revision != revision {
		return "", claudeprompt.ErrSDKSessionStale
	}
	projection, err := s.openTaskProjectionLocked(scope, path, binding)
	if err != nil {
		return "", err
	}
	if projection != nil {
		defer func() {
			if projection != nil {
				_ = projection.Close()
			}
		}()
	}
	seen := make(map[string]bool, len(index.UUIDs))
	for _, id := range index.UUIDs {
		seen[id] = true
	}
	if _, err := desktopTranscriptUUIDs(lines, seen); err != nil {
		return "", err
	}
	next := uuid.NewString()
	plaintext, err := json.Marshal(desktopTranscriptFrame{Version: 1, Binding: binding, Previous: revision, Revision: next, Lines: lines})
	if err != nil || os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	protector, ciphertext, err := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if err != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	encoded, err := json.Marshal(desktopContextEnvelope{Version: 1, Protector: protector, Ciphertext: ciphertext})
	if err != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	n, errWrite := file.Write(encoded)
	errSync := file.Sync()
	errClose := file.Close()
	if errWrite != nil || n != len(encoded) || errSync != nil || errClose != nil {
		// A partial frame remains diagnostic evidence. Further reads/appends
		// reject it; no truncation or invented successful mirror is attempted.
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	if projection != nil {
		n, errWrite := projection.Write(lines)
		errSync := projection.Sync()
		if n != len(lines) || errWrite != nil || errSync != nil {
			return "", claudeprompt.ErrSDKSessionUnavailable
		}
		errClose := projection.Close()
		projection = nil
		if errClose != nil {
			return "", claudeprompt.ErrSDKSessionUnavailable
		}
	}
	return next, nil
}

func desktopTranscriptUUIDs(lines []byte, seen map[string]bool) ([]string, error) {
	if len(lines) == 0 || lines[len(lines)-1] != '\n' {
		return nil, claudeprompt.ErrSDKSessionInvalid
	}
	var ids []string
	for _, line := range bytes.Split(lines[:len(lines)-1], []byte{'\n'}) {
		var row struct {
			UUID string `json:"uuid"`
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &row) != nil {
			return nil, claudeprompt.ErrSDKSessionInvalid
		}
		if row.Type == "bridge-session" {
			if _, err := claudeprompt.ParseSDKBridgeTranscriptRecord(line); err != nil {
				return nil, err
			}
			continue
		}
		if row.Type == "atis-latch" {
			var fields map[string]json.RawMessage
			if json.Unmarshal(line, &fields) != nil || len(fields) != 3 {
				return nil, claudeprompt.ErrSDKSessionInvalid
			}
			var pin *string
			var session, kind string
			if json.Unmarshal(fields["type"], &kind) != nil || kind != "atis-latch" ||
				json.Unmarshal(fields["atis"], &pin) != nil || pin == nil ||
				json.Unmarshal(fields["sessionId"], &session) != nil || session == "" {
				return nil, claudeprompt.ErrSDKSessionInvalid
			}
			continue
		}
		id, err := uuid.Parse(row.UUID)
		if err != nil || id == uuid.Nil || seen[row.UUID] || (row.Type != "user" && row.Type != "assistant" && row.Type != "system" && row.Type != "attachment") {
			return nil, claudeprompt.ErrSDKSessionInvalid
		}
		seen[row.UUID] = true
		ids = append(ids, row.UUID)
	}
	return ids, nil
}

// Legacy index and local verification callers share the same protected parser.
func readDesktopTranscript(path, binding string, visit func([]byte)) (claudeprompt.SDKTranscriptIndex, error) {
	var reader func([]byte) error
	if visit != nil {
		reader = func(lines []byte) error { visit(lines); return nil }
	}
	return readDesktopTranscriptContent(path, binding, reader)
}

func readDesktopTranscriptContent(path, binding string, visit func([]byte) error) (index claudeprompt.SDKTranscriptIndex, resultErr error) {
	index.Path = path
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return index, nil
	}
	if err != nil {
		return index, claudeprompt.ErrSDKSessionUnavailable
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			resultErr = claudeprompt.ErrSDKSessionUnavailable
		}
	}()
	reader := bufio.NewReader(file)
	seen := make(map[string]bool)
	for {
		var encoded []byte
		for {
			part, errRead := reader.ReadSlice('\n')
			if len(encoded)+len(part) > 3*claudeprompt.MaxSDKSessionStateBytes {
				return index, claudeprompt.ErrSDKSessionInvalid
			}
			encoded = append(encoded, part...)
			if errors.Is(errRead, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(errRead, io.EOF) && len(encoded) == 0 {
				return index, nil
			}
			if errRead != nil {
				return index, claudeprompt.ErrSDKSessionInvalid
			}
			break
		}
		var envelope desktopContextEnvelope
		if json.Unmarshal(encoded, &envelope) != nil || envelope.Version != 1 || len(envelope.Ciphertext) == 0 {
			return index, claudeprompt.ErrSDKSessionInvalid
		}
		plaintext, err := claudedesktop.UnprotectRuntimePayload(path, envelope.Protector, envelope.Ciphertext)
		if err != nil || len(plaintext) > 2*claudeprompt.MaxSDKSessionStateBytes {
			return index, claudeprompt.ErrSDKSessionInvalid
		}
		var frame desktopTranscriptFrame
		if json.Unmarshal(plaintext, &frame) != nil || frame.Version != 1 || frame.Binding != binding || frame.Previous != index.Revision || len(frame.Lines) > claudeprompt.MaxSDKSessionStateBytes {
			return index, claudeprompt.ErrSDKSessionInvalid
		}
		id, err := uuid.Parse(frame.Revision)
		if err != nil || id == uuid.Nil || frame.Revision == frame.Previous {
			return index, claudeprompt.ErrSDKSessionInvalid
		}
		ids, err := desktopTranscriptUUIDs(frame.Lines, seen)
		if err != nil {
			return index, err
		}
		index.UUIDs = append(index.UUIDs, ids...)
		index.Revision = frame.Revision
		if visit != nil {
			if err := visit(frame.Lines); err != nil {
				return index, claudeprompt.ErrSDKSessionInvalid
			}
		}
	}
}
