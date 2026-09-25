package management

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (h *Handler) appendAuthFileHealth(entry gin.H, auth *coreauth.Auth) {
	if h.authManager == nil {
		return
	}
	if health := h.authManager.CredentialHealthSnapshot(auth.ID); health != nil {
		entry["credential_health"] = health
	}
	if !claudedesktop.IsDesktopMetadata(auth.Metadata) {
		return
	}
	executor, ok := h.authManager.Executor(auth.Provider)
	if !ok {
		return
	}
	if controller, okController := executor.(interface {
		AccountStatus(string) runtimeexecutor.ClaudeAccountRuntimeStatus
	}); okController {
		status := controller.AccountStatus(auth.ID)
		entry["runtime_state"] = status.State
		if status.QuarantineReason != "" {
			entry["runtime_message"] = status.QuarantineReason
		}
	}
}

// ImportAuthFileHealthObservation attaches independently verified historical
// evidence to the unchanged credential file. It makes no provider request and
// never changes enablement, cooldowns, runtime approvals, or usage counters.
func (h *Handler) ImportAuthFileHealthObservation(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager is unavailable"})
		return
	}
	var input struct {
		AuthID             string         `json:"auth_id"`
		ExpectedFileSHA256 string         `json:"expected_file_sha256"`
		Error              coreauth.Error `json:"error"`
		FirstObservedAt    time.Time      `json:"first_observed_at"`
		LastObservedAt     time.Time      `json:"last_observed_at"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid health observation"})
		return
	}
	auth, ok := h.authManager.GetByID(strings.TrimSpace(input.AuthID))
	if !ok || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	path := strings.TrimSpace(auth.Attributes["path"])
	file, errOpen := os.Open(path)
	if errOpen != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "credential file is unavailable"})
		return
	}
	digest := sha256.New()
	_, errRead := io.Copy(digest, file)
	errClose := file.Close()
	if errRead != nil || errClose != nil || len(input.ExpectedFileSHA256) != 64 || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), input.ExpectedFileSHA256) {
		c.JSON(http.StatusConflict, gin.H{"error": "credential file changed; historical evidence must be verified again"})
		return
	}
	if errImport := h.authManager.ImportCredentialHealth(auth.ID, coreauth.CredentialHealthFingerprint(auth), &input.Error, input.FirstObservedAt, input.LastObservedAt); errImport != nil {
		c.JSON(http.StatusConflict, gin.H{"error": errImport.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "credential_health": h.authManager.CredentialHealthSnapshot(auth.ID)})
}
