package api

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	log "github.com/sirupsen/logrus"
)

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	s.engine.POST("/v0/management/oauth-callback", s.managementAvailabilityMiddleware(), s.mgmt.PostOAuthCallback)
	s.engine.GET("/v0/management/oauth-callback", s.managementAvailabilityMiddleware(), s.mgmt.GetOAuthCallback)
	s.engine.GET("/v0/management/oauth-session/:state/browser", s.managementAvailabilityMiddleware(), s.mgmt.StreamOAuthBrowser)

	mgmt := s.engine.Group("/v0/management")
	mgmt.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware())
	{
		mgmt.GET("/config", s.mgmt.GetConfig)
		mgmt.GET("/config.yaml", s.mgmt.GetConfigYAML)
		mgmt.PUT("/config.yaml", s.mgmt.PutConfigYAML)
		mgmt.GET("/latest-version", s.mgmt.GetLatestVersion)
		mgmt.GET("/plugins", s.mgmt.ListPlugins)
		mgmt.GET("/plugin-store", s.mgmt.ListPluginStore)
		mgmt.POST("/plugin-store/:id/install", s.mgmt.InstallPluginFromStore)
		mgmt.DELETE("/plugins/:id", s.mgmt.DeletePlugin)
		mgmt.PATCH("/plugins/:id/enabled", s.mgmt.PatchPluginEnabled)
		mgmt.GET("/plugins/:id/config", s.mgmt.GetPluginConfig)
		mgmt.PUT("/plugins/:id/config", s.mgmt.PutPluginConfig)
		mgmt.PATCH("/plugins/:id/config", s.mgmt.PatchPluginConfig)

		mgmt.GET("/debug", s.mgmt.GetDebug)
		mgmt.PUT("/debug", s.mgmt.PutDebug)
		mgmt.PATCH("/debug", s.mgmt.PutDebug)

		mgmt.GET("/logging-to-file", s.mgmt.GetLoggingToFile)
		mgmt.PUT("/logging-to-file", s.mgmt.PutLoggingToFile)
		mgmt.PATCH("/logging-to-file", s.mgmt.PutLoggingToFile)

		mgmt.GET("/logs-max-total-size-mb", s.mgmt.GetLogsMaxTotalSizeMB)
		mgmt.PUT("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)
		mgmt.PATCH("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)

		mgmt.GET("/error-logs-max-files", s.mgmt.GetErrorLogsMaxFiles)
		mgmt.PUT("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)
		mgmt.PATCH("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)

		mgmt.GET("/usage-statistics-enabled", s.mgmt.GetUsageStatisticsEnabled)
		mgmt.PUT("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.PATCH("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)

		mgmt.GET("/proxy-url", s.mgmt.GetProxyURL)
		mgmt.PUT("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.PATCH("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.DELETE("/proxy-url", s.mgmt.DeleteProxyURL)

		mgmt.POST("/api-call", s.mgmt.APICall)

		mgmt.POST("/reset-quota", s.mgmt.ResetQuota)

		mgmt.GET("/api-keys", s.mgmt.GetAPIKeys)
		mgmt.PUT("/api-keys", s.mgmt.PutAPIKeys)
		mgmt.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
		mgmt.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)
		mgmt.GET("/api-key-usage", s.mgmt.GetAPIKeyUsage)
		mgmt.GET("/usage-queue", s.mgmt.GetUsageQueue)
		mgmt.GET("/claude-desktop/telemetry", s.mgmt.GetClaudeDesktopTelemetry)
		mgmt.POST("/claude-desktop/telemetry/flush", s.mgmt.FlushClaudeDesktopTelemetry)
		mgmt.POST("/claude-desktop/telemetry/retry-dead", s.mgmt.RetryClaudeDesktopTelemetryDeadLetters)
		mgmt.GET("/claude-desktop/runtimes", s.mgmt.GetClaudeDesktopRuntimes)
		mgmt.GET("/claude-desktop/runtimes/:auth_id", s.mgmt.GetClaudeDesktopRuntime)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/promote", s.mgmt.PromoteClaudeDesktopRuntime)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/rollback", s.mgmt.RollbackClaudeDesktopRuntime)
		mgmt.GET("/claude-desktop/runtimes/:auth_id/sessions", s.mgmt.GetClaudeDesktopSessions)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/heartbeat-check", s.mgmt.CheckClaudeDesktopSessionHeartbeats)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/local", s.mgmt.StartClaudeDesktopLocalSession)
		mgmt.GET("/claude-desktop/runtimes/:auth_id/sessions/:session_id/local", s.mgmt.GetClaudeDesktopLocalSession)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/:session_id/messages", s.mgmt.SendClaudeDesktopLocalMessage)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/:session_id/local-resume", s.mgmt.ResumeClaudeDesktopLocalSession)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/ui-observations", s.mgmt.ObserveClaudeDesktopSessionUI)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/telemetry/renderer", s.mgmt.ObserveClaudeDesktopRendererTelemetry)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/telemetry/main-process", s.mgmt.ObserveClaudeDesktopMainProcessTelemetry)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/telemetry/sdk", s.mgmt.ObserveClaudeDesktopSDKTelemetry)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/telemetry/performance", s.mgmt.ObserveClaudeDesktopPerformanceTelemetry)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/telemetry/crash", s.mgmt.ObserveClaudeDesktopCrashTelemetry)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/remote", s.mgmt.StartClaudeDesktopRemoteSession)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/:session_id/attach", s.mgmt.AttachClaudeDesktopRemoteSession)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/:session_id/resume", s.mgmt.ResumeClaudeDesktopSession)
		mgmt.POST("/claude-desktop/runtimes/:auth_id/sessions/:session_id/stop", s.mgmt.StopClaudeDesktopSession)

		mgmt.GET("/logs", s.mgmt.GetLogs)
		mgmt.DELETE("/logs", s.mgmt.DeleteLogs)
		mgmt.GET("/request-error-logs", s.mgmt.GetRequestErrorLogs)
		mgmt.GET("/request-error-logs/:name", s.mgmt.DownloadRequestErrorLog)
		mgmt.GET("/request-log-by-id/:id", s.mgmt.GetRequestLogByID)
		mgmt.GET("/request-log", s.mgmt.GetRequestLog)
		mgmt.PUT("/request-log", s.mgmt.PutRequestLog)
		mgmt.PATCH("/request-log", s.mgmt.PutRequestLog)
		mgmt.GET("/request-retry", s.mgmt.GetRequestRetry)
		mgmt.PUT("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.PATCH("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.GET("/max-retry-credentials", s.mgmt.GetMaxRetryCredentials)
		mgmt.PUT("/max-retry-credentials", s.mgmt.PutMaxRetryCredentials)
		mgmt.PATCH("/max-retry-credentials", s.mgmt.PutMaxRetryCredentials)
		mgmt.GET("/max-retry-interval", s.mgmt.GetMaxRetryInterval)
		mgmt.PUT("/max-retry-interval", s.mgmt.PutMaxRetryInterval)
		mgmt.PATCH("/max-retry-interval", s.mgmt.PutMaxRetryInterval)

		mgmt.GET("/force-model-prefix", s.mgmt.GetForceModelPrefix)
		mgmt.PUT("/force-model-prefix", s.mgmt.PutForceModelPrefix)
		mgmt.PATCH("/force-model-prefix", s.mgmt.PutForceModelPrefix)

		mgmt.GET("/routing/strategy", s.mgmt.GetRoutingStrategy)
		mgmt.PUT("/routing/strategy", s.mgmt.PutRoutingStrategy)
		mgmt.PATCH("/routing/strategy", s.mgmt.PutRoutingStrategy)

		mgmt.GET("/claude-api-key", s.mgmt.GetClaudeKeys)
		mgmt.PUT("/claude-api-key", s.mgmt.PutClaudeKeys)
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)

		mgmt.GET("/oauth-request-scoped-errors", s.mgmt.GetOAuthRequestScopedErrors)
		mgmt.PUT("/oauth-request-scoped-errors", s.mgmt.PutOAuthRequestScopedErrors)
		mgmt.PATCH("/oauth-request-scoped-errors", s.mgmt.PatchOAuthRequestScopedErrors)
		mgmt.DELETE("/oauth-request-scoped-errors", s.mgmt.DeleteOAuthRequestScopedErrors)

		mgmt.GET("/auth-files", s.mgmt.ListAuthFiles)
		mgmt.GET("/auth-files/models", s.mgmt.GetAuthFileModels)
		mgmt.GET("/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)
		mgmt.GET("/auth-files/download", s.mgmt.DownloadAuthFile)
		mgmt.POST("/auth-files", s.mgmt.UploadAuthFile)
		mgmt.DELETE("/auth-files", s.mgmt.DeleteAuthFile)
		mgmt.PATCH("/auth-files/status", s.mgmt.PatchAuthFileStatus)
		mgmt.POST("/auth-files/health-observations", s.mgmt.ImportAuthFileHealthObservation)
		mgmt.PATCH("/auth-files/fields", s.mgmt.PatchAuthFileFields)

		mgmt.GET("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.POST("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.POST("/claude-desktop/session-key", s.mgmt.ImportClaudeSession)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
		mgmt.POST("/oauth-session/:state/browser-ticket", s.mgmt.CreateOAuthBrowserTicket)
		mgmt.DELETE("/oauth-session", s.mgmt.CancelAuthSession)
	}
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.managementAvailable(c) {
			return
		}
		c.Next()
	}
}

