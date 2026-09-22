package claudedesktop

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSaveMetadataFileProtectsAndHydratesSecrets(t *testing.T) {
	authDir := t.TempDir()
	identity := AccountIdentity{
		AccountUUID:      testAccountUUID,
		Email:            "desktop@example.com",
		OrganizationUUID: testOrgAUUID,
		OrganizationName: "Desktop Org",
	}
	device := TrustedDevice{DeviceID: testDeviceUUID, DeviceToken: "secret-trusted-device", DisplayName: "Claude Desktop test"}
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	result := &LoginResult{
		AuthID:     authID,
		SessionKey: "secret-browser-session-key",
		Token: TokenData{
			AccessToken:  "secret-access-token",
			RefreshToken: "secret-refresh-token",
			ExpiresIn:    3600,
			Expire:       time.Date(2026, 9, 2, 16, 0, 0, 0, time.UTC).Format(time.RFC3339),
		},
		Identity:           identity,
		Enrollment:         NewEnrollment(authID, identity, device, time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)),
		Device:             device,
		TelemetryMaterials: testTelemetryMaterials(),
	}
	path := filepath.Join(authDir, authID)
	if errSave := SaveMetadataFile(path, MetadataFromLogin(result)); errSave != nil {
		t.Fatalf("SaveMetadataFile() error = %v", errSave)
	}

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	materials := testTelemetryMaterials()
	for _, secret := range [][]byte{
		[]byte("secret-access-token"),
		[]byte("secret-refresh-token"),
		[]byte("secret-browser-session-key"),
		[]byte("secret-trusted-device"),
		[]byte(materials.SegmentWriteKey),
		[]byte(materials.DatadogLogsAPIKey),
		[]byte(materials.DatadogRUMClientToken),
		[]byte(materials.DatadogRUMApplicationID),
		[]byte(materials.SentryPublicKey),
	} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("credential file contains plaintext secret %q", secret)
		}
	}
	var stored map[string]any
	if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, key := range []string{"access_token", "refresh_token", MetadataSessionKeyKey, MetadataTrustedDeviceTokenKey, MetadataTelemetryMaterialsKey} {
		if _, exists := stored[key]; exists {
			t.Fatalf("credential file contains plaintext field %q", key)
		}
	}
	envelopeMap, ok := stored[MetadataCredentialsKey].(map[string]any)
	if !ok {
		t.Fatalf("credential envelope missing: %#v", stored[MetadataCredentialsKey])
	}
	protector, _ := envelopeMap["protector"].(string)
	if runtime.GOOS == "windows" {
		if protector != platformProtectorName() {
			t.Fatalf("protector = %q, want %q", protector, platformProtectorName())
		}
		if _, errStat := os.Stat(filepath.Join(authDir, fallbackKeyFileName)); !os.IsNotExist(errStat) {
			t.Fatalf("Windows credential storage created fallback key: %v", errStat)
		}
	} else if protector != "file-key-aesgcm" {
		t.Fatalf("protector = %q, want file-key-aesgcm", protector)
	}

	if errHydrate := HydrateMetadata(path, stored); errHydrate != nil {
		t.Fatalf("HydrateMetadata() error = %v", errHydrate)
	}
	if stored["access_token"] != "secret-access-token" || stored["refresh_token"] != "secret-refresh-token" || stored[MetadataTrustedDeviceTokenKey] != "secret-trusted-device" {
		t.Fatalf("hydrated secrets do not match")
	}
	if sessionKey, errSessionKey := SessionKeyFromMetadata(stored); errSessionKey != nil || sessionKey != "secret-browser-session-key" {
		t.Fatalf("hydrated sessionKey does not match: value=%q error=%v", sessionKey, errSessionKey)
	}
	hydratedMaterials, errMaterials := TelemetryMaterialsFromMetadata(stored)
	if errMaterials != nil || hydratedMaterials != materials {
		t.Fatalf("hydrated telemetry materials do not match: %v", errMaterials)
	}
}

