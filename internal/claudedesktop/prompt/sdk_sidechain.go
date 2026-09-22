package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SDKTaskOutputStore projects authenticated JSONL into an owned, access-restricted
// output file. The encrypted journal itself is never advertised as readable JSONL.
type SDKTaskOutputStore interface {
	PrepareTaskOutput(scope string) (string, error)
	ReadTaskOutput(scope string, limit int64) (string, error)
}

// SDKSidechain is an independent content owner, not a main prompt relabeled as a
// subagent. It shares the account's actual deferred transcript drain chain, but
// never changes the main session's history, leaf, ATIS latch or accounting.
type SDKSidechain struct {
	owner                                 *Tracker
	mu                                    sync.Mutex
	writer                                *sdkTranscriptWriter
	output                                SDKTaskOutputStore
	scope, sessionID, agentID, path, leaf string
	options                               SDKNativeContentOptions
	rows                                  map[string]*SDKNativeMessage
	order                                 []string
	latest                                []string
	err                                   error
	closed                                bool
}

// sdkSidechainAgentID accepts the native agent identifier ("a" plus an optional
// name segment and 16 hex digits) and the base-36 identifiers earlier local
// builds persisted.
var sdkSidechainNativeAgentID = regexp.MustCompile(`^a(?:[\w-]{1,63}-)?[0-9a-f]{16}$`)

