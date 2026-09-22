package management

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (h *Handler) localDesktopDependencies(c *gin.Context) (cliproxyexecutor.ClaudeDesktopLocalController, string, bool) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return nil, "", false
	}
	local, ok := controller.(cliproxyexecutor.ClaudeDesktopLocalController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Local Claude Desktop sessions are unavailable"})
		return nil, "", false
	}
	return local, authID, true
}

func decodeLocalDesktopRequest(c *gin.Context, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 131072))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A single valid session request object is required"})
		return false
	}
	return true
}

func (h *Handler) StartClaudeDesktopLocalSession(c *gin.Context) {
	controller, authID, ok := h.localDesktopDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopLocalStart
	if !decodeLocalDesktopRequest(c, &request) {
		return
	}
	value, err := controller.StartDesktopLocalSession(c.Request.Context(), authID, request)
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusCreated, value)
}

func (h *Handler) GetClaudeDesktopLocalSession(c *gin.Context) {
	controller, authID, ok := h.localDesktopDependencies(c)
	if !ok {
		return
	}
	value, err := controller.GetDesktopLocalSession(authID, c.Param("session_id"))
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *Handler) SendClaudeDesktopLocalMessage(c *gin.Context) {
	controller, authID, ok := h.localDesktopDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopLocalInput
	if !decodeLocalDesktopRequest(c, &request) {
		return
	}
	value, err := controller.SendDesktopLocalMessage(c.Request.Context(), authID, c.Param("session_id"), request)
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *Handler) ResumeClaudeDesktopLocalSession(c *gin.Context) {
	controller, authID, ok := h.localDesktopDependencies(c)
	if !ok {
		return
	}
	var request struct {
		ExpectedGeneration string `json:"expected_generation"`
	}
	if !decodeLocalDesktopRequest(c, &request) {
		return
	}
	value, err := controller.ResumeDesktopLocalSession(c.Request.Context(), authID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: c.Param("session_id"), ExpectedGeneration: request.ExpectedGeneration})
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *Handler) ObserveClaudeDesktopSessionUI(c *gin.Context) {
	controller, authID, ok := h.localDesktopDependencies(c)
	if !ok {
		return
	}
	var request cliproxyexecutor.ClaudeDesktopUIObservation
	if !decodeLocalDesktopRequest(c, &request) {
		return
	}
	if err := controller.ObserveDesktopSessionUI(c.Request.Context(), authID, request); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}
