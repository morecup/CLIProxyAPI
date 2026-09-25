package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// CredentialHealth is a passive observation, independent of the operator's
// enabled switch and scheduler cooldown. It never changes routing or credentials.
type CredentialHealth struct {
	State           string    `json:"state"`
	Message         string    `json:"message"`
	HTTPStatus      int       `json:"http_status"`
	FirstObservedAt time.Time `json:"first_observed_at"`
	LastObservedAt  time.Time `json:"last_observed_at"`
	ResolvedAt      time.Time `json:"resolved_at,omitzero"`
	Source          string    `json:"source"`
}

type credentialHealthRecord struct {
	Version     int               `json:"version"`
	Fingerprint string            `json:"credential_fingerprint"`
	Health      *CredentialHealth `json:"health,omitempty"`
}

var explicitAccountRestriction = regexp.MustCompile(`(?i)^(?:(?:your|this|the)\s+)?(account|organization)\s+(?:has been|is|was)\s+(?:permanently\s+)?(disabled|suspended|banned|deactivated|terminated)(?:[.\s:]|$)`)

func classifyCredentialHealth(err *Error) *CredentialHealth {
	if err == nil || shouldSkipCredentialCooldown(err) {
		return nil
	}
	status := statusCodeFromResult(err)
	if status != 401 && status != 403 && status != 429 {
		return nil
	}
	message := strings.TrimSpace(err.Message)
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &envelope) == nil && envelope.Error.Message != "" {
		message = strings.TrimSpace(envelope.Error.Message)
	}
	health := &CredentialHealth{HTTPStatus: status}
	lower := strings.ToLower(message)
	switch {
	case status == 429:
		health.State, health.Message = "rate_limited", "The upstream service reported a rate or usage limit."
	case lower == "oauth access token has been revoked." || lower == "oauth access token has been revoked" || lower == "access token has been revoked." || lower == "access token has been revoked":
		health.State, health.Message = "credential_revoked", "OAuth access token has been revoked."
	case explicitAccountRestriction.MatchString(message):
		if strings.EqualFold(explicitAccountRestriction.FindStringSubmatch(message)[1], "organization") {
			health.State, health.Message = "organization_disabled", "The upstream service explicitly reported that this organization is disabled or suspended."
		} else {
			health.State, health.Message = "account_disabled", "The upstream service explicitly reported that this account is disabled or suspended."
		}
	case status == 401:
		health.State, health.Message = "authentication_failed", "The upstream service rejected this credential (HTTP 401)."
	default:
		health.State, health.Message = "permission_denied", "The upstream service denied access (HTTP 403); this alone does not establish an account suspension."
	}
	return health
}

func (h *CredentialHealth) terminal() bool {
	return h != nil && (h.State == "credential_revoked" || h.State == "account_disabled" || h.State == "organization_disabled")
}

func credentialHealthFingerprint(auth *Auth) string {
	if auth == nil {
		return ""
	}
	secret := accessTokenForFingerprint(auth)
	if secret == "" {
		secret = auth.Attributes["api_key"]
	}
	if secret == "" {
		secret, _ = auth.Metadata["api_key"].(string)
	}
	if secret == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(auth.Provider + "\x00" + strings.TrimSpace(auth.Attributes["base_url"]) + "\x00" + strings.TrimSpace(secret)))
	return hex.EncodeToString(digest[:])
}

func (m *Manager) credentialHealthPath(auth *Auth) string {
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(auth.Provider + "\x00" + auth.ID))
	// The non-JSON suffix keeps the auth-file watcher from treating observations
	// as credentials. Diagnostic writes cannot reset credential cooldowns.
	return filepath.Join(cfg.AuthDir, ".credential-health", hex.EncodeToString(digest[:])+".health")
}

func (m *Manager) credentialHealthRecordLocked(auth *Auth) (*credentialHealthRecord, string) {
	if auth == nil || credentialHealthFingerprint(auth) == "" {
		return nil, ""
	}
	path := m.credentialHealthPath(auth)
	key := path + "\x00" + auth.ID
	if m.credentialHealthRecords == nil {
		m.credentialHealthRecords = make(map[string]*credentialHealthRecord)
	}
	record := m.credentialHealthRecords[key]
	if record == nil {
		record = &credentialHealthRecord{Version: 1}
		if path != "" {
			data, errRead := os.ReadFile(path)
			if errRead == nil {
				if json.Unmarshal(data, record) != nil || record.Version != 1 {
					record = &credentialHealthRecord{Version: 1}
					log.Warn("auth health: invalid persisted observation")
				}
			} else if !errors.Is(errRead, os.ErrNotExist) {
				log.Warn("auth health: failed to read persisted observation")
			}
		}
		m.credentialHealthRecords[key] = record
	}
	fingerprint := credentialHealthFingerprint(auth)
	if record.Fingerprint != fingerprint {
		record.Fingerprint = fingerprint
		record.Health = nil
	}
	return record, path
}

