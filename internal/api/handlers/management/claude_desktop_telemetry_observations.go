package management

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (h *Handler) claudeDesktopTelemetryObservationDependencies(c *gin.Context) (cliproxyexecutor.ClaudeDesktopTelemetryObservationController, string, bool) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return nil, "", false
	}
	observations, ok := controller.(cliproxyexecutor.ClaudeDesktopTelemetryObservationController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop telemetry observations are unavailable"})
		return nil, "", false
	}
	return observations, authID, true
}

func decodeDesktopTelemetryObservation(c *gin.Context, value any, maximum int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maximum))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A single valid Desktop telemetry observation object is required"})
		return false
	}
	return true
}

func (h *Handler) ObserveClaudeDesktopRendererTelemetry(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopTelemetryObservationDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopRendererTelemetryObservation
	if !decodeDesktopTelemetryObservation(c, &request, 256<<10) {
		return
	}
	if err := controller.ObserveDesktopRendererTelemetry(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ObserveClaudeDesktopMainProcessTelemetry(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopTelemetryObservationDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopMainProcessTelemetryObservation
	if !decodeDesktopTelemetryObservation(c, &request, 256<<10) {
		return
	}
	if err := controller.ObserveDesktopMainProcessTelemetry(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ObserveClaudeDesktopSDKTelemetry(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopTelemetryObservationDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopSDKTelemetryObservation
	if !decodeDesktopTelemetryObservation(c, &request, 256<<10) {
		return
	}
	if err := controller.ObserveDesktopSDKTelemetry(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ObserveClaudeDesktopPerformanceTelemetry(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopTelemetryObservationDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopPerformanceTelemetryObservation
	if !decodeDesktopTelemetryObservation(c, &request, 256<<10) {
		return
	}
	if err := controller.ObserveDesktopPerformanceTelemetry(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ObserveClaudeDesktopCrashTelemetry(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopTelemetryObservationDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopCrashTelemetryObservation
	if !decodeDesktopTelemetryObservation(c, &request, 5<<20) {
		return
	}
	if err := controller.ObserveDesktopCrashTelemetry(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}
