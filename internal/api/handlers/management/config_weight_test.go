package management

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPatchClaudeKeyWeight(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key"}}}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key", strings.NewReader(`{"index":0,"value":{"weight":7}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if weight := cfg.ClaudeKey[0].Weight; weight == nil || *weight != 7 {
		t.Fatalf("weight = %v, want 7", weight)
	}
}

func TestPatchAPIKeyWeightResetAndStrictValidation(t *testing.T) {
	initial := 5
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key", Weight: &initial}}}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	patch := func(raw string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		body := fmt.Sprintf(`{"index":0,"value":{"weight":%s}}`, raw)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchClaudeKey(ctx)
		return rec
	}

	for _, invalid := range []string{"1.5", "1000001", "9223372036854775808", `"7"`} {
		rec := patch(invalid)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("weight %s status = %d, want 400; body=%s", invalid, rec.Code, rec.Body.String())
		}
		if cfg.ClaudeKey[0].Weight == nil || *cfg.ClaudeKey[0].Weight != initial {
			t.Fatalf("invalid weight %s changed config", invalid)
		}
	}

	if rec := patch("null"); rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg.ClaudeKey[0].Weight != nil {
		t.Fatalf("reset weight = %v, want nil default", cfg.ClaudeKey[0].Weight)
	}
}

func TestPutAPIKeyWeightRejectsAboveMaximum(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/claude-api-key", strings.NewReader(`[{"api-key":"key","weight":1000001}]`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutClaudeKeys(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ClaudeKey) != 0 {
		t.Fatal("invalid PUT changed config")
	}
}
