package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/claude/models"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
)

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	healthzHandler := func(c *gin.Context) {
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	s.engine.GET("/healthz", healthzHandler)
	s.engine.HEAD("/healthz", healthzHandler)

	s.engine.GET("/management.html", s.serveManagementControlPanel)
	openaiHandlers := openai.NewOpenAIAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(s.handlers)

	// OpenAI compatible API routes
	v1 := s.engine.Group("/v1")
	v1.Use(AuthMiddleware(s.accessManager))
	{
		v1.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		v1.POST("/chat/completions", openaiHandlers.ChatCompletions)
		v1.POST("/completions", openaiHandlers.Completions)
		v1.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		v1.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		v1.POST("/responses", openaiResponsesHandlers.Responses)
	}

	// Root endpoint
	s.engine.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "CLI Proxy API Server",
			"endpoints": []string{
				"POST /v1/chat/completions",
				"POST /v1/completions",
				"GET /v1/models",
			},
		})
	})

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// isAnthropicModelsRequest reports whether a /v1/models request should be served in
// Anthropic format. Anthropic API clients send the Anthropic-Version header; Claude
// Code additionally uses a claude-cli User-Agent.
func isAnthropicModelsRequest(c *gin.Context) bool {
	if c.GetHeader("Anthropic-Version") != "" {
		return true
	}
	return strings.HasPrefix(c.GetHeader("User-Agent"), "claude-cli")
}

// unifiedModelsHandler creates a unified handler for the /v1/models endpoint
// that routes to different handlers based on the request.
// Anthropic API requests (Anthropic-Version header, or a claude-cli User-Agent)
// route to the Claude handler, otherwise they route to the OpenAI handler.
func (s *Server) unifiedModelsHandler(openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeModels(c)
			return
		}

		// Route to Claude handler for Anthropic API requests.
		if isAnthropicModelsRequest(c) {
			claudeHandler.ClaudeModels(c)
		} else {
			openaiHandler.OpenAIModels(c)
		}
	}
}

type homeModelEntry struct {
	id                  string
	created             int64
	ownedBy             string
	displayName         string
	contextLength       int
	maxCompletionTokens int
}

func (s *Server) handleHomeModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	isClaude := isAnthropicModelsRequest(c)

	if isClaude {
		disableCloaking := s.cfg != nil && s.cfg.ClaudeCode.DisableCloakingModelList
		c.JSON(http.StatusOK, claudemodels.BuildResponse(formatHomeClaudeModels(entries), disableCloaking))
		return
	}

	filtered := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{
			"id":     entry.id,
			"object": "model",
		}
		if entry.created > 0 {
			model["created"] = entry.created
		}
		if entry.ownedBy != "" {
			model["owned_by"] = entry.ownedBy
		}
		filtered = append(filtered, model)
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   filtered,
	})
}

func formatHomeClaudeModels(entries []homeModelEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatHomeClaudeModel(entry))
	}
	return out
}

func formatHomeClaudeModel(entry homeModelEntry) map[string]any {
	displayName := entry.displayName
	if displayName == "" {
		displayName = entry.id
	}
	maxInput := entry.contextLength
	if maxInput <= 0 {
		maxInput = registry.DefaultClaudeMaxInputTokens
	}
	maxOutput := entry.maxCompletionTokens
	if maxOutput <= 0 {
		maxOutput = registry.DefaultClaudeMaxOutputTokens
	}
	model := map[string]any{
		"id":               entry.id,
		"object":           "model",
		"owned_by":         entry.ownedBy,
		"type":             "model",
		"display_name":     displayName,
		"max_input_tokens": maxInput,
		"max_tokens":       maxOutput,
	}
	if entry.created > 0 {
		model["created_at"] = time.Unix(entry.created, 0).UTC().Format(time.RFC3339)
	}
	return model
}

