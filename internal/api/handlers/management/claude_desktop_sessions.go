package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (h *Handler) GetClaudeDesktopSessions(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return
	}
	values, err := controller.ListDesktopSessions(authID)
	if err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": values})
}

func (h *Handler) CheckClaudeDesktopSessionHeartbeats(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return
	}
	heartbeats, ok := controller.(cliproxyexecutor.ClaudeDesktopSessionHeartbeatController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop session heartbeat checks are unavailable"})
		return
	}
	if err := heartbeats.CheckDesktopSessionHeartbeats(c.Request.Context(), authID); err != nil {
		claudeDesktopSessionError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ResumeClaudeDesktopSession(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return
	}
	resume, ok := controller.(cliproxyexecutor.ClaudeDesktopResumeController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop resume operations are unavailable"})
		return
	}
	var body struct {
		ExpectedGeneration string `json:"expected_generation"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected_generation is required in a single JSON object"})
		return
	}
	generation, err := uuid.Parse(body.ExpectedGeneration)
	if err != nil || generation == uuid.Nil || generation.String() != body.ExpectedGeneration {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected_generation must be the exact saved generation UUID"})
		return
	}
	value, err := resume.ResumeDesktopSession(c.Request.Context(), authID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: c.Param("session_id"), ExpectedGeneration: body.ExpectedGeneration})
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, gin.H{"session": value})
}

func (h *Handler) StopClaudeDesktopSession(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return
	}
	var body struct {
		ExpectedQueryID string `json:"expected_query_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || strings.TrimSpace(body.ExpectedQueryID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected_query_id is required in a single JSON object"})
		return
	}
	value, err := controller.StopDesktopSession(c.Request.Context(), authID, cliproxyexecutor.ClaudeDesktopSessionStop{
		SessionID: c.Param("session_id"), ExpectedQueryID: body.ExpectedQueryID,
	})
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, gin.H{"session": value})
}

func (h *Handler) StartClaudeDesktopRemoteSession(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopRemoteDependencies(c)
	if !ok {
		return
	}
	var body cliproxyexecutor.ClaudeDesktopRemoteStart
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF ||
		claudesessions.NormalizeRemoteSessionID(body.RemoteSessionID) == "" || strings.TrimSpace(body.Folder) == "" || strings.TrimSpace(body.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "remote_session_id, folder and model are required in a single JSON object"})
		return
	}
	value, err := controller.StartDesktopRemoteSession(c.Request.Context(), authID, body)
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, gin.H{"session": value})
}

func (h *Handler) AttachClaudeDesktopRemoteSession(c *gin.Context) {
	controller, authID, ok := h.claudeDesktopRemoteDependencies(c)
	if !ok {
		return
	}
	var body struct {
		ExpectedQueryID string `json:"expected_query_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || strings.TrimSpace(body.ExpectedQueryID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected_query_id is required in a single JSON object"})
		return
	}
	value, err := controller.AttachDesktopRemoteSession(c.Request.Context(), authID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: c.Param("session_id"), ExpectedQueryID: body.ExpectedQueryID})
	if err != nil {
		claudeDesktopSessionError(c, err, value)
		return
	}
	c.JSON(http.StatusOK, gin.H{"session": value})
}

func (h *Handler) claudeDesktopRemoteDependencies(c *gin.Context) (cliproxyexecutor.ClaudeDesktopRemoteController, string, bool) {
	controller, authID, ok := h.claudeDesktopSessionDependencies(c)
	if !ok {
		return nil, "", false
	}
	remote, ok := controller.(cliproxyexecutor.ClaudeDesktopRemoteController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop remote operations are unavailable"})
		return nil, "", false
	}
	return remote, authID, true
}

func (h *Handler) claudeDesktopSessionDependencies(c *gin.Context) (cliproxyexecutor.ClaudeDesktopSessionController, string, bool) {
	manager, _, ok := h.claudeDesktopRuntimeDependencies(c)
	if !ok {
		return nil, "", false
	}
	auth, ok := claudeDesktopRuntimeAuth(c, manager)
	if !ok {
		return nil, "", false
	}
	provider, _ := manager.Executor("claude")
	controller, ok := provider.(cliproxyexecutor.ClaudeDesktopSessionController)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Claude Desktop session operations are unavailable"})
		return nil, "", false
	}
	return controller, auth.ID, true
}

func claudeDesktopSessionError(c *gin.Context, err error, value any) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, claudesessions.ErrInvalid), errors.Is(err, claudetelemetry.ErrInvalidObservation):
		status = http.StatusBadRequest
	case errors.Is(err, claudesessions.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, claudesessions.ErrStaleQuery), errors.Is(err, claudesessions.ErrRemoteBound), errors.Is(err, claudesessions.ErrRemoteDetached), errors.Is(err, claudesessions.ErrRemoteMismatch), errors.Is(err, claudeprompt.ErrSDKSessionStale), errors.Is(err, claudeprompt.ErrSDKSessionActive), errors.Is(err, claudeprompt.ErrSDKResumeReconstructionRequired):
		status = http.StatusConflict
	case errors.Is(err, claudesessions.ErrUnavailable), errors.Is(err, claudesessions.ErrResumeUnverified), errors.Is(err, claudeprompt.ErrSDKSessionUnavailable), errors.Is(err, claudeprompt.ErrSDKSessionInvalid):
		status = http.StatusServiceUnavailable
	}
	var upstream interface{ StatusCode() int }
	if status == http.StatusInternalServerError && errors.As(err, &upstream) && upstream.StatusCode() >= 400 && upstream.StatusCode() <= 599 {
		status = upstream.StatusCode()
	}
	c.JSON(status, gin.H{"error": err.Error(), "session": value})
}