// CredentialHealthSnapshot returns the last observation for the current token.
// Re-enabling or restarting does not erase evidence; replacing the token makes
// the previous token's observation inapplicable without asserting recovery.
func (m *Manager) CredentialHealthSnapshot(authID string) *CredentialHealth {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, _ := m.credentialHealthRecordLocked(m.auths[authID])
	if record == nil || record.Health == nil {
		return nil
	}
	snapshot := *record.Health
	return &snapshot
}

func (m *Manager) observeCredentialHealthLocked(auth *Auth, result Result, now time.Time) {
	if result.credentialFingerprint != "" && result.credentialFingerprint != credentialHealthFingerprint(auth) {
		return
	}
	health := classifyCredentialHealth(result.Error)
	if !result.Success && health == nil {
		return
	}
	record, path := m.credentialHealthRecordLocked(auth)
	if record == nil {
		return
	}
	if result.Success {
		// A late success from a concurrent request must not hide a revoked token.
		if record.Health == nil || record.Health.terminal() || !record.Health.ResolvedAt.IsZero() {
			return
		}
		record.Health.ResolvedAt = now.UTC()
	} else {
		health.FirstObservedAt, health.LastObservedAt = now.UTC(), now.UTC()
		health.Source = "upstream_response"
		if !mergeCredentialHealth(record, health) {
			return
		}
	}
	if errSave := saveCredentialHealth(path, record); errSave != nil {
		log.Warn("auth health: failed to persist observation")
	}
}

func mergeCredentialHealth(record *credentialHealthRecord, next *CredentialHealth) bool {
	previous := record.Health
	if previous != nil {
		if next.LastObservedAt.Before(previous.LastObservedAt) || !previous.ResolvedAt.IsZero() && !next.LastObservedAt.After(previous.ResolvedAt) {
			return false
		}
		if previous.terminal() && !next.terminal() {
			return false
		}
		if (previous.State == "account_disabled" || previous.State == "organization_disabled") && next.State == "credential_revoked" {
			return false
		}
		if previous.State == next.State && previous.ResolvedAt.IsZero() && previous.FirstObservedAt.Before(next.FirstObservedAt) {
			next.FirstObservedAt = previous.FirstObservedAt
		}
	}
	record.Health = next
	return true
}

// ImportCredentialHealth records verified historical upstream evidence without
// issuing a provider request, rewriting tokens, or fabricating request counters.
func (m *Manager) ImportCredentialHealth(authID, expectedFingerprint string, err *Error, first, last time.Time) error {
	health := classifyCredentialHealth(err)
	if health == nil || first.Before(time.Unix(0, 0)) || last.Before(first) || last.After(time.Now().Add(time.Minute)) {
		return fmt.Errorf("invalid credential health observation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil || expectedFingerprint == "" || credentialHealthFingerprint(auth) != expectedFingerprint {
		return fmt.Errorf("credential changed or no longer exists")
	}
	record, path := m.credentialHealthRecordLocked(auth)
	if record == nil || path == "" {
		return fmt.Errorf("credential health persistence is unavailable")
	}
	health.FirstObservedAt, health.LastObservedAt, health.Source = first.UTC(), last.UTC(), "historical_log"
	previous := record.Health
	if !mergeCredentialHealth(record, health) {
		return fmt.Errorf("newer or more specific credential health evidence already exists")
	}
	if errSave := saveCredentialHealth(path, record); errSave != nil {
		record.Health = previous
		return fmt.Errorf("failed to persist credential health observation")
	}
	return nil
}

// CredentialHealthFingerprint binds a management import to its auth snapshot.
// This digest is for internal comparisons and must not be exposed in responses.
func CredentialHealthFingerprint(auth *Auth) string { return credentialHealthFingerprint(auth) }

func saveCredentialHealth(path string, record *credentialHealthRecord) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".health-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err = temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
