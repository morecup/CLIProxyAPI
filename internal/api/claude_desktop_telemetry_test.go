package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaudeDesktopTelemetryManagementRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)

	unauthorized := httptest.NewRequest(http.MethodGet, "/v0/management/claude-desktop/telemetry", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorizedRecorder.Code, http.StatusUnauthorized)
	}

	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v0/management/claude-desktop/telemetry"},
		{method: http.MethodPost, path: "/v0/management/claude-desktop/telemetry/flush"},
		{method: http.MethodPost, path: "/v0/management/claude-desktop/telemetry/retry-dead"},
	} {
		request := httptest.NewRequest(endpoint.method, endpoint.path, nil)
		request.Header.Set("Authorization", "Bearer test-management-key")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want %d body=%s", endpoint.method, endpoint.path, recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var payload map[string]any
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
			t.Fatalf("%s %s response is not JSON: %v", endpoint.method, endpoint.path, errDecode)
		}
		if _, exists := payload["telemetry"]; !exists {
			t.Fatalf("%s %s response omits telemetry: %s", endpoint.method, endpoint.path, recorder.Body.String())
		}
		if containsJSONKey(payload, "state_path") {
			t.Fatalf("%s %s response exposes state_path: %s", endpoint.method, endpoint.path, recorder.Body.String())
		}
	}
}

func containsJSONKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for candidate, nested := range typed {
			if candidate == key || containsJSONKey(nested, key) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsJSONKey(nested, key) {
				return true
			}
		}
	}
	return false
}
