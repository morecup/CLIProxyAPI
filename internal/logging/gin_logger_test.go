package logging

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestIsAIAPIPathIncludesPublicAPIGroups(t *testing.T) {
	for _, path := range []string{
		"/v1",
		"/v1/models",
		"/v1/messages",
		"/v1/responses",
	} {
		if !isAIAPIPath(path) {
			t.Fatalf("expected %s to be treated as AI API path", path)
		}
	}
	for _, path := range []string{
		"/v0/management/config",
		"/v10/models",
	} {
		if isAIAPIPath(path) {
			t.Fatalf("expected %s not to be treated as AI API path", path)
		}
	}
}

func TestGinLogrusLoggerAddsRequestIDForAIPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	var requestIDFromContext string
	var requestIDFromGin string
	engine.POST("/v1/messages", func(c *gin.Context) {
		requestIDFromContext = GetRequestID(c.Request.Context())
		requestIDFromGin = GetGinRequestID(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if requestIDFromContext == "" {
		t.Fatalf("expected request ID in request context")
	}
	if requestIDFromGin != requestIDFromContext {
		t.Fatalf("expected Gin request ID %q to match context request ID %q", requestIDFromGin, requestIDFromContext)
	}
}