func (s *Server) managementAvailable(c *gin.Context) bool {
	if s == nil || s.cfg == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	if s.cfg.Home.Enabled {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	if !s.managementRoutesEnabled.Load() {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) refreshPluginManagementRoutes() {
	if s == nil || s.pluginHost == nil || s.engine == nil {
		return
	}
	s.pluginHost.RegisterManagementRoutes(context.Background(), s.registeredManagementRouteKeys())
}

// RefreshPluginManagementRoutes rebuilds plugin-owned Management API routes.
func (s *Server) RefreshPluginManagementRoutes() {
	s.refreshPluginManagementRoutes()
}

func (s *Server) registeredManagementRouteKeys() map[string]struct{} {
	out := make(map[string]struct{})
	if s == nil || s.engine == nil {
		return out
	}
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/v0/management/") || route.Path == "/v0/management" {
			out[strings.ToUpper(strings.TrimSpace(route.Method))+" "+route.Path] = struct{}{}
		}
	}
	return out
}

func (s *Server) pluginManagementNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/v0/resource/plugins/") {
		s.pluginResourceNoRoute(c)
		return
	}
	if path != "/v0/management" && !strings.HasPrefix(path, "/v0/management/") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s.pluginHost == nil || s.mgmt == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !s.managementAvailable(c) {
		return
	}
	s.mgmt.Middleware()(c)
	if c.IsAborted() {
		return
	}
	if s.mgmt.ServePluginAuthURL(c) {
		c.Abort()
		return
	}
	if s.pluginHost.ServeManagementHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) pluginResourceNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	if s.cfg == nil || s.cfg.Home.Enabled || s.pluginHost == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s.pluginHost.ServeResourceHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	filePath := managementasset.FilePath(s.configFilePath)
	if strings.TrimSpace(filePath) == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			// Synchronously ensure management.html is available with a detached context.
			// Control panel bootstrap should not be canceled by client disconnects.
			if !managementasset.EnsureLatestManagementHTML(context.Background(), managementasset.StaticDir(s.configFilePath), cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
		} else {
			log.WithError(err).Error("failed to stat management control panel asset")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}

	c.File(filePath)
}
