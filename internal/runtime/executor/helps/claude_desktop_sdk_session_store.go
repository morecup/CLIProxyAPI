package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

// ClaudeDesktopSDKSessionStore persists actual tracker ownership/observations.
// It is not the native Desktop transcript format or a source of file/hook facts.
// Account runtimes may use a revision-independent durable directory; scope and
// effective egress independently isolate records within that account boundary.
type ClaudeDesktopSDKSessionStore struct {
	root             string
	egress           string
	nativeContent    bool
	featureCache     bool
	sessionAliases   bool
	desktopRecords   bool
	remoteTranscript bool
	agentTasks       bool
}

type desktopSDKSessionRecord struct {
	Version  int             `json:"version"`
	Scope    string          `json:"scope"`
	Revision string          `json:"revision"`
	Payload  json.RawMessage `json:"payload"`
}

func NewClaudeDesktopSDKSessionStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress}
}

// NewClaudeDesktopFeatureStore reuses protected revision-checked persistence
// under a distinct namespace. Process-local exposure marks are never stored.
func NewClaudeDesktopFeatureStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, featureCache: true}
}

// NewClaudeDesktopSessionAliasStore protects caller-to-native session bindings
// separately from transcripts, structural history and evaluated feature data.
func NewClaudeDesktopSessionAliasStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, sessionAliases: true}
}

// NewClaudeDesktopSessionRecordStore keeps the Desktop record catalog separate
// from SDK aliases. Live queries and transcript content are not serialized here.
func NewClaudeDesktopSessionRecordStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, desktopRecords: true}
}

// NewClaudeDesktopNativeContentStore uses the same authenticated protection and
// revision checks but a separate directory and binding domain. Its payload is
// a live native-message checkpoint, never structural state or SDK disk JSONL.
func NewClaudeDesktopNativeContentStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, nativeContent: true}
}

// Remote hydration has its own protected namespace. A full server replacement
// must never overwrite the original local append journal.
func NewClaudeDesktopRemoteTranscriptStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, remoteTranscript: true}
}

// Executable task records have their own protected namespace. They are never
// restored from a caller body or confused with the foreground transcript.
func NewClaudeDesktopAgentTaskStore(root, globalEgress string) *ClaudeDesktopSDKSessionStore {
	return &ClaudeDesktopSDKSessionStore{root: strings.TrimSpace(root), egress: globalEgress, agentTasks: true}
}

func (s *ClaudeDesktopSDKSessionStore) path(scope string) (string, string, error) {
	raw, err := hex.DecodeString(scope)
	if s == nil || s.root == "" || err != nil || len(raw) != sha256.Size {
		return "", "", claudeprompt.ErrSDKSessionUnavailable
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", "", claudeprompt.ErrSDKSessionUnavailable
	}
	domain, directory := "desktop-sdk-state-v1", "sdk-sessions"
	if s.nativeContent {
		domain, directory = "desktop-sdk-native-content-v1", "sdk-native-content"
	}
	if s.featureCache {
		domain, directory = "desktop-sdk-features-v1", "sdk-features"
	}
	if s.sessionAliases {
		domain, directory = "desktop-sdk-session-alias-v1", "sdk-session-aliases"
	}
	if s.desktopRecords {
		domain, directory = "desktop-session-records-v1", "desktop-session-records"
	}
	if s.remoteTranscript {
		domain, directory = "desktop-sdk-remote-transcript-v1", "sdk-remote-transcripts"
	}
	if s.agentTasks {
		domain, directory = "desktop-sdk-agent-tasks-v1", "sdk-agent-tasks"
	}
	identity, _ := json.Marshal([]string{domain, s.egress, scope})
	sum := sha256.Sum256(identity)
	binding := hex.EncodeToString(sum[:])
	return filepath.Join(root, directory, binding+".json"), binding, nil
}

func (s *ClaudeDesktopSDKSessionStore) Load(scope string) ([]byte, string, error) {
	path, binding, err := s.path(scope)
	if err != nil {
		return nil, "", err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	defer lock.Unlock()
	record, err := readDesktopSDKSession(path, binding)
	if err != nil {
		return nil, "", err
	}
	return append([]byte(nil), record.Payload...), record.Revision, nil
}

func (s *ClaudeDesktopSDKSessionStore) Save(scope, previousRevision string, payload []byte) (string, error) {
	if len(payload) == 0 || len(payload) > claudeprompt.MaxSDKSessionStateBytes || !json.Valid(payload) {
		return "", claudeprompt.ErrSDKSessionInvalid
	}
	path, binding, err := s.path(scope)
	if err != nil {
		return "", err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	defer lock.Unlock()
	previous, err := readDesktopSDKSession(path, binding)
	if err != nil {
		return "", err
	}
	if previous.Revision != previousRevision {
		return "", claudeprompt.ErrSDKSessionStale
	}
	revision := uuid.NewString()
	plaintext, err := json.Marshal(desktopSDKSessionRecord{Version: 1, Scope: binding, Revision: revision, Payload: payload})
	if err != nil {
		return "", claudeprompt.ErrSDKSessionInvalid
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	protector, ciphertext, err := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if err != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	encoded, err := json.Marshal(desktopContextEnvelope{Version: 1, Protector: protector, Ciphertext: ciphertext})
	if err != nil || AtomicWriteFile(path, encoded, 0600) != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	return revision, nil
}

func readDesktopSDKSession(path, binding string) (desktopSDKSessionRecord, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return desktopSDKSessionRecord{}, nil
	}
	if err != nil {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionUnavailable
	}
	encoded, errRead := io.ReadAll(io.LimitReader(file, 2*claudeprompt.MaxSDKSessionStateBytes+1))
	errClose := file.Close()
	if errRead != nil || errClose != nil || len(encoded) > 2*claudeprompt.MaxSDKSessionStateBytes {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionUnavailable
	}
	var envelope desktopContextEnvelope
	if json.Unmarshal(encoded, &envelope) != nil || envelope.Version != 1 || len(envelope.Ciphertext) == 0 {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionInvalid
	}
	plaintext, err := claudedesktop.UnprotectRuntimePayload(path, envelope.Protector, envelope.Ciphertext)
	if err != nil || len(plaintext) > claudeprompt.MaxSDKSessionStateBytes+65536 {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionInvalid
	}
	var record desktopSDKSessionRecord
	if json.Unmarshal(plaintext, &record) != nil || record.Version != 1 || record.Scope != binding ||
		len(record.Payload) == 0 || len(record.Payload) > claudeprompt.MaxSDKSessionStateBytes || !json.Valid(record.Payload) {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionInvalid
	}
	id, err := uuid.Parse(record.Revision)
	if err != nil || id == uuid.Nil {
		return desktopSDKSessionRecord{}, claudeprompt.ErrSDKSessionInvalid
	}
	return record, nil
}
