package claudedesktop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTelemetryMaterialsFromOfficialResourceShapes(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "ion-dist")
	if errMkdir := os.MkdirAll(filepath.Join(webRoot, "assets", "v1"), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	materials := testTelemetryMaterials()
	webConfig := `const app={segmentKey:"` + materials.SegmentWriteKey + `",segmentCdnHost:"a-cdn.anthropic.com",segmentApiHost:"a-api.anthropic.com"};` +
		`init({applicationId:"` + materials.DatadogRUMApplicationID + `",clientToken:"` + materials.DatadogRUMClientToken + `",site:"us5.datadoghq.com"});`
	if errWrite := os.WriteFile(filepath.Join(webRoot, "assets", "v1", "index-fixture.js"), []byte(webConfig), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	archive := filepath.Join(root, "app.asar")
	sentryConfig := `init({dsn:` + "`https://" + materials.SentryPublicKey + `@o1158394.ingest.us.sentry.io/4507368973008896` + "`" + `});`
	if errWrite := os.WriteFile(archive, []byte(sentryConfig), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	codeBinary := filepath.Join(root, "claude.exe")
	codeConfig := append([]byte("binary-prefix\x00https://http-intake.logs.us5.datadoghq.com/api/v2/logs\x00\x04"), []byte(materials.DatadogLogsAPIKey)...)
	codeConfig = append(codeConfig, []byte("\x00binary-suffix")...)
	if errWrite := os.WriteFile(codeBinary, codeConfig, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	resolved, errResolve := resolveTelemetryMaterialsFromSources(context.Background(), telemetryMaterialSources{
		resourceArchives: []string{archive},
		webRoots:         []string{webRoot},
		codeBinaries:     []string{codeBinary},
	})
	if errResolve != nil {
		t.Fatalf("resolveTelemetryMaterialsFromSources() error = %v", errResolve)
	}
	if resolved != materials {
		t.Fatalf("resolved materials did not match the official resource shapes")
	}
}

func TestResolveTelemetryMaterialsReportsMissingMaterialWithoutValues(t *testing.T) {
	_, errResolve := resolveTelemetryMaterialsFromSources(context.Background(), telemetryMaterialSources{})
	if errResolve == nil {
		t.Fatal("resolveTelemetryMaterialsFromSources() error = nil")
	}
	if got := errResolve.Error(); got == "" || containsTelemetryMaterialValue(got, testTelemetryMaterials()) {
		t.Fatalf("resolver error exposed material values: %q", got)
	}
}

func containsTelemetryMaterialValue(text string, materials TelemetryMaterials) bool {
	for _, value := range []string{
		materials.SegmentWriteKey,
		materials.DatadogLogsAPIKey,
		materials.DatadogRUMClientToken,
		materials.DatadogRUMApplicationID,
		materials.SentryPublicKey,
	} {
		if value != "" && len(value) > 0 && containsString(text, value) {
			return true
		}
	}
	return false
}

func containsString(text, value string) bool {
	for index := 0; index+len(value) <= len(text); index++ {
		if text[index:index+len(value)] == value {
			return true
		}
	}
	return false
}
