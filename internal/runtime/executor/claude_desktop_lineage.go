package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	claudeDesktopPromptIDMetadataKey        = "claude_desktop_prompt_id"
	claudeDesktopClientRequestIDMetadataKey = "claude_desktop_client_request_id"
	claudeDesktopLineageTTL                 = time.Hour
)

type claudeDesktopLineageEntry struct {
	previousRequestID string
	minimumSequence   uint64
	committedSequence uint64
	expiresAt         time.Time
}

type claudeDesktopLineageRequestState struct {
	key      string
	sequence uint64
	durable  *claudetelemetry.LineageToken
}

type claudeDesktopLineageStore struct {
	sync.Mutex
	entries      map[string]claudeDesktopLineageEntry
	nextSequence uint64
}

func (s *claudeDesktopLineageStore) begin(auth *cliproxyauth.Auth, sessionID string) (claudeDesktopLineageRequestState, string) {
	credentialIdentity := claudeDiagnosticsCredentialIdentity(auth)
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || credentialIdentity == "" || sessionID == "" {
		return claudeDesktopLineageRequestState{}, ""
	}
	digest := sha256.Sum256([]byte(credentialIdentity + "\x00" + sessionID))
	key := hex.EncodeToString(digest[:])
	now := time.Now()

	s.Lock()
	defer s.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]claudeDesktopLineageEntry)
	}
	for candidate, entry := range s.entries {
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			delete(s.entries, candidate)
		}
	}
	s.nextSequence++
	sequence := s.nextSequence
	entry, found := s.entries[key]
	if !found {
		entry.minimumSequence = sequence
	}
	entry.expiresAt = now.Add(claudeDesktopLineageTTL)
	s.entries[key] = entry
	return claudeDesktopLineageRequestState{key: key, sequence: sequence}, entry.previousRequestID
}

func (s *claudeDesktopLineageStore) commit(state claudeDesktopLineageRequestState, requestID string) {
	requestID = strings.TrimSpace(requestID)
	if s == nil || state.key == "" || state.sequence == 0 || !strings.HasPrefix(requestID, "req_") {
		return
	}
	now := time.Now()
	s.Lock()
	defer s.Unlock()
	entry, ok := s.entries[state.key]
	if !ok || state.sequence < entry.minimumSequence || state.sequence < entry.committedSequence {
		return
	}
	entry.previousRequestID = requestID
	entry.committedSequence = state.sequence
	entry.expiresAt = now.Add(claudeDesktopLineageTTL)
	s.entries[state.key] = entry
}

func (e *ClaudeExecutor) beginClaudeDesktopRequestLineage(auth *cliproxyauth.Auth, sessionID string) (claudeDesktopLineageRequestState, string, error) {
	if e != nil && e.desktopTelemetry != nil && e.desktopTelemetry.Enabled() {
		token, previousRequestID, errBegin := e.desktopTelemetry.BeginLineage(auth, sessionID)
		if errBegin != nil {
			return claudeDesktopLineageRequestState{}, "", fmt.Errorf("begin Claude Desktop session lineage: %w", errBegin)
		}
		if token != nil && token.Active() {
			return claudeDesktopLineageRequestState{durable: token}, previousRequestID, nil
		}
	}
	if e == nil {
		return claudeDesktopLineageRequestState{}, "", nil
	}
	state, previousRequestID := e.desktopLineage.begin(auth, sessionID)
	return state, previousRequestID, nil
}

func (e *ClaudeExecutor) commitClaudeDesktopRequestLineage(state claudeDesktopLineageRequestState, requestID string) error {
	if state.durable != nil && state.durable.Active() {
		if errCommit := state.durable.Commit(requestID); errCommit != nil {
			return fmt.Errorf("commit Claude Desktop session lineage: %w", errCommit)
		}
		return nil
	}
	if e != nil {
		e.desktopLineage.commit(state, requestID)
	}
	return nil
}

func claudeDesktopRequestUUID(metadata ...map[string]any) (promptID, clientRequestID string) {
	promptID = existingClaudeDesktopUUID(claudeDesktopPromptIDMetadataKey, metadata...)
	if promptID == "" {
		promptID = uuid.New().String()
	}
	clientRequestID = existingClaudeDesktopUUID(claudeDesktopClientRequestIDMetadataKey, metadata...)
	if clientRequestID == "" {
		clientRequestID = uuid.New().String()
	}
	for _, values := range metadata {
		if values == nil {
			continue
		}
		values[claudeDesktopPromptIDMetadataKey] = promptID
		values[claudeDesktopClientRequestIDMetadataKey] = clientRequestID
	}
	return promptID, clientRequestID
}

func existingClaudeDesktopUUID(key string, metadata ...map[string]any) string {
	for _, values := range metadata {
		value, _ := values[key].(string)
		value = strings.TrimSpace(value)
		if _, errParse := uuid.Parse(value); errParse == nil {
			return value
		}
	}
	return ""
}

func claudeDesktopResponseRequestID(headers http.Header) string {
	for _, name := range []string{"request-id", "x-request-id"} {
		if requestID := strings.TrimSpace(headers.Get(name)); strings.HasPrefix(requestID, "req_") {
			return requestID
		}
	}
	return ""
}
