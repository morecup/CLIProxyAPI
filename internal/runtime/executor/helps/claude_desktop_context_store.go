package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/sjson"
)

const maxDesktopContextBytes = 32 << 20

var (
	ErrClaudeDesktopContextUnavailable = errors.New("claude desktop owned context is unavailable")
	ErrClaudeDesktopContextInvalid     = errors.New("claude desktop owned context cannot be verified")
	ErrClaudeDesktopContextStale       = errors.New("claude desktop owned context changed during recovery")
	desktopContextLocks                [64]sync.Mutex
)

// ClaudeDesktopContextStore retains only adopted compaction transformations,
// not an inferred transcript, file system or hook configuration. Account-local
// runtime directories add the enrollment/machine revision boundary. Contents
// are protected at rest using the credential store's current-user policy. An
// account runtime keeps this store outside revision-specific process state.
type ClaudeDesktopContextStore struct {
	root   string
	egress string
}

type desktopContextTransition struct {
	Source      []string          `json:"source"`
	Replacement []json.RawMessage `json:"replacement"`
}

type desktopContextRecord struct {
	Version     int                        `json:"version"`
	Scope       string                     `json:"scope"`
	Revision    string                     `json:"revision"`
	Transitions []desktopContextTransition `json:"transitions"`
}

type desktopContextEnvelope struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext []byte `json:"ciphertext"`
}

// ClaudeDesktopContextLease binds the exact pre-render history to one loaded
// revision. It is request-local and intentionally has no exported data fields.
// A stale or unreadable record is never overwritten with an empty history.
type ClaudeDesktopContextLease struct {
	path     string
	scope    string
	revision string
	input    []string
}

func NewClaudeDesktopContextStore(root, globalEgress string) *ClaudeDesktopContextStore {
	return &ClaudeDesktopContextStore{root: strings.TrimSpace(root), egress: globalEgress}
}

func (s *ClaudeDesktopContextStore) contextPath(auth *cliproxyauth.Auth, profile, session string) (string, string, error) {
	if s == nil || s.root == "" || auth == nil || auth.ID == "" || profile == "" || session == "" {
		return "", "", ErrClaudeDesktopContextUnavailable
	}
	egress := auth.ProxyURL
	if egress == "" {
		egress = s.egress
	}
	identity, _ := json.Marshal([]string{"desktop-owned-context-v1", auth.ID, profile, egress, session})
	sum := sha256.Sum256(identity)
	scope := hex.EncodeToString(sum[:])
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", "", ErrClaudeDesktopContextUnavailable
	}
	// No caller-provided path segment is used, including session IDs.
	return filepath.Join(root, "owned-context", scope+".json"), scope, nil
}

func desktopContextLock(path string) *sync.Mutex {
	// Fixed stripes bound lock memory and synchronize separate stores in the
	// same process. The account runtime remains the sole cross-process owner.
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	return &desktopContextLocks[int(sum[0])%len(desktopContextLocks)]
}

// Resume replaces only a complete, exact previously adopted prefix. A caller
// may send the original history, an intermediate compaction, or the latest
// history; transitions are applied once in chronological order. All new rows
// (including tool/image content) and all non-message request fields survive.
func (s *ClaudeDesktopContextStore) Resume(ctx context.Context, auth *cliproxyauth.Auth, profile, session string, body []byte) (*ClaudeDesktopContextLease, []byte, error) {
	if ctx == nil {
		return nil, body, ErrClaudeDesktopContextUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, body, err
	}
	path, scope, err := s.contextPath(auth, profile, session)
	if err != nil {
		return nil, body, err
	}
	rows, err := desktopContextRows(body)
	if err != nil {
		return nil, body, err
	}
	lock := desktopContextLock(path)
	lock.Lock()
	record, err := readDesktopContext(path, scope)
	lock.Unlock()
	if err != nil {
		return nil, body, err
	}
	changed := false
	for _, transition := range record.Transitions {
		if len(rows) < len(transition.Source) {
			continue
		}
		keys, errKeys := desktopContextKeys(rows[:len(transition.Source)])
		if errKeys != nil || !equalDesktopContextKeys(keys, transition.Source) {
			continue
		}
		rows = append(append([]json.RawMessage(nil), transition.Replacement...), rows[len(transition.Source):]...)
		changed = true
	}
	keys, err := desktopContextKeys(rows)
	if err != nil {
		return nil, body, err
	}
	lease := &ClaudeDesktopContextLease{path: path, scope: scope, revision: record.Revision, input: keys}
	if !changed {
		return lease, body, nil
	}
	encoded, err := json.Marshal(rows)
	if err != nil || len(encoded) > maxDesktopContextBytes {
		return nil, body, ErrClaudeDesktopContextInvalid
	}
	updated, err := sjson.SetRawBytes(body, "messages", encoded)
	if err != nil {
		return nil, body, ErrClaudeDesktopContextInvalid
	}
	return lease, updated, nil
}

