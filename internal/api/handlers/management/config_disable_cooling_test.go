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

func TestPatchDisableCoolingOverrideForClaudeKey(t *testing.T) {
	initial := true
	cfg := &config.Config{}
	cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "key", DisableCooling: &initial}}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	patch := func(value string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		body := fmt.Sprintf(`{"index":0,"value":{"disable-cooling":%s}}`, value)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchClaudeKey(ctx)
		return rec
	}

	if rec := patch("false"); rec.Code != http.StatusOK {
		t.Fatalf("false patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if override := cfg.ClaudeKey[0].DisableCooling; override == nil || *override {
		t.Fatalf("disable-cooling = %v, want explicit false", override)
	}

	if rec := patch("null"); rec.Code != http.StatusOK {
		t.Fatalf("null patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if override := cfg.ClaudeKey[0].DisableCooling; override != nil {
		t.Fatalf("disable-cooling = %v, want inherited value", override)
	}
}
