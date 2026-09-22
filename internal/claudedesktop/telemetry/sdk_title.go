package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const FactSDKTitleGenerated = "session_title_generated"

// This event uses the session's lifecycle dimensions, not the title model's
// request beta list. All 18 v140609 observations have this exact value.
const sdkTitleLifecycleBetas = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07"

const maxTitleResponseBytes = 64 * 1024 // Memory bound, not a Desktop title limit.

type sdkPromptParent struct {
	owner    *claudeprompt.Request
	promptID string
	model    string
	at       time.Time
}

func sdkParentKey(worker *accountWorker, session, prompt string) string {
	id, errParse := uuid.Parse(strings.TrimSpace(prompt))
	if worker == nil || session == "" || errParse != nil || id == uuid.Nil {
		return ""
	}
	return worker.binding.BindingRevision + "\x00" + session + "\x00" + id.String()
}

func (s *RequestSpan) observeSDKParent(facts RequestFacts) {
	if s.sdkWorker == nil || facts.Role != claudeprofile.RoleMain {
		return
	}
	key := sdkParentKey(s.sdkWorker, facts.SessionID, facts.PromptID)
	if key == "" {
		return
	}
	m := s.manager
	m.sdkParentMu.Lock()
	defer m.sdkParentMu.Unlock()
	if m.sdkParents == nil {
		m.sdkParents = make(map[string]sdkPromptParent)
	}
	now := m.now()
	oldestKey := ""
	var oldest time.Time
	for candidate, parent := range m.sdkParents {
		if now.Sub(parent.at) > time.Hour {
			delete(m.sdkParents, candidate)
			continue
		}
		if oldestKey == "" || parent.at.Before(oldest) {
			oldestKey, oldest = candidate, parent.at
		}
	}
	if _, exists := m.sdkParents[key]; exists {
		// Tool continuations/retries cannot change the original prompt model.
		return
	}
	if len(m.sdkParents) >= maxPromptIssues {
		delete(m.sdkParents, oldestKey)
	}
	id, _ := uuid.Parse(facts.PromptID)
	m.sdkParents[key] = sdkPromptParent{owner: facts.Prompt, promptID: id.String(), model: facts.Model, at: now}
}

func (s *RequestSpan) sdkTitleParent() *sdkPromptParent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.titleParent != nil {
		return s.titleParent
	}
	key := sdkParentKey(s.sdkWorker, s.facts.SessionID, s.facts.ParentPromptID)
	if key == "" {
		return nil
	}
	s.manager.sdkParentMu.Lock()
	parent, found := s.manager.sdkParents[key]
	s.manager.sdkParentMu.Unlock()
	if !found || s.manager.now().Sub(parent.at) > time.Hour {
		return nil
	}
	s.titleParent = &parent
	return s.titleParent
}

type sdkTitleResponse struct {
	text         []byte
	started      bool
	block        bool
	blockDone    bool
	blockType    string
	nextIndex    int64
	stopReason   string
	complete     bool
	outcomeKnown bool
	valid        bool
	invalid      bool
	textOverflow bool
}

func (r *sdkTitleResponse) append(text string) {
	if r.invalid || r.textOverflow {
		return
	}
	if len(text) > maxTitleResponseBytes-len(r.text) {
		r.textOverflow, r.text = true, nil
		return
	}
	r.text = append(r.text, text...)
}

func (r *sdkTitleResponse) finish() {
	var output map[string]json.RawMessage
	var title string
	r.complete = true
	// Native generation concatenates text blocks, strips optional code fences
	// and calls JSON.parse. It does not repair JSON: complete malformed text is
	// a known negative, distinct from a missing or truncated response.
	r.outcomeKnown = !r.invalid && !r.textOverflow && r.blockDone && !r.block && r.stopReason != ""
	// Native schema property names are case-sensitive. A Go struct would also
	// accept TITLE/Title and could turn a schema failure into a successful title.
	r.valid = r.outcomeKnown && json.Unmarshal([]byte(sdkTitleJSONText(string(r.text))), &output) == nil && json.Unmarshal(output["title"], &title) == nil && strings.TrimFunc(title, sdkTitleJSWhitespace) != ""
	r.text = nil
}

// Mirrors native trim/ASCII-language-fence/trim order. Go regexp's \s and
// strings.TrimSpace do not have JavaScript's whitespace membership.
func sdkTitleJSONText(text string) string {
	text = strings.TrimFunc(text, sdkTitleJSWhitespace)
	if strings.HasPrefix(text, "```") {
		text = text[3:]
		for len(text) > 0 && (text[0] >= 'a' && text[0] <= 'z' || text[0] >= 'A' && text[0] <= 'Z') {
			text = text[1:]
		}
		text = strings.TrimLeftFunc(text, sdkTitleJSWhitespace)
	}
	text = strings.TrimSuffix(text, "```")
	return strings.TrimFunc(text, sdkTitleJSWhitespace)
}