// SaveAdopted is called only after the application has committed and before
// sending its next main request. A storage failure is reported separately from
// API success; it must not turn an adopted history into a rejected API call or
// claim durable restoration. No uncommitted helper output enters this store.
func (l *ClaudeDesktopContextLease) SaveAdopted(ctx context.Context, source, continued []byte) error {
	if l == nil || ctx == nil {
		return ErrClaudeDesktopContextUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceRows, err := desktopContextRows(source)
	if err != nil {
		return err
	}
	sourceKeys, err := desktopContextKeys(sourceRows)
	if err != nil || !equalDesktopContextKeys(sourceKeys, l.input) {
		return ErrClaudeDesktopContextStale
	}
	replacement, err := desktopContextRows(continued)
	if err != nil {
		return err
	}
	replacementKeys, err := desktopContextKeys(replacement)
	if err != nil || equalDesktopContextKeys(sourceKeys, replacementKeys) {
		return ErrClaudeDesktopContextInvalid
	}
	lock := desktopContextLock(l.path)
	lock.Lock()
	defer lock.Unlock()
	record, err := readDesktopContext(l.path, l.scope)
	if err != nil {
		return err
	}
	if record.Revision != l.revision {
		return ErrClaudeDesktopContextStale
	}
	if len(record.Transitions) >= 256 {
		return ErrClaudeDesktopContextUnavailable
	}
	record.Transitions = append(record.Transitions, desktopContextTransition{Source: sourceKeys, Replacement: replacement})
	record.Revision = uuid.NewString()
	plaintext, err := json.Marshal(record)
	if err != nil || len(plaintext) > maxDesktopContextBytes {
		return ErrClaudeDesktopContextInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.MkdirAll(filepath.Dir(l.path), 0700) != nil {
		return ErrClaudeDesktopContextUnavailable
	}
	protector, ciphertext, err := claudedesktop.ProtectRuntimePayload(l.path, plaintext)
	if err != nil {
		return ErrClaudeDesktopContextUnavailable
	}
	encoded, err := json.Marshal(desktopContextEnvelope{Version: 1, Protector: protector, Ciphertext: ciphertext})
	if err != nil {
		return ErrClaudeDesktopContextUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if AtomicWriteFile(l.path, encoded, 0600) != nil {
		return ErrClaudeDesktopContextUnavailable
	}
	return nil
}

func readDesktopContext(path, scope string) (desktopContextRecord, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return desktopContextRecord{Version: 1, Scope: scope}, nil
	}
	if err != nil {
		return desktopContextRecord{}, ErrClaudeDesktopContextUnavailable
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 2*maxDesktopContextBytes+1))
	errClose := file.Close()
	if err != nil || errClose != nil || len(encoded) > 2*maxDesktopContextBytes {
		return desktopContextRecord{}, ErrClaudeDesktopContextUnavailable
	}
	var envelope desktopContextEnvelope
	if json.Unmarshal(encoded, &envelope) != nil || envelope.Version != 1 || len(envelope.Ciphertext) == 0 {
		return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
	}
	plaintext, err := claudedesktop.UnprotectRuntimePayload(path, envelope.Protector, envelope.Ciphertext)
	if err != nil || len(plaintext) > maxDesktopContextBytes {
		return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
	}
	var record desktopContextRecord
	if json.Unmarshal(plaintext, &record) != nil || record.Version != 1 || record.Scope != scope || len(record.Transitions) == 0 || len(record.Transitions) > 256 {
		return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
	}
	if revision, errRevision := uuid.Parse(record.Revision); errRevision != nil || revision == uuid.Nil {
		return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
	}
	for _, transition := range record.Transitions {
		if len(transition.Source) == 0 || len(transition.Source) > 16384 || len(transition.Replacement) == 0 {
			return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
		}
		for _, key := range transition.Source {
			if value, errKey := hex.DecodeString(key); errKey != nil || len(value) != sha256.Size {
				return desktopContextRecord{}, ErrClaudeDesktopContextInvalid
			}
		}
		if _, errKeys := desktopContextKeys(transition.Replacement); errKeys != nil {
			return desktopContextRecord{}, errKeys
		}
	}
	return record, nil
}

func desktopContextRows(body []byte) ([]json.RawMessage, error) {
	if len(body) > maxDesktopContextBytes {
		return nil, ErrClaudeDesktopContextInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrClaudeDesktopContextInvalid
	}
	root := make(map[string]json.RawMessage)
	for decoder.More() {
		key, errKey := decoder.Token()
		name, ok := key.(string)
		if errKey != nil || !ok {
			return nil, ErrClaudeDesktopContextInvalid
		}
		if _, duplicate := root[name]; duplicate {
			return nil, ErrClaudeDesktopContextInvalid
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return nil, ErrClaudeDesktopContextInvalid
		}
		root[name] = raw
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrClaudeDesktopContextInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrClaudeDesktopContextInvalid
	}
	var rows []json.RawMessage
	if json.Unmarshal(root["messages"], &rows) != nil || len(rows) == 0 || len(rows) > 16384 {
		return nil, ErrClaudeDesktopContextInvalid
	}
	return rows, nil
}

func desktopContextKeys(rows []json.RawMessage) ([]string, error) {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if !desktopContextUnicodeValid(row) {
			return nil, ErrClaudeDesktopContextInvalid
		}
		decoder := json.NewDecoder(bytes.NewReader(row))
		decoder.UseNumber()
		value, err := desktopContextJSON(decoder, 0)
		message, ok := value.(map[string]any)
		if err != nil || !ok || (message["role"] != "user" && message["role"] != "assistant") || message["content"] == nil {
			return nil, ErrClaudeDesktopContextInvalid
		}
		// Native text normalization permits string/block-array equivalents.
		// Only known text fields and cache breakpoints may disappear here.
		// Unknown extensions and every non-text value remain part of the key.
		if blocks, ok := message["content"].([]any); ok && len(blocks) > 0 {
			var text strings.Builder
			known := true
			for _, raw := range blocks {
				block, ok := raw.(map[string]any)
				textValue, textOK := block["text"].(string)
				if !ok || !textOK || block["type"] != "text" {
					known = false
					break
				}
				for key := range block {
					if key != "type" && key != "text" && key != "cache_control" {
						known = false
					}
				}
				text.WriteString(textValue)
			}
			if known {
				message["content"] = text.String()
			}
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return nil, ErrClaudeDesktopContextInvalid
		}
		sum := sha256.Sum256(encoded)
		keys = append(keys, hex.EncodeToString(sum[:]))
	}
	return keys, nil
}

