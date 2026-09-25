package watcher

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDesktopCredentialResaveDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "unchanged_credentials"},
		{name: "access_token", change: func(m map[string]any) { m["access_token"] = "new-access-token" }},
		{name: "refresh_token", change: func(m map[string]any) { m["refresh_token"] = "new-refresh-token" }},
		{name: "session_key", change: func(m map[string]any) { m[claudedesktop.MetadataSessionKeyKey] = "new-session-key" }},
		{name: "device_token", change: func(m map[string]any) { m[claudedesktop.MetadataTrustedDeviceTokenKey] = "new-device-token" }},
		{name: "telemetry_materials", change: func(m map[string]any) {
			materials := m[claudedesktop.MetadataTelemetryMaterialsKey].(claudedesktop.TelemetryMaterials)
			materials.SegmentWriteKey = "new-segment-key"
			m[claudedesktop.MetadataTelemetryMaterialsKey] = materials
		}},
		{name: "proxy", change: func(m map[string]any) { m["proxy_url"] = "http://127.0.0.1:9002" }},
		{name: "disabled", change: func(m map[string]any) { m["disabled"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			path := filepath.Join(authDir, "desktop.json")
			metadata := map[string]any{
				"type":                              claudedesktop.Provider,
				claudedesktop.MetadataAuthFlowKey:   claudedesktop.AuthFlowDesktop,
				"access_token":                      "test-access-token",
				"refresh_token":                     "test-refresh-token",
				claudedesktop.MetadataSessionKeyKey: "test-session-key",
				claudedesktop.MetadataTrustedDeviceTokenKey: "test-device-token",
				"proxy_url": "http://127.0.0.1:9001",
				"disabled":  false,
				claudedesktop.MetadataTelemetryMaterialsKey: claudedesktop.TelemetryMaterials{
					SegmentWriteKey:         "test-segment-key",
					DatadogLogsAPIKey:       "test-logs-key",
					DatadogRUMClientToken:   "test-rum-token",
					DatadogRUMApplicationID: "55555555-5555-4555-8555-555555555555",
					SentryPublicKey:         "test-sentry-key",
				},
			}
			if _, err := claudedesktop.TelemetryMaterialsFromMetadata(metadata); err != nil {
				t.Fatalf("invalid telemetry fixture: %v", err)
			}
			ctx := &synthesizer.SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     authDir,
				Now:         time.Now(),
				IDGenerator: synthesizer.NewStableIDGenerator(),
			}
			saveAndLoad := func() ([]byte, *coreauth.Auth) {
				t.Helper()
				if err := claudedesktop.SaveMetadataFile(path, metadata); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				auths, err := synthesizer.SynthesizeAuthFile(ctx, path, data)
				if err != nil || len(auths) != 1 {
					t.Fatalf("synthesize saved credential: auth count = %d, error = %v", len(auths), err)
				}
				return data, auths[0]
			}
			beforeBytes, before := saveAndLoad()
			if tc.change != nil {
				tc.change(metadata)
			}
			afterBytes, after := saveAndLoad()
			if bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("expected credential storage to generate a new ciphertext")
			}
			w := &Watcher{currentAuths: map[string]*coreauth.Auth{before.ID: before.Clone()}}
			updates := w.computePerPathUpdatesLocked(
				map[string]*coreauth.Auth{before.ID: before},
				map[string]*coreauth.Auth{after.ID: after},
			)
			if tc.change == nil {
				if len(updates) != 0 {
					t.Fatalf("credential re-encryption dispatched %d spurious auth updates", len(updates))
				}
			} else if len(updates) != 1 || updates[0].Action != AuthUpdateActionModify {
				t.Fatal("changed credential or routing setting did not dispatch one modify update")
			}
			for _, a := range []*coreauth.Auth{before, after, w.currentAuths[before.ID]} {
				if _, ok := a.Metadata[claudedesktop.MetadataCredentialsKey]; !ok {
					t.Fatal("auth comparison mutated the stored metadata")
				}
			}
		})
	}
}

func TestAuthEqualRetainsUnhydratedCredentialEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flow  string
		token string
	}{
		{name: "desktop_without_token", flow: claudedesktop.AuthFlowDesktop},
		{name: "desktop_blank_token", flow: claudedesktop.AuthFlowDesktop, token: " "},
		{name: "non_desktop", token: "test-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := &coreauth.Auth{Metadata: map[string]any{
				claudedesktop.MetadataAuthFlowKey:    tc.flow,
				"access_token":                       tc.token,
				claudedesktop.MetadataCredentialsKey: "first-envelope",
			}}
			after := before.Clone()
			after.Metadata[claudedesktop.MetadataCredentialsKey] = "second-envelope"
			if authEqual(before, after) {
				t.Fatal("credential envelope changes were ignored without hydrated Desktop credentials")
			}
		})
	}
}
