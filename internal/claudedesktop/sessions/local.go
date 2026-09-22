package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

// LocalConversation is stored with the existing protected account catalog.
// It is the local UI's request history, not a replacement SDK resume authority.
type LocalConversation struct {
	Model          string                  `json:"model"`
	Folder         string                  `json:"folder"`
	InitialMessage string                  `json:"initial_message,omitempty"`
	Messages       []LocalMessage          `json:"messages"`
	LastError      string                  `json:"last_error,omitempty"`
	LastTurn       LocalTurnObservation    `json:"last_turn"`
	Startup        LocalStartupObservation `json:"startup"`
}

type LocalMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type LocalStartupObservation struct {
	StartedAt   time.Time `json:"started_at"`
	PreflightMS int64     `json:"preflight_ms"`
	QueryMS     int64     `json:"query_ms"`
	InitMS      int64     `json:"init_ms"`
	HasRepo     *bool     `json:"has_repo,omitempty"`
}

type LocalTurnObservation struct {
	PromptID         string    `json:"prompt_id,omitempty"`
	AssistantID      string    `json:"assistant_id,omitempty"`
	RequestID        string    `json:"request_id,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	FirstAssistantAt time.Time `json:"first_assistant_at"`
	CompletedAt      time.Time `json:"completed_at"`
	RequestCount     int       `json:"request_count"`
	ToolCount        int       `json:"tool_count"`
	IsFirstTurn      bool      `json:"is_first_turn"`
}

func cloneLocal(value *LocalConversation) LocalConversation {
	if value == nil {
		return LocalConversation{}
	}
	result := *value
	result.Messages = append([]LocalMessage{}, value.Messages...)
	for i := range result.Messages {
		result.Messages[i].Content = append(json.RawMessage(nil), result.Messages[i].Content...)
	}
	return result
}

func (r *Registry) localLocked(owner, id string) *record {
	for _, value := range r.records {
		if value.Owner == owner && value.ID == id && value.Local != nil {
			return value
		}
	}
	return nil
}

// CreateLocal admits an actual host and persists the new record before returning.
func (r *Registry) CreateLocal(ctx context.Context, owner string, conversation LocalConversation, start func(string) (*features.Host, error)) (Snapshot, error) {
	if r == nil || ctx == nil || !validKey(owner) || !filepath.IsAbs(conversation.Folder) || strings.TrimSpace(conversation.Model) == "" || strings.TrimSpace(conversation.InitialMessage) == "" || start == nil {
		return Snapshot{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := r.loadLocked(); err != nil {
		return Snapshot{}, err
	}
	id := "local_" + uuid.NewString()
	digest := sha256.Sum256([]byte("desktop-local-conversation-v1:" + owner + ":" + id))
	scope := hex.EncodeToString(digest[:])
	host, err := start(scope)
	if err != nil || host == nil {
		return Snapshot{}, errors.Join(ErrUnavailable, err)
	}
	conversation.Messages = []LocalMessage{}
	value := &record{ID: id, Generation: uuid.NewString(), Owner: owner, Scope: scope, SDKSessionID: host.SessionID(), CreatedAt: time.Now().UTC(), host: host, Local: &conversation}
	r.records[scope] = value
	if err := r.saveLocked(); err != nil {
		delete(r.records, scope)
		return snapshot(value), err
	}
	return snapshot(value), nil
}

func (r *Registry) LocalConversation(owner, id string) (Snapshot, LocalConversation, bool, error) {
	if r == nil || !validKey(owner) {
		return Snapshot{}, LocalConversation{}, false, ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return Snapshot{}, LocalConversation{}, false, err
	}
	value := r.localLocked(owner, id)
	if value == nil {
		return Snapshot{}, LocalConversation{}, false, ErrNotFound
	}
	return snapshot(value), cloneLocal(value.Local), value.localTurn != nil, nil
}

func (r *Registry) SetLocalStartup(owner, id string, queryMS, initMS int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.localLocked(owner, id)
	if value == nil {
		return ErrNotFound
	}
	value.Local.Startup.QueryMS, value.Local.Startup.InitMS = queryMS, initMS
	return r.saveLocked()
}

// ResumeLocal preserves the durable SDK session and UI history. The old request
// must have actually completed before its successor is admitted.
func (r *Registry) ResumeLocal(ctx context.Context, owner, id, generation string, start func(string, string) (*features.Host, error)) (Snapshot, error) {
	if r == nil || ctx == nil || start == nil {
		return Snapshot{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return Snapshot{}, err
	}
	value := r.localLocked(owner, id)
	if value == nil {
		return Snapshot{}, ErrNotFound
	}
	if value.Generation != generation || value.localTurn != nil {
		return snapshot(value), ErrStaleQuery
	}
	if err := ctx.Err(); err != nil {
		return snapshot(value), err
	}
	if before := snapshot(value); before.Running {
		return before, nil
	}
	host, err := start(value.Scope, value.SDKSessionID)
	if err != nil || host == nil {
		return snapshot(value), errors.Join(ErrUnavailable, err)
	}
	oldHost, oldGeneration := value.host, value.Generation
	value.host, value.Generation = host, uuid.NewString()
	if err := r.saveLocked(); err != nil {
		value.host, value.Generation = oldHost, oldGeneration
		return snapshot(value), err
	}
	return snapshot(value), nil
}

type LocalTurn struct {
	registry  *Registry
	value     *record
	host      *features.Host
	done      chan struct{}
	once      sync.Once
	finishErr error
}

// BeginLocalTurn excludes duplicate submissions and durably records the input.
func (r *Registry) BeginLocalTurn(ctx context.Context, owner, id, generation, message string) (*LocalTurn, Snapshot, LocalConversation, error) {
	if r == nil || ctx == nil || strings.TrimSpace(message) == "" {
		return nil, Snapshot{}, LocalConversation{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return nil, Snapshot{}, LocalConversation{}, err
	}
	value := r.localLocked(owner, id)
	if value == nil {
		return nil, Snapshot{}, LocalConversation{}, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, snapshot(value), LocalConversation{}, err
	}
	if value.Generation != generation || value.localTurn != nil || !snapshot(value).Running {
		return nil, snapshot(value), LocalConversation{}, ErrStaleQuery
	}
	if value.Local.InitialMessage != "" && value.Local.InitialMessage != message {
		return nil, snapshot(value), LocalConversation{}, ErrInvalid
	}
	before := cloneLocal(value.Local)
	content, _ := json.Marshal(message)
	idPrompt := uuid.NewString()
	value.Local.Messages = append(value.Local.Messages, LocalMessage{ID: idPrompt, Role: "user", Content: content})
	value.Local.InitialMessage, value.Local.LastError = "", ""
	value.Local.LastTurn = LocalTurnObservation{PromptID: idPrompt, StartedAt: time.Now().UTC(), IsFirstTurn: len(before.Messages) == 0}
	if err := r.saveLocked(); err != nil {
		value.Local = &before
		return nil, snapshot(value), LocalConversation{}, err
	}
	turn := &LocalTurn{registry: r, value: value, host: value.host, done: make(chan struct{})}
	value.localTurn, value.inputDone = turn, turn.done
	return turn, snapshot(value), cloneLocal(value.Local), nil
}

func (t *LocalTurn) Host() *features.Host {
	if t == nil {
		return nil
	}
	return t.host
}

func (t *LocalTurn) Finish(response json.RawMessage, observed LocalTurnObservation, failure error) error {
	if t == nil {
		return ErrInvalid
	}
	t.once.Do(func() {
		r := t.registry
		r.mu.Lock()
		defer r.mu.Unlock()
		defer close(t.done)
		value := t.value
		before := cloneLocal(value.Local)
		if value.localTurn != t {
			t.finishErr = ErrStaleQuery
			return
		}
		defer func() {
			value.localTurn = nil
			if value.inputDone == t.done {
				value.inputDone = nil
			}
		}()
		if observed.PromptID == "" {
			observed.PromptID = value.Local.LastTurn.PromptID
		}
		observed.IsFirstTurn = value.Local.LastTurn.IsFirstTurn
		observed.StartedAt = value.Local.LastTurn.StartedAt
		observed.CompletedAt = time.Now().UTC()
		if failure == nil {
			var message struct {
				ID      string          `json:"id"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(response, &message) != nil || message.Role != "assistant" || message.ID == "" || len(message.Content) == 0 {
				failure = ErrInvalid
			} else {
				observed.AssistantID = message.ID
				value.Local.Messages = append(value.Local.Messages, LocalMessage{ID: message.ID, Role: "assistant", Content: message.Content})
			}
		}
		value.Local.LastTurn = observed
		if failure != nil {
			value.Local.LastError = failure.Error()
		}
		t.finishErr = r.saveLocked()
		if t.finishErr != nil {
			value.Local = &before
			value.Local.LastError = fmt.Sprintf("local response was not saved: %v", t.finishErr)
		}
	})
	return t.finishErr
}