// Go's JSON decoder replaces invalid UTF-8 and unpaired UTF-16 surrogates.
// Those replacements are lossy and must never establish history equality.
func desktopContextUnicodeValid(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	quoted := false
	for index := 0; index < len(raw); index++ {
		if raw[index] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw) {
			return false
		}
		if raw[index] != 'u' {
			continue
		}
		if index+4 >= len(raw) {
			return false
		}
		first, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
		if err != nil || (first >= 0xdc00 && first <= 0xdfff) {
			return false
		}
		index += 4
		if first < 0xd800 || first > 0xdbff {
			continue
		}
		if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
			return false
		}
		second, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
		if err != nil || second < 0xdc00 || second > 0xdfff {
			return false
		}
		index += 6
	}
	return !quoted
}

// Reject duplicate keys instead of guessing which interpretation an upstream
// JSON parser will use. json.Number preserves integers beyond float64 range.
func desktopContextJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, ErrClaudeDesktopContextInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, ErrClaudeDesktopContextInvalid
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for decoder.More() {
			key, errKey := decoder.Token()
			name, ok := key.(string)
			if errKey != nil || !ok {
				return nil, ErrClaudeDesktopContextInvalid
			}
			if _, duplicate := object[name]; duplicate {
				return nil, ErrClaudeDesktopContextInvalid
			}
			value, errValue := desktopContextJSON(decoder, depth+1)
			if errValue != nil {
				return nil, errValue
			}
			object[name] = value
		}
		end, errEnd := decoder.Token()
		if errEnd != nil || end != json.Delim('}') {
			return nil, ErrClaudeDesktopContextInvalid
		}
		return object, nil
	case json.Delim('['):
		array := make([]any, 0)
		for decoder.More() {
			value, errValue := desktopContextJSON(decoder, depth+1)
			if errValue != nil {
				return nil, errValue
			}
			array = append(array, value)
		}
		end, errEnd := decoder.Token()
		if errEnd != nil || end != json.Delim(']') {
			return nil, ErrClaudeDesktopContextInvalid
		}
		return array, nil
	default:
		if _, delimiter := token.(json.Delim); delimiter {
			return nil, ErrClaudeDesktopContextInvalid
		}
		return token, nil
	}
}

func equalDesktopContextKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
