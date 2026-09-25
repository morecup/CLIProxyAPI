package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type healthRuntimeExecutor struct{ coreauth.ProviderExecutor }

func (healthRuntimeExecutor) Identifier() string { return "claude" }
func (healthRuntimeExecutor) AccountStatus(string) runtimeexecutor.ClaudeAccountRuntimeStatus {
	return runtimeexecutor.ClaudeAccountRuntimeStatus{State: "quarantined", QuarantineReason: "binding drift"}
}

func TestAuthFileHealthHistoricalImportAndList(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := newManagementClaudeDesktopAuth(t)
	auth.Metadata["access_token"] = "synthetic-test-secret"
	auth.FileName = auth.ID
	path := filepath.Join(dir, auth.ID)
	auth.Attributes = map[string]string{"path": path}
	data := []byte(`{"type":"claude","access_token":"synthetic-test-secret"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	manager.RegisterExecutor(healthRuntimeExecutor{})
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	digest := sha256.Sum256(data)
	first := time.Now().Add(-time.Hour).UTC()
	body := map[string]any{
		"auth_id": auth.ID, "expected_file_sha256": hex.EncodeToString(digest[:]),
		"first_observed_at": first, "last_observed_at": first.Add(time.Minute),
		"error": map[string]any{"http_status": 401, "message": "OAuth access token has been revoked."},
	}
	callImport := func() *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/health-observations", bytes.NewReader(payload))
		h.ImportAuthFileHealthObservation(ctx)
		return rec
	}
	if rec := callImport(); rec.Code != http.StatusOK {
		t.Fatalf("import = %d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)
	var payload struct {
		Files []struct {
			Disabled bool                       `json:"disabled"`
			Size     int                        `json:"size"`
			Success  uint64                     `json:"success"`
			Failed   uint64                     `json:"failed"`
			Health   *coreauth.CredentialHealth `json:"credential_health"`
			Runtime  string                     `json:"runtime_state"`
		} `json:"files"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &payload) != nil || len(payload.Files) != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	entry := payload.Files[0]
	if entry.Disabled || entry.Size != len(data) || entry.Success != 0 || entry.Failed != 0 || entry.Runtime != "quarantined" {
		t.Fatalf("health changed independent fields or omitted runtime: %+v", entry)
	}
	if entry.Health == nil || entry.Health.State != "credential_revoked" || entry.Health.Source != "historical_log" || !entry.Health.FirstObservedAt.Equal(first) {
		t.Fatalf("missing historical evidence: %+v", entry.Health)
	}
	if strings.Contains(rec.Body.String(), "synthetic-test-secret") || strings.Contains(rec.Body.String(), "credential_fingerprint") || strings.Contains(rec.Body.String(), "resolved_at") {
		t.Fatal("response contains secret, fingerprint, or a zero resolved timestamp")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(data, after) {
		t.Fatal("health import rewrote credential file")
	}
	// Evidence cannot be attached after the operator replaces the on-disk file.
	if err := os.WriteFile(path, []byte(`{"access_token":"replacement"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := callImport(); rec.Code != http.StatusConflict {
		t.Fatalf("changed file accepted: %d", rec.Code)
	}
	body["auth_id"] = "missing.json"
	if rec := callImport(); rec.Code != http.StatusNotFound {
		t.Fatalf("missing auth accepted: %d", rec.Code)
	}
}