func sdkSidechainAgentID(agentID string) bool {
	if sdkSidechainNativeAgentID.MatchString(agentID) {
		return true
	}
	return len(agentID) == 9 && agentID[0] == 'a' &&
		strings.IndexFunc(agentID[1:], func(r rune) bool { return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')) }) < 0
}

func (t *Tracker) OpenSidechain(accountID, sessionID, agentID, expectedLeaf string) (*SDKSidechain, error) {
	if t == nil || accountID == "" || sessionID == "" || !sdkSidechainAgentID(agentID) {
		return nil, ErrSDKSessionInvalid
	}
	t.mu.Lock()
	if t.closed || t.nativeOptions.TranscriptStore == nil {
		t.mu.Unlock()
		return nil, ErrSDKSessionUnavailable
	}
	reader, readable := t.nativeOptions.TranscriptStore.(SDKTranscriptReader)
	output, projectable := t.nativeOptions.TranscriptStore.(SDKTaskOutputStore)
	if !readable || !projectable {
		t.mu.Unlock()
		return nil, ErrSDKSessionUnavailable
	}
	if t.transcript == nil {
		t.transcript = newSDKTranscriptWriter(t.nativeOptions.TranscriptStore)
	}
	s := &SDKSidechain{writer: t.transcript, output: output, scope: digest(accountID, sessionID, "sidechain", agentID),
		sessionID: sessionID, agentID: agentID, options: t.nativeOptions, rows: make(map[string]*SDKNativeMessage)}
	if t.sidechains == nil {
		t.sidechains = make(map[string]*SDKSidechain)
	}
	if t.sidechains[s.scope] != nil {
		t.mu.Unlock()
		return nil, ErrSDKSessionStale
	}
	s.owner = t
	t.sidechains[s.scope] = s
	t.mu.Unlock()
	opened := false
	defer func() {
		if !opened {
			t.mu.Lock()
			if t.sidechains[s.scope] == s {
				delete(t.sidechains, s.scope)
			}
			t.mu.Unlock()
		}
	}()
	state := s.writer.load(s.scope)
	if state.loadErr != nil {
		return nil, state.loadErr
	}
	_, err := reader.ReadTranscript(s.scope, func(lines []byte) error {
		for _, line := range bytes.Split(bytes.TrimSuffix(lines, []byte{'\n'}), []byte{'\n'}) {
			var row SDKNativeMessage
			if json.Unmarshal(line, &row) != nil || !validNativeMessage(row, sessionID) || !row.IsSidechain || row.AgentID != agentID {
				return ErrSDKSessionInvalid
			}
			if row.ParentUUID != nil && s.rows[*row.ParentUUID] == nil {
				return ErrSDKSessionInvalid
			}
			copy := cloneNativeMessage(row)
			s.rows[row.UUID], s.leaf = &copy, row.UUID
			s.order = append(s.order, row.UUID)
		}
		return nil
	})
	// A recovery checkpoint is not evidence of an earlier disk append. Preserve
	// incomplete originals instead of manufacturing the missing prefix or tail.
	if err != nil || s.leaf != expectedLeaf {
		return nil, ErrSDKSessionInvalid
	}
	s.path, err = output.PrepareTaskOutput(s.scope)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrSDKSessionUnavailable
	}
	opened = true
	return s, nil
}

func (s *SDKSidechain) appendLocked(row SDKNativeMessage) error {
	if s.closed || s.err != nil {
		return ErrSDKSessionUnavailable
	}
	if s.rows[row.UUID] != nil {
		return ErrSDKSessionInvalid
	}
	row.IsSidechain, row.AgentID, row.SessionID = true, s.agentID, s.sessionID
	row.Version, row.Entrypoint, row.Cwd = s.options.Version, s.options.Entrypoint, s.options.Cwd
	row.UserType = "external"
	if s.options.Metadata != nil {
		metadata := s.options.Metadata(s.sessionID)
		row.SessionKind, row.GitBranch, row.Slug = metadata.SessionKind, cloneNativeString(metadata.GitBranch), cloneNativeString(metadata.Slug)
	}
	parent := s.leaf
	if row.SourceToolAssistantUUID != "" {
		parent = row.SourceToolAssistantUUID
	}
	if parent != "" {
		row.ParentUUID = &parent
	}
	if !validNativeMessage(row, s.sessionID) {
		return ErrSDKSessionInvalid
	}
	copy := cloneNativeMessage(row)
	s.rows[row.UUID], s.leaf = &copy, row.UUID
	s.order = append(s.order, row.UUID)
	// Native sidechain appendEntry does not use main-transcript UUID dedup.
	// The query reducer above owns per-yield insertion; late serialization sees
	// usage/stop mutations on that same live object, never a reconstructed reply.
	s.writer.enqueueMessage(s.scope, row.UUID, func() ([]byte, error) {
		s.mu.Lock()
		value := cloneNativeMessage(*s.rows[row.UUID])
		s.mu.Unlock()
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(value) != nil {
			return nil, ErrSDKSessionInvalid
		}
		return buffer.Bytes(), nil
	}, true)
	return nil
}

// AppendInput is called at the actual owned admission/tool-result transition,
// not when a retry repeats an upstream request body.
func (s *SDKSidechain) AppendInput(content json.RawMessage, promptID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !json.Valid(content) {
		return ErrSDKSessionInvalid
	}
	parts := []json.RawMessage{content}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) == nil && len(blocks) > 0 {
		allResults := true
		for _, raw := range blocks {
			var block struct{ Type string }
			allResults = allResults && json.Unmarshal(raw, &block) == nil && block.Type == "tool_result"
		}
		if allResults {
			parts = nil
			for _, raw := range blocks {
				parts = append(parts, append(append(json.RawMessage{'['}, raw...), ']'))
			}
		}
	}
	for _, part := range parts {
		row := SDKNativeMessage{Type: "user", UUID: uuid.NewString(), Timestamp: nativeContentTimestamp(at), PromptID: promptID}
		var results []struct {
			Type      string
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(part, &results) == nil && len(results) == 1 && results[0].Type == "tool_result" {
			row.SourceToolAssistantUUID = s.toolParentLocked(results[0].ToolUseID)
			if row.SourceToolAssistantUUID == "" {
				s.err = ErrSDKSessionInvalid
				return s.err
			}
		}
		row.Message, _ = json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{"user", part})
		if err := s.appendLocked(row); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

// AppendMetaInput records a native SendMessage delivery: an isMeta user row
// whose UUID is the queued_command attachment's source_uuid and whose origin
// is the delivery provenance. The caller owns both values.
func (s *SDKSidechain) AppendMetaInput(content, origin json.RawMessage, rowUUID, promptID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !json.Valid(content) || !json.Valid(origin) {
		return ErrSDKSessionInvalid
	}
	message, err := sdkAttachmentJSON(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{"user", content})
	if err != nil {
		return ErrSDKSessionInvalid
	}
	row := SDKNativeMessage{Type: "user", UUID: rowUUID, Timestamp: nativeContentTimestamp(at), PromptID: promptID,
		Message: message, IsMeta: true, Origin: bytes.Clone(origin)}
	if err := s.appendLocked(row); err != nil {
		s.err = err
		return err
	}
	return nil
}

// AppendAttachment records a native attachment row (for example the
// queued_command entry that precedes a delivered SendMessage).
func (s *SDKSidechain) AppendAttachment(attachment json.RawMessage, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !json.Valid(attachment) {
		return ErrSDKSessionInvalid
	}
	row := SDKNativeMessage{Type: "attachment", UUID: uuid.NewString(), Timestamp: nativeContentTimestamp(at), Attachment: bytes.Clone(attachment)}
	if err := s.appendLocked(row); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *SDKSidechain) toolParentLocked(id string) string {
	for i := len(s.order) - 1; i >= 0; i-- {
		row := s.rows[s.order[i]]
		if row.Type != "assistant" {
			continue
		}
		var message struct{ Content []struct{ Type, ID string } }
		if json.Unmarshal(row.Message, &message) != nil {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ID == id {
				return row.UUID
			}
		}
	}
	return ""
}

func (s *SDKSidechain) Observe(rows []SDKNativeMessage, issue string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSDKSessionUnavailable
	}
	if issue != "" {
		s.err = ErrSDKSessionInvalid
	}
	s.latest = nil
	for _, row := range rows {
		if row.Type != "assistant" {
			s.err = ErrSDKSessionInvalid
			break
		}
		if old := s.rows[row.UUID]; old != nil {
			if old.Timestamp != row.Timestamp || old.RequestID != row.RequestID || !nativeResponseContentUnchanged(old.Message, row.Message) {
				s.err = ErrSDKSessionInvalid
				break
			}
			old.Message = bytes.Clone(row.Message)
		} else if err := s.appendLocked(row); err != nil {
			s.err = err
			break
		}
		s.latest = append(s.latest, row.UUID)
	}
	return s.err
}

// VerifyResponse binds the runner's completed response to the actual per-yield
// observation. A path containing only the prompt or an earlier retry is not
// proof that the full returned report was recorded.
func (s *SDKSidechain) VerifyResponse(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var response struct {
		ID, Role string
		Content  json.RawMessage
	}
	if s.closed || s.err != nil || json.Unmarshal(raw, &response) != nil || response.ID == "" || response.Role != "assistant" {
		s.err = ErrSDKSessionInvalid
		return s.err
	}
	blocks := make([]json.RawMessage, 0)
	for _, id := range s.latest {
		row := s.rows[id]
		var message struct {
			ID, Role string
			Content  json.RawMessage
		}
		if row == nil || json.Unmarshal(row.Message, &message) != nil || message.ID != response.ID || message.Role != "assistant" {
			s.err = ErrSDKSessionInvalid
			return s.err
		}
		var parts []json.RawMessage
		if json.Unmarshal(message.Content, &parts) != nil {
			s.err = ErrSDKSessionInvalid
			return s.err
		}
		blocks = append(blocks, parts...)
	}
	content, _ := json.Marshal(blocks)
	a, b := sha256.New(), sha256.New()
	if !sdkWireHashJSON(a, content, 0) || !sdkWireHashJSON(b, response.Content, 0) || !bytes.Equal(a.Sum(nil), b.Sum(nil)) {
		s.err = ErrSDKSessionInvalid
	}
	return s.err
}

func (s *SDKSidechain) Leaf() string { s.mu.Lock(); defer s.mu.Unlock(); return s.leaf }
func (s *SDKSidechain) Failure() error {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.writer.failure(s.scope)
}
func (s *SDKSidechain) Flush() error { s.writer.drain(); return s.Failure() }
func (s *SDKSidechain) Path() string {
	if s.Failure() != nil {
		return ""
	}
	if _, err := s.ReadTail(1); err != nil {
		return ""
	}
	return s.path
}
func (s *SDKSidechain) ReadTail(limit int64) (string, error) {
	if err := s.Failure(); err != nil {
		return "", err
	}
	text, err := s.output.ReadTaskOutput(s.scope, limit)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	}
	return text, err
}
func (s *SDKSidechain) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.Flush()
	s.owner.mu.Lock()
	if s.owner.sidechains[s.scope] == s {
		delete(s.owner.sidechains, s.scope)
	}
	s.owner.mu.Unlock()
	return err
}