// The native title factory uses JavaScript trim: U+FEFF is whitespace but
// U+0085 is not. Go strings.TrimSpace has different membership.
func sdkTitleJSWhitespace(r rune) bool {
	return r >= '\t' && r <= '\r' || r == ' ' || r == '\u00a0' || r == '\u1680' ||
		r >= '\u2000' && r <= '\u200a' || r == '\u2028' || r == '\u2029' || r == '\u202f' ||
		r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

// Only title-role text blocks are decoded transiently. Tool arguments, thinking
// and ordinary assistant text can never supply a title. No title is persisted.
func (s *RequestSpan) observeSDKTitleResponse(root gjson.Result) {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || !s.requestObserved || s.facts.Role != claudeprofile.RoleTitle || !s.responseObserved || s.responseStatus < 200 || s.responseStatus >= 300 {
		return
	}
	r := &s.titleResponse
	if r.complete {
		return
	}
	switch root.Get("type").String() {
	case "message":
		content := root.Get("content")
		if r.started || root.Get("role").String() != "assistant" || !content.IsArray() {
			r.invalid = true
		} else {
			for _, block := range content.Array() {
				if block.Get("type").String() == "text" {
					if text := block.Get("text"); text.Type == gjson.String {
						r.append(text.String())
					} else {
						r.invalid = true
					}
				}
			}
		}
		r.blockDone = true
		r.stopReason = root.Get("stop_reason").String()
		r.finish()
	case "message_start":
		if r.started || root.Get("message.role").String() != "assistant" || len(root.Get("message.content").Array()) != 0 {
			r.invalid = true
		}
		r.started = true
		r.blockDone = true
	case "content_block_start":
		if !r.started || r.block || !root.Get("index").Exists() || root.Get("index").Int() != r.nextIndex {
			r.invalid = true
		}
		r.block = true
		r.blockDone = false
		r.blockType = root.Get("content_block.type").String()
		// The SDK yields one assistant per completed stream block. Its title
		// helper returns the last assistant, not the concatenated HTTP message.
		// Native stream assembly also starts text from empty at block_start.
		r.text, r.textOverflow = nil, false
	case "content_block_delta":
		if !r.block || root.Get("index").Int() != r.nextIndex {
			r.invalid = true
		}
		if root.Get("delta.type").String() == "text_delta" {
			if text := root.Get("delta.text"); r.blockType == "text" && text.Type == gjson.String {
				r.append(text.String())
			} else {
				r.invalid = true
			}
		} else if r.blockType == "text" {
			r.invalid = true
		}
	case "content_block_stop":
		if !r.block || root.Get("index").Int() != r.nextIndex {
			r.invalid = true
		}
		r.block = false
		r.blockDone = true
		r.nextIndex++
	case "message_delta":
		r.stopReason = root.Get("delta.stop_reason").String()
	case "message_stop":
		if !r.started {
			r.invalid = true
		}
		r.finish()
	case "error":
		r.invalid, r.text = true, nil
	}
}

func (m *Manager) finishSDKTitle(span *RequestSpan, category, errorClass string) error {
	span.mu.Lock()
	facts := span.facts
	valid := span.titleResponse.valid && span.titleResponse.complete
	known := span.titleResponse.outcomeKnown && span.titleResponse.complete
	span.titleResponse.text = nil
	span.mu.Unlock()
	if facts.Role != claudeprofile.RoleTitle || category != "" || errorClass != "" {
		return nil
	}
	return m.finishSDKTitleOutcome(span, known, valid)
}

// TitleFinalizerKey identifies this title generation within one conductor call.
// Raw HttpRequest retries may allocate new request/prompt IDs, so those attempt
// IDs cannot be the retry key. The account binding, session, explicit parent
// and chain start isolate concurrent calls without choosing the newest prompt.
func (s *RequestSpan) TitleFinalizerKey() string {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.facts.Role != claudeprofile.RoleTitle || !s.requestObserved {
		return ""
	}
	encoded, _ := json.Marshal([]string{s.sdkWorker.binding.BindingRevision, s.facts.SessionID,
		s.facts.ParentPromptID, s.facts.ChainStartedAt.UTC().Format(time.RFC3339Nano)})
	digest := sha256.Sum256(encoded)
	return "claude-desktop-title:" + hex.EncodeToString(digest[:])
}

// FinishTitleFailure is called only after the outermost retry owner terminates
// generation (or immediately for a direct invocation). Native generation catches
// query exceptions and emits success=false; an individual API retry is not that
// terminal outcome. Error text is neither retained nor exported here.
func (s *RequestSpan) FinishTitleFailure() {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	failed := s.finished && s.titleAttemptFailed
	s.mu.Unlock()
	if !failed {
		return
	}
	if errTitle := s.manager.finishSDKTitleOutcome(s, true, false); errTitle != nil {
		s.sdkWorker.recordQueueFailure(errTitle)
		log.WithError(errTitle).Warn("claude desktop SDK telemetry: terminal title event was not persisted")
	}
}

func (m *Manager) finishSDKTitleOutcome(span *RequestSpan, known, valid bool) error {
	span.mu.Lock()
	if span.facts.Role != claudeprofile.RoleTitle || !span.requestObserved || span.titleOutcomeFinished {
		span.mu.Unlock()
		return nil
	}
	span.titleOutcomeFinished = true
	facts := span.facts
	span.mu.Unlock()
	if errClear := span.sdkWorker.setFactIssue(factIssueTitle, facts.SessionID, span.TitleFinalizerKey(), false); errClear != nil {
		span.sdkWorker.recordQueueFailure(errClear)
	}
	parent := span.sdkTitleParent()
	if parent == nil || !known {
		return span.sdkWorker.setFactIssue(factIssueTitle, facts.SessionID, requestFactOwner(facts), true)
	}
	// Only this post-generation event uses the parent session dimensions. The
	// title request and API-success event retain their own model/header/system.
	facts.PromptID, facts.Model, facts.Betas = parent.promptID, parent.model, sdkTitleLifecycleBetas
	if errEnqueue := m.enqueueSDKEvent(span.sdkWorker, FactSDKTitleGenerated, facts, struct {
		SubscriptionType string `json:"subscription_type"`
		PromptID         string `json:"cc_prompt_id"`
		Success          bool   `json:"success"`
	}{subscriptionType(span.sdkWorker.authSnapshot()), parent.promptID, valid}); errEnqueue != nil {
		return errEnqueue
	}
	return span.sdkWorker.setFactIssue(factIssueTitle, facts.SessionID, requestFactOwner(facts), false)
}
