package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type claudeDesktopRuntimeController interface {
	AccountStatus(authID string) runtimeexecutor.ClaudeAccountRuntimeStatus
	PromoteAuth(auth *coreauth.Auth) error
	RollbackAuth(auth *coreauth.Auth) error
}

type claudeDesktopRuntimeResponse struct {
	AuthID    string                                     `json:"auth_id"`
	AuthIndex string                                     `json:"auth_index,omitempty"`
	Label     string                                     `json:"label,omitempty"`
	Disabled  bool                                       `json:"disabled"`
	Runtime   runtimeexecutor.ClaudeAccountRuntimeStatus `json:"runtime"`
}

func (h *Handler) GetClaudeDesktopRuntimes(c *gin.Context) {
	manager, controller, ok := h.claudeDesktopRuntimeDependencies(c)
	if !ok {
		return
	}
	auths := manager.List()
	result := make([]claudeDesktopRuntimeResponse, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), claudedesktop.Provider) {
			continue
		}
		result = append(result, claudeDesktopRuntimeResponse{
			AuthID: auth.ID, AuthIndex: auth.Index, Label: auth.Label, Disabled: auth.Disabled,
			Runtime: controller.AccountStatus(auth.ID),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AuthID < result[j].AuthID })
	c.JSON(http.StatusOK, gin.H{"runtimes": result})
}

func (h *Handler) GetClaudeDesktopRuntime(c *gin.Context) {
	manager, controller, ok := h.claudeDesktopRuntimeDependencies(c)
	if !ok {
		return
	}
	auth, ok := claudeDesktopRuntimeAuth(c, manager)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, claudeDesktopRuntimeResponse{
		AuthID: auth.ID, AuthIndex: auth.Index, Label: auth.Label, Disabled: auth.Disabled,
		Runtime: controller.AccountStatus(auth.ID),
	})
}

func (h *Handler) PromoteClaudeDesktopRuntime(c *gin.Context) {
	h.mutateClaudeDesktopRuntime(c, "promote")
}

func (h *Handler) RollbackClaudeDesktopRuntime(c *gin.Context) {
	h.mutateClaudeDesktopRuntime(c, "rollback")
}

func (h *Handler) mutateClaudeDesktopRuntime(c *gin.Context, operation string) {
	manager, controller, ok := h.claudeDesktopRuntimeDependencies(c)
	if !ok {
		return
	}
	auth, ok := claudeDesktopRuntimeAuth(c, manager)
	if !ok {
		return
	}
	var errOperation error
	switch operation {
	case "promote":
		errOperation = controller.PromoteAuth(auth)
	case "rollback":
		errOperation = controller.RollbackAuth(auth)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unsupported Claude Desktop runtime operation"})
		return
	}
	status := controller.AccountStatus(auth.ID)
	if errOperation != nil {
		c.JSON(http.StatusConflict, gin.H{
			"status": "error", "operation": operation, "error": errOperation.Error(), "runtime": status,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "operation": operation, "runtime": status})
}

func (h *Handler) claudeDesktopRuntimeDependencies(c *gin.Context) (*coreauth.Manager, claudeDesktopRuntimeController, bool) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "management handler is unavailable"})
		return nil, nil, false
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager is unavailable"})
		return nil, nil, false
	}
	providerExecutor, exists := manager.Executor(claudedesktop.Provider)
	if !exists {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop runtime executor is unavailable"})
		return nil, nil, false
	}
	controller, ok := providerExecutor.(claudeDesktopRuntimeController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude provider does not expose Desktop runtime lifecycle controls"})
		return nil, nil, false
	}
	return manager, controller, true
}

func claudeDesktopRuntimeAuth(c *gin.Context, manager *coreauth.Manager) (*coreauth.Auth, bool) {
	authID := strings.TrimSpace(c.Param("auth_id"))
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return nil, false
	}
	auth, exists := manager.GetByID(authID)
	if !exists || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Claude Desktop auth was not found"})
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), claudedesktop.Provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth is not a Claude Desktop credential"})
		return nil, false
	}
	return auth, true
}