func (s *Server) loadHomeModelEntries(c *gin.Context) ([]homeModelEntry, bool) {
	if s == nil || c == nil || c.Request == nil {
		return nil, false
	}
	client := home.Current()
	if client == nil {
		c.JSON(http.StatusServiceUnavailable, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "home control center unavailable",
				Type:    "server_error",
			},
		})
		return nil, false
	}

	raw, errGet := client.GetModels(c.Request.Context(), c.Request.Header, c.Request.URL.Query())
	if errGet != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errGet.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	if statusCode, ok := homeModelsAuthStatus(raw); ok {
		c.JSON(statusCode, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: homeModelsErrorMessage(raw),
				Type:    "authentication_error",
			},
		})
		return nil, false
	}

	entries, errDecode := decodeHomeModels(raw)
	if errDecode != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errDecode.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	return entries, true
}

// homeModelsAuthStatus inspects a home models response for an authentication/error envelope.
// It returns the HTTP status code to surface (401 for credential issues, 502 otherwise)
// and true when the payload is an error response rather than model data.
func homeModelsAuthStatus(raw []byte) (int, bool) {
	errType := homeModelsErrorType(raw)
	if errType == "" {
		return 0, false
	}
	if errType == "no_credentials" || errType == "invalid_credential" {
		return http.StatusUnauthorized, true
	}
	return http.StatusBadGateway, true
}

func homeModelsErrorType(raw []byte) string {
	top, ok := unmarshalHomeModelsTopLevel(raw)
	if !ok {
		return ""
	}
	rawErr, exists := top["error"]
	if !exists {
		return ""
	}
	var errObj struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(rawErr, &errObj); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(errObj.Type)
}

func homeModelsErrorMessage(raw []byte) string {
	top, ok := unmarshalHomeModelsTopLevel(raw)
	if !ok {
		return "home models request failed"
	}
	rawErr, exists := top["error"]
	if !exists {
		return "home models request failed"
	}
	var errObj struct {
		Message string `json:"message"`
	}
	if errUnmarshal := json.Unmarshal(rawErr, &errObj); errUnmarshal != nil {
		return "home models request failed"
	}
	if msg := strings.TrimSpace(errObj.Message); msg != "" {
		return msg
	}
	return "home models request failed"
}

func unmarshalHomeModelsTopLevel(raw []byte) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return nil, false
	}
	return top, true
}

func decodeHomeModels(raw []byte) ([]homeModelEntry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("home models payload is empty")
	}

	var bySection map[string][]map[string]any
	if err := json.Unmarshal(raw, &bySection); err != nil {
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	if len(bySection) == 0 {
		return nil, fmt.Errorf("home models payload has no sections")
	}

	seen := make(map[string]struct{})
	out := make([]homeModelEntry, 0, 256)
	for _, models := range bySection {
		for _, model := range models {
			id, _ := model["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				name, _ := model["name"].(string)
				name = strings.TrimSpace(name)
				id = strings.TrimPrefix(name, "models/")
			}
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}

			ownedBy, _ := model["owned_by"].(string)
			ownedBy = strings.TrimSpace(ownedBy)
			displayName, _ := model["display_name"].(string)
			displayName = strings.TrimSpace(displayName)
			if displayName == "" {
				displayName, _ = model["displayName"].(string)
				displayName = strings.TrimSpace(displayName)
			}

			out = append(out, homeModelEntry{
				id:                  id,
				created:             homeModelInt64Value(model, "created"),
				ownedBy:             ownedBy,
				displayName:         displayName,
				contextLength:       int(homeModelInt64Value(model, "context_length", "contextLength", "inputTokenLimit", "max_input_tokens")),
				maxCompletionTokens: int(homeModelInt64Value(model, "max_completion_tokens", "maxCompletionTokens", "outputTokenLimit", "max_tokens")),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	if len(out) == 0 {
		return nil, fmt.Errorf("home models payload contains no models")
	}
	return out, nil
}

func homeModelInt64Value(model map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := model[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		case json.Number:
			if n, errInt := value.Int64(); errInt == nil {
				return n
			}
		case string:
			if n, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64); errParse == nil {
				return n
			}
		}
	}
	return 0
}