// ClaimUIObservation ties browser facts to a real account/record generation.
// The bounded process-local deduplication set is not durable execution authority.
func (r *Registry) ClaimUIObservation(owner, key, id, generation string, prerequisites ...string) (bool, Snapshot, LocalConversation, error) {
	if r == nil || !validKey(owner) || key == "" {
		return false, Snapshot{}, LocalConversation{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return false, Snapshot{}, LocalConversation{}, err
	}
	var view Snapshot
	var conversation LocalConversation
	if id != "" {
		value := r.localLocked(owner, id)
		if value == nil {
			return false, view, conversation, ErrNotFound
		}
		if value.Generation != generation {
			return false, snapshot(value), conversation, ErrStaleQuery
		}
		view, conversation = snapshot(value), cloneLocal(value.Local)
	}
	full := owner + ":" + key
	if r.uiObservations == nil {
		r.uiObservations = make(map[string]time.Time)
	}
	for _, prerequisite := range prerequisites {
		if _, found := r.uiObservations[owner+":"+prerequisite]; !found {
			return false, view, conversation, ErrInvalid
		}
	}
	if _, found := r.uiObservations[full]; found {
		return false, view, conversation, nil
	}
	if len(r.uiObservations) >= 1024 {
		var oldest string
		var at time.Time
		for name, value := range r.uiObservations {
			if oldest == "" || value.Before(at) {
				oldest, at = name, value
			}
		}
		delete(r.uiObservations, oldest)
	}
	r.uiObservations[full] = time.Now()
	return true, view, conversation, nil
}

// WaitLocalTurn joins a retired local request outside the record lock. A canceled
// host is not proof that response persistence and accounting have completed.
func (r *Registry) WaitLocalTurn(ctx context.Context, owner, id, query string) error {
	r.mu.Lock()
	value := r.localLocked(owner, id)
	var done <-chan struct{}
	if value != nil && snapshot(value).QueryID == query && value.localTurn != nil {
		done = value.localTurn.done
	}
	r.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
