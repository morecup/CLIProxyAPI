package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	proxyURL, errProxy := readClaudeLoginProxy(c)
	if errProxy != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errProxy.Error()})
		return
	}

	h.mu.Lock()
	cfg := cloneClaudeLoginConfig(h.cfg, proxyURL)
	login := h.claudeLogin
	h.mu.Unlock()
	if cfg == nil || login == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "Claude Desktop authentication is unavailable"})
		return
	}

	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Claude Desktop authentication...")
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	RegisterOAuthSession(state, "anthropic")
	go func() {
		defer claudedesktop.CancelMagicLinkBrowserSession(state)
		record, errLogin := login(ctx, cfg, &sdkAuth.LoginOptions{
			Metadata: map[string]string{
				claudedesktop.InteractiveSessionMetadataKey: state,
			},
			Prompt: func(string) (string, error) {
				return WaitOAuthSessionInput(ctx, state, "anthropic")
			},
		})
		if errLogin != nil {
			log.WithError(errLogin).Error("Claude Desktop authentication failed")
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Claude Desktop authentication failed", errLogin))
			return
		}
		applyClaudeLoginProxy(record, proxyURL)
		if errGuard := guardOAuthSessionPendingForSave(state, "anthropic"); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Claude Desktop authentication and enrollment saved to %s\n", savedPath)
		CompleteOAuthSession(state)
	}()

	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"state":     state,
		"flow":      "magic_link",
		"login_url": claudedesktop.DefaultLoginURL,
	})
}

// CancelAuthSession cancels a pending OAuth session identified by state.
// Protected by management auth. Safe for both callback and device-code flows:
// waiters check IsOAuthSessionPending and exit without saving credentials.
func (h *Handler) CancelAuthSession(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "missing state"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	cancelled := CancelOAuthSession(state)
	if cancelled {
		claudedesktop.CancelMagicLinkBrowserSession(state)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "cancelled": cancelled})
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	provider, status, isPlugin, metadata, completed, ok := GetOAuthSessionDetails(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if completed {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if status != "" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": status})
		return
	}
	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()
	if isPlugin && host != nil && host.HasAuthProvider(provider) {
		ctx := PopulateAuthContext(context.Background(), c)
		resp, handled, errPoll := host.PollLogin(ctx, provider, state, metadata)
		if handled {
			if errPoll != nil {
				message := strings.TrimSpace(errPoll.Error())
				if message == "" {
					message = "Authentication failed"
				}
				SetOAuthSessionError(state, message)
				c.JSON(http.StatusOK, gin.H{"status": "error", "error": message})
				return
			}
			switch resp.Status {
			case "", pluginapi.AuthLoginStatusPending:
				c.JSON(http.StatusOK, gin.H{"status": "wait"})
				return
			case pluginapi.AuthLoginStatusError:
				message := strings.TrimSpace(resp.Message)
				if message == "" {
					message = "Authentication failed"
				}
				SetOAuthSessionError(state, message)
				c.JSON(http.StatusOK, gin.H{"status": "error", "error": message})
				return
			case pluginapi.AuthLoginStatusSuccess:
				records := pluginLoginPollAuths(host, resp)
				if len(records) == 0 {
					SetOAuthSessionError(state, "Authentication failed")
					c.JSON(http.StatusOK, gin.H{"status": "error", "error": "Authentication failed"})
					return
				}
				if errSave := h.savePluginLoginRecords(ctx, records); errSave != nil {
					log.WithError(errSave).WithField("provider", provider).Error("failed to save plugin auth tokens")
					SetOAuthSessionError(state, "Failed to save authentication tokens")
					c.JSON(http.StatusOK, gin.H{"status": "error", "error": "Failed to save authentication tokens"})
					return
				}
				CompleteOAuthSession(state)
				c.JSON(http.StatusOK, gin.H{"status": "ok"})
				return
			default:
				c.JSON(http.StatusOK, gin.H{"status": "wait"})
				return
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait"})
}

func pluginLoginPollAuths(host *pluginhost.Host, resp pluginapi.AuthLoginPollResponse) []*coreauth.Auth {
	if host == nil {
		return nil
	}
	authDatas := resp.Auths
	if len(authDatas) == 0 {
		authDatas = []pluginapi.AuthData{resp.Auth}
	}
	records := make([]*coreauth.Auth, 0, len(authDatas))
	for _, authData := range authDatas {
		record := host.AuthDataToCoreAuth(authData, "", "")
		if record == nil {
			return nil
		}
		records = append(records, record)
	}
	return records
}

func (h *Handler) savePluginLoginRecords(ctx context.Context, records []*coreauth.Auth) error {
	savedPaths := make([]string, 0, len(records))
	for _, record := range records {
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if strings.TrimSpace(savedPath) != "" {
			savedPaths = append(savedPaths, savedPath)
		}
		if errSave != nil {
			h.rollbackSavedTokenRecords(ctx, savedPaths)
			return errSave
		}
	}
	return nil
}

func (h *Handler) rollbackSavedTokenRecords(ctx context.Context, savedPaths []string) {
	for i := len(savedPaths) - 1; i >= 0; i-- {
		path := strings.TrimSpace(savedPaths[i])
		if path == "" {
			continue
		}
		if errDelete := h.deleteTokenRecord(ctx, path); errDelete != nil {
			log.WithError(errDelete).WithField("path", path).Warn("failed to roll back plugin auth token")
		}
		h.removeAuthsForPath(ctx, path, path)
	}
}

// PopulateAuthContext extracts request info and adds it to the context
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	info := &coreauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	}
	return coreauth.WithRequestInfo(ctx, info)
}

func watchOAuthSessionCancel(pollCtx context.Context, cancel context.CancelFunc, state, provider string) {
	if cancel == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-pollCtx.Done():
			return
		case <-ticker.C:
			if !IsOAuthSessionPending(state, provider) {
				cancel()
				return
			}
		}
	}
}
