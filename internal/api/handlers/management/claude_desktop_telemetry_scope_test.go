package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
)

func TestClaudeDesktopTelemetryFullObservedScope(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	manager := claudetelemetry.NewManager(claudetelemetry.Options{Bundle: bundle, StatePath: t.TempDir()})
	t.Cleanup(manager.Close)
	router := gin.New()
	handler := &Handler{}
	router.GET("/telemetry", handler.GetClaudeDesktopTelemetry)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/telemetry", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var response struct {
		Telemetry []claudetelemetry.Status `json:"telemetry"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var snapshot *claudetelemetry.Status
	for i := range response.Telemetry {
		if response.Telemetry[i].ProfileID == bundle.ProfileID {
			snapshot = &response.Telemetry[i]
			break
		}
	}
	if snapshot == nil || snapshot.TelemetryEvidence == nil || snapshot.TelemetryEvidence.ObservedScope == nil {
		t.Fatal("missing management provenance")
	}
	scope := snapshot.TelemetryEvidence.ObservedScope
	if scope.BaselineEndpointEventCount != 260 || scope.SupplementalEndpointEventCount != 43 || len(scope.Sources) != 13 ||
		snapshot.LiveEmitterCoverage.ObservableEndpointEventCount != 303 || len(snapshot.LiveEmitterCoverage.EndpointEvents) != 303 {
		t.Fatal("management scope is incomplete")
	}
	if scope.Sources[10].EndpointEventCount != 0 {
		t.Fatal("empty diagnostic source omitted")
	}
	encodedScope, errScope := json.Marshal(scope)
	if errScope != nil || strings.Contains(string(encodedScope), `"endpoint_events"`) {
		t.Fatal("management source metadata must omit overlapping membership arrays")
	}
	for _, secret := range []string{`"state_path"`, `"authorization"`, `"cookie"`, `"access_token"`, `"refresh_token"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("management exposes %s", secret)
		}
	}
	// Optional offline UI fixture: only this synthetic, empty-account snapshot
	// is exported. No credentials, account state or source payloads are read.
	if os.Getenv("CLAUDE_DESKTOP_SCOPE_TEST_EXPORT") == "1" {
		snapshot.AppSessionIDHash = ""
		snapshot.RuntimeStartedAt = nil
		fixture := struct {
			Telemetry []claudetelemetry.Status `json:"telemetry"`
		}{[]claudetelemetry.Status{*snapshot}}
		encoded, errJSON := json.Marshal(fixture)
		if errJSON != nil {
			t.Fatal(errJSON)
		}
		t.Logf("observed-scope-snapshot: %s", encoded)
	}
}
