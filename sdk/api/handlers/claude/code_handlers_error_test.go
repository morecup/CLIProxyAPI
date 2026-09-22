package claude

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func TestClaudeHandlersRejectInvalidOrMissingModelAtIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{name: "missing", body: `{"messages":[]}`, wantMessage: "model: Field required"},
		{name: "null", body: `{"model":null,"messages":[]}`, wantMessage: "model: Input should be a valid string"},
		{name: "number", body: `{"model":42,"messages":[]}`, wantMessage: "model: Input should be a valid string"},
		{name: "blank", body: `{"model":"  ","messages":[]}`, wantMessage: "model: String should have at least 1 character"},
		{name: "invalid json", body: `{`, wantMessage: "Invalid request: request body must be valid JSON"},
	}
	endpoints := []struct {
		name string
		path string
		run  func(*ClaudeCodeAPIHandler, *gin.Context)
	}{
		{name: "messages", path: "/v1/messages", run: func(h *ClaudeCodeAPIHandler, c *gin.Context) { h.ClaudeMessages(c) }},
		{name: "count_tokens", path: "/v1/messages/count_tokens", run: func(h *ClaudeCodeAPIHandler, c *gin.Context) { h.ClaudeCountTokens(c) }},
	}

	for _, endpoint := range endpoints {
		for _, test := range tests {
			t.Run(endpoint.name+"/"+test.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(test.body))
				handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})

				endpoint.run(handler, ctx)

				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
				}
				body := recorder.Body.Bytes()
				if got := gjson.GetBytes(body, "type").String(); got != "error" {
					t.Fatalf("type = %q, want error; body=%s", got, body)
				}
				if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
					t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
				}
				if got := gjson.GetBytes(body, "error.message").String(); got != test.wantMessage {
					t.Fatalf("error.message = %q, want %q; body=%s", got, test.wantMessage, body)
				}
			})
		}
	}
}

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := handlers.PendingStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}
