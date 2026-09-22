package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) GetClaudeDesktopTelemetry(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"telemetry": claudetelemetry.StatusSnapshots()})
}

func (h *Handler) FlushClaudeDesktopTelemetry(c *gin.Context) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if errFlush := claudetelemetry.FlushAll(ctx); errFlush != nil {
		log.Warn("claude desktop telemetry: management flush failed")
		c.JSON(http.StatusBadGateway, gin.H{
			"status":    "error",
			"error":     "claude desktop telemetry flush failed",
			"telemetry": claudetelemetry.StatusSnapshots(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"telemetry": claudetelemetry.StatusSnapshots(),
	})
}

func (h *Handler) RetryClaudeDesktopTelemetryDeadLetters(c *gin.Context) {
	count, errRetry := claudetelemetry.RetryDeadLettersAll()
	if errRetry != nil {
		log.Warn("claude desktop telemetry: management dead-letter retry failed")
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":    "error",
			"error":     "claude desktop telemetry dead-letter retry failed",
			"retried":   count,
			"telemetry": claudetelemetry.StatusSnapshots(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"retried":   count,
		"telemetry": claudetelemetry.StatusSnapshots(),
	})
}