func TestLegacyCredentialEnvelopesHydrateWithoutSessionKey(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			authDir := t.TempDir()
			path := filepath.Join(authDir, "legacy.json")
			plaintext, errMarshal := json.Marshal(credentialSecrets{
				AccessToken:        "legacy-access-token",
				RefreshToken:       "legacy-refresh-token",
				TrustedDeviceToken: "legacy-device-token",
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			protector, ciphertext, errProtect := protectForPath(path, plaintext)
			if errProtect != nil {
				t.Fatal(errProtect)
			}
			metadata := map[string]any{
				"type":              Provider,
				MetadataAuthFlowKey: AuthFlowDesktop,
				MetadataCredentialsKey: credentialEnvelope{
					Version:    version,
					Protector:  protector,
					Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
				},
			}

			if errHydrate := HydrateMetadata(path, metadata); errHydrate != nil {
				t.Fatalf("HydrateMetadata() error = %v", errHydrate)
			}
			if metadata["access_token"] != "legacy-access-token" || metadata["refresh_token"] != "legacy-refresh-token" || metadata[MetadataTrustedDeviceTokenKey] != "legacy-device-token" {
				t.Fatalf("legacy secrets were not hydrated: %#v", metadata)
			}
			if _, errSessionKey := SessionKeyFromMetadata(metadata); errSessionKey == nil || !strings.Contains(errSessionKey.Error(), "log in again") {
				t.Fatalf("SessionKeyFromMetadata() error = %v, want relogin requirement", errSessionKey)
			}

			if errSave := SaveMetadataFile(path, metadata); errSave != nil {
				t.Fatalf("SaveMetadataFile() error = %v", errSave)
			}
			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			for _, secret := range [][]byte{[]byte("legacy-access-token"), []byte("legacy-refresh-token"), []byte("legacy-device-token")} {
				if bytes.Contains(raw, secret) {
					t.Fatalf("resaved credential exposes plaintext secret %q", secret)
				}
			}
			var resaved map[string]any
			if errUnmarshal := json.Unmarshal(raw, &resaved); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if _, exists := resaved[MetadataSessionKeyKey]; exists {
				t.Fatal("resaved legacy credential exposes a plaintext sessionKey field")
			}
			envelope, okEnvelope := resaved[MetadataCredentialsKey].(map[string]any)
			if !okEnvelope || int(envelope["version"].(float64)) != CredentialsEnvelopeVersion {
				t.Fatalf("resaved envelope was not migrated to v%d: %#v", CredentialsEnvelopeVersion, resaved[MetadataCredentialsKey])
			}
			if errHydrate := HydrateMetadata(path, resaved); errHydrate != nil {
				t.Fatalf("HydrateMetadata(resaved) error = %v", errHydrate)
			}
			if _, errSessionKey := SessionKeyFromMetadata(resaved); errSessionKey == nil || !strings.Contains(errSessionKey.Error(), "log in again") {
				t.Fatalf("SessionKeyFromMetadata(resaved) error = %v, want relogin requirement", errSessionKey)
			}
		})
	}
}

func TestAccessOnlyCredentialWithoutTrustedDeviceSupportsInferenceButIsNotReusable(t *testing.T) {
	authDir := t.TempDir()
	identity := AccountIdentity{
		AccountUUID:      testAccountUUID,
		Email:            "desktop@example.com",
		OrganizationUUID: testOrgAUUID,
		OrganizationName: "Desktop Org",
	}
	device := TrustedDevice{DeviceID: testDeviceUUID, DisplayName: "Claude Desktop local"}
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := NewEnrollment(authID, identity, device, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
	enrollment, errTransition := TransitionEnrollment(enrollment, EnrollmentActive, "", time.Date(2026, 9, 21, 10, 0, 1, 0, time.UTC))
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	result := &LoginResult{
		AuthID:     authID,
		SessionKey: "secret-browser-session-key",
		Token: TokenData{
			AccessToken: "secret-access-token",
			Expire:      time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC).Format(time.RFC3339),
		},
		Identity:   identity,
		Enrollment: enrollment,
		Device:     device,
	}
	metadata := MetadataFromLogin(result)
	if _, exists := metadata[MetadataTrustedDeviceTokenKey]; exists {
		t.Fatal("metadata retained an empty trusted-device credential")
	}
	if _, exists := metadata["refresh_token"]; exists {
		t.Fatal("metadata retained an empty refresh token")
	}
	path := filepath.Join(authDir, authID)
	if errSave := SaveMetadataFile(path, metadata); errSave != nil {
		t.Fatalf("SaveMetadataFile() error = %v", errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, secret := range [][]byte{[]byte("secret-access-token"), []byte("secret-browser-session-key")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("credential file contains plaintext secret %q", secret)
		}
	}
	var stored map[string]any
	if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if _, exists := stored[MetadataTrustedDeviceTokenKey]; exists {
		t.Fatal("credential file contains an empty trusted-device field")
	}
	if _, exists := stored["refresh_token"]; exists {
		t.Fatal("credential file contains an empty refresh-token field")
	}
	if errHydrate := HydrateMetadata(path, stored); errHydrate != nil {
		t.Fatalf("HydrateMetadata() error = %v", errHydrate)
	}
	if stored["access_token"] != "secret-access-token" {
		t.Fatal("access token was not hydrated")
	}
	if _, exists := stored["refresh_token"]; exists {
		t.Fatal("hydration injected an empty refresh token")
	}
	if _, exists := stored[MetadataTrustedDeviceTokenKey]; exists {
		t.Fatal("hydration injected an empty trusted-device credential")
	}
	if _, errValidate := ValidateActiveEnrollment(authID, stored); errValidate != nil {
		t.Fatalf("inference enrollment was rejected: %v", errValidate)
	}
	if _, _, found, errReusable := FindReusableTrustedDevice(authDir, identity); errReusable != nil || found {
		t.Fatalf("FindReusableTrustedDevice() found=%t error=%v", found, errReusable)
	}
}
