package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPatchClaudeKeyRejectsFingerprintProfile(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "test-claude-key"},
		},
	}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":0,"value":{"fingerprint-profile":"claude-code-cli"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "removed Claude Code setting") {
		t.Fatalf("error body = %s, want migration error", rec.Body.String())
	}
	if got := cfg.ClaudeKey[0].APIKey; got != "test-claude-key" {
		t.Fatalf("APIKey = %q, want rejected patch to leave entry unchanged", got)
	}
}

// A typo must fail the write instead of reaching the request path, where it can
// only be reported as a warning behind every later request.
func TestPatchClaudeKeyRejectsUnknownFingerprintProfile(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "test-claude-key"},
		},
	}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":0,"value":{"fingerprint-profile":"claude-code"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fingerprint-profile") {
		t.Fatalf("error body = %s, want it to name the field", rec.Body.String())
	}
	if got := cfg.ClaudeKey[0].APIKey; got != "test-claude-key" {
		t.Fatalf("APIKey = %q, want rejected patch to leave entry unchanged", got)
	}
}

func TestPutClaudeKeysRejectsUnknownFingerprintProfile(t *testing.T) {
	cfg := &config.Config{}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/claude-api-key",
		strings.NewReader(`[{"api-key":"k1"},{"api-key":"k2","fingerprint-profile":"claude-cli"}]`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutClaudeKeys(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude-api-key[1].fingerprint-profile") {
		t.Fatalf("error body = %s, want the offending index", rec.Body.String())
	}
	if len(cfg.ClaudeKey) != 0 {
		t.Fatalf("ClaudeKey = %+v, want the rejected write to change nothing", cfg.ClaudeKey)
	}
}
