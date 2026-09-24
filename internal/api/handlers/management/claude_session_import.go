package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

const maxClaudeSessionImportBody = 16 << 10

type claudeSessionImportRequest struct {
	SessionKey string `json:"session_key"`
	ProxyURL   string `json:"proxy_url,omitempty"`
}

// ImportClaudeSession imports an already authenticated Claude.ai sessionKey.
// The credential is accepted only through the authenticated Management API,
// kept in request memory, and persisted through the encrypted Claude Desktop
// credential store after OAuth and trusted-device enrollment succeed.
func (h *Handler) ImportClaudeSession(c *gin.Context) {
	if h == nil || c == nil || c.Request == nil {
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxClaudeSessionImportBody))
	decoder.DisallowUnknownFields()
	var body claudeSessionImportRequest
	if errDecode := decoder.Decode(&body); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid Claude sessionKey request"})
		return
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid Claude sessionKey request"})
		return
	}
	sessionKey := strings.TrimSpace(body.SessionKey)
	proxyURL, errProxy := normalizeClaudeLoginProxyURL(body.ProxyURL)
	body.SessionKey = ""
	body.ProxyURL = ""
	if sessionKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Claude sessionKey is required"})
		return
	}
	if errProxy != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errProxy.Error()})
		return
	}

	h.mu.Lock()
	var cfg *config.Config
	if h.cfg != nil {
		cfg = cloneClaudeLoginConfig(h.cfg, proxyURL)
	}
	login := h.claudeSessionKeyLogin
	h.mu.Unlock()
	if cfg == nil || login == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "Claude sessionKey import is unavailable"})
		return
	}

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to initialize Claude sessionKey import"})
		return
	}
	RegisterOAuthSession(state, "anthropic")
	authContext := PopulateAuthContext(context.Background(), c)
	go func(importKey, loginProxyURL string) {
		importCtx, cancelImport := context.WithCancel(authContext)
		defer cancelImport()
		go watchOAuthSessionCancel(importCtx, cancelImport, state, "anthropic")

		record, errLogin := login(importCtx, cfg, importKey)
		safeLoginError := redactClaudeSessionImportError(errLogin, importKey)
		importKey = ""
		if errLogin != nil {
			if !IsOAuthSessionPending(state, "anthropic") {
				return
			}
			log.WithError(safeLoginError).Warn("Claude sessionKey import failed")
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Claude sessionKey import failed", safeLoginError))
			return
		}
		if record == nil {
			SetOAuthSessionError(state, "Claude sessionKey import returned no credential")
			return
		}
		applyClaudeLoginProxy(record, loginProxyURL)
		if errGuard := guardOAuthSessionPendingForSave(state, "anthropic"); errGuard != nil {
			return
		}
		if _, errSave := h.saveTokenRecord(importCtx, record); errSave != nil {
			log.WithError(errSave).Error("failed to save imported Claude credential")
			SetOAuthSessionError(state, "failed to save imported Claude credential")
			return
		}
		log.WithField("auth_file", record.FileName).Info("Claude sessionKey credential imported")
		CompleteOAuthSession(state)
	}(sessionKey, proxyURL)
	sessionKey = ""

	c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "flow": "session_key"})
}

func redactClaudeSessionImportError(err error, sessionKey string) error {
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(err.Error())
	if key := strings.TrimSpace(sessionKey); key != "" {
		detail = strings.ReplaceAll(detail, key, "[redacted]")
	}
	if detail == "" {
		detail = "Claude sessionKey import failed"
	}
	return errors.New(detail)
}
