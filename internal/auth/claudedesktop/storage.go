package claudedesktop

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const fallbackKeyFileName = ".claude-desktop-credential-key"

type credentialSecrets struct {
	AccessToken        string             `json:"access_token"`
	RefreshToken       string             `json:"refresh_token,omitempty"`
	SessionKey         string             `json:"session_key,omitempty"`
	TrustedDeviceToken string             `json:"trusted_device_token,omitempty"`
	TelemetryMaterials TelemetryMaterials `json:"telemetry_materials,omitempty"`
}

type credentialEnvelope struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext string `json:"ciphertext"`
}

type TokenStorage struct {
	Type             string     `json:"type"`
	AuthFlow         string     `json:"auth_flow"`
	Email            string     `json:"email"`
	AccountUUID      string     `json:"account_uuid"`
	OrganizationUUID string     `json:"organization_uuid"`
	OrganizationName string     `json:"organization_name,omitempty"`
	Expire           string     `json:"expired"`
	LastRefresh      string     `json:"last_refresh"`
	Enrollment       Enrollment `json:"claude_desktop_enrollment"`

	AccessToken        string             `json:"-"`
	RefreshToken       string             `json:"-"`
	SessionKey         string             `json:"-"`
	TrustedDeviceToken string             `json:"-"`
	TelemetryMaterials TelemetryMaterials `json:"-"`
	Metadata           map[string]any     `json:"-"`
}

func NewTokenStorage(result *LoginResult) (*TokenStorage, error) {
	if result == nil {
		return nil, fmt.Errorf("Claude Desktop login result is nil")
	}
	return &TokenStorage{
		Type:               Provider,
		AuthFlow:           AuthFlowDesktop,
		Email:              result.Identity.Email,
		AccountUUID:        result.Identity.AccountUUID,
		OrganizationUUID:   result.Identity.OrganizationUUID,
		OrganizationName:   result.Identity.OrganizationName,
		Expire:             result.Token.Expire,
		LastRefresh:        time.Now().UTC().Format(time.RFC3339),
		Enrollment:         result.Enrollment,
		AccessToken:        result.Token.AccessToken,
		RefreshToken:       result.Token.RefreshToken,
		SessionKey:         result.SessionKey,
		TrustedDeviceToken: result.Device.DeviceToken,
		TelemetryMaterials: result.TelemetryMaterials,
	}, nil
}

func (s *TokenStorage) SetMetadata(metadata map[string]any) {
	s.Metadata = metadata
}

func (s *TokenStorage) SaveTokenToFile(authFilePath string) error {
	if s == nil {
		return fmt.Errorf("Claude Desktop token storage is nil")
	}
	metadata := make(map[string]any, len(s.Metadata)+12)
	for key, value := range s.Metadata {
		metadata[key] = value
	}
	metadata["type"] = Provider
	metadata[MetadataAuthFlowKey] = AuthFlowDesktop
	metadata["email"] = strings.TrimSpace(s.Email)
	metadata["account_uuid"] = strings.ToLower(strings.TrimSpace(s.AccountUUID))
	metadata["organization_uuid"] = strings.ToLower(strings.TrimSpace(s.OrganizationUUID))
	if value := strings.TrimSpace(s.OrganizationName); value != "" {
		metadata["organization_name"] = value
	}
	metadata["expired"] = s.Expire
	metadata["last_refresh"] = s.LastRefresh
	metadata[MetadataEnrollmentKey] = s.Enrollment
	metadata["claude_device_ids"] = []string{RequestDeviceID(s.Enrollment.DeviceID)}
	metadata["access_token"] = s.AccessToken
	if value := strings.TrimSpace(s.RefreshToken); value != "" {
		metadata["refresh_token"] = value
	} else {
		delete(metadata, "refresh_token")
	}
	if value := strings.TrimSpace(s.SessionKey); value != "" {
		metadata[MetadataSessionKeyKey] = value
	}
	if value := strings.TrimSpace(s.TrustedDeviceToken); value != "" {
		metadata[MetadataTrustedDeviceTokenKey] = value
	} else {
		delete(metadata, MetadataTrustedDeviceTokenKey)
	}
	if errMaterials := s.TelemetryMaterials.Validate(); errMaterials == nil {
		metadata[MetadataTelemetryMaterialsKey] = s.TelemetryMaterials
	}
	return SaveMetadataFile(authFilePath, metadata)
}

func MetadataFromLogin(result *LoginResult) map[string]any {
	if result == nil {
		return nil
	}
	metadata := map[string]any{
		"type":                        Provider,
		MetadataAuthFlowKey:           AuthFlowDesktop,
		"auth_kind":                   "oauth",
		"email":                       result.Identity.Email,
		"account_uuid":                result.Identity.AccountUUID,
		"organization_uuid":           result.Identity.OrganizationUUID,
		"organization_name":           result.Identity.OrganizationName,
		"access_token":                result.Token.AccessToken,
		MetadataSessionKeyKey:         result.SessionKey,
		"expired":                     result.Token.Expire,
		"last_refresh":                time.Now().UTC().Format(time.RFC3339),
		MetadataEnrollmentKey:         result.Enrollment,
		MetadataTelemetryMaterialsKey: result.TelemetryMaterials,
		"claude_device_ids":           []string{RequestDeviceID(result.Device.DeviceID)},
	}
	if value := strings.TrimSpace(result.Token.RefreshToken); value != "" {
		metadata["refresh_token"] = value
	}
	if value := strings.TrimSpace(result.Device.DeviceToken); value != "" {
		metadata[MetadataTrustedDeviceTokenKey] = value
	}
	return metadata
}

func IsDesktopMetadata(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	flow, _ := metadata[MetadataAuthFlowKey].(string)
	return strings.EqualFold(strings.TrimSpace(flow), AuthFlowDesktop)
}

func SaveMetadataFile(path string, metadata map[string]any) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("Claude Desktop credential path is empty")
	}
	if !IsDesktopMetadata(metadata) {
		return fmt.Errorf("refusing to save non-Desktop credential through Claude Desktop storage")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop credential directory: %w", errMkdir)
	}
	prepared, errPrepare := protectedMetadata(path, metadata)
	if errPrepare != nil {
		return errPrepare
	}
	encoded, errMarshal := json.Marshal(prepared)
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop credential: %w", errMarshal)
	}
	misc.LogSavingCredentials(path)
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open Claude Desktop credential: %w", errOpen)
	}
	if _, errWrite := file.Write(encoded); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write Claude Desktop credential: %w", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close Claude Desktop credential: %w", errClose)
	}
	return nil
}

func HydrateMetadata(path string, metadata map[string]any) error {
	if !IsDesktopMetadata(metadata) {
		return nil
	}
	rawEnvelope, ok := metadata[MetadataCredentialsKey]
	if !ok {
		return fmt.Errorf("Claude Desktop credential envelope is missing")
	}
	encodedEnvelope, errMarshal := json.Marshal(rawEnvelope)
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop credential envelope: %w", errMarshal)
	}
	var envelope credentialEnvelope
	if errUnmarshal := json.Unmarshal(encodedEnvelope, &envelope); errUnmarshal != nil {
		return fmt.Errorf("parse Claude Desktop credential envelope: %w", errUnmarshal)
	}
	if envelope.Version != 1 && envelope.Version != 2 && envelope.Version != CredentialsEnvelopeVersion {
		return fmt.Errorf("unsupported Claude Desktop credential envelope version %d", envelope.Version)
	}
	ciphertext, errDecode := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if errDecode != nil {
		return fmt.Errorf("decode Claude Desktop credential envelope: %w", errDecode)
	}
	plaintext, errUnprotect := unprotectForPath(path, envelope.Protector, ciphertext)
	if errUnprotect != nil {
		return fmt.Errorf("decrypt Claude Desktop credential envelope: %w", errUnprotect)
	}
	var secrets credentialSecrets
	if errUnmarshal := json.Unmarshal(plaintext, &secrets); errUnmarshal != nil {
		return fmt.Errorf("parse Claude Desktop credential secrets: %w", errUnmarshal)
	}
	if strings.TrimSpace(secrets.AccessToken) == "" {
		return fmt.Errorf("Claude Desktop credential envelope is incomplete")
	}
	metadata["access_token"] = secrets.AccessToken
	if value := strings.TrimSpace(secrets.RefreshToken); value != "" {
		metadata["refresh_token"] = value
	} else {
		delete(metadata, "refresh_token")
	}
	if value := strings.TrimSpace(secrets.SessionKey); value != "" {
		metadata[MetadataSessionKeyKey] = value
	} else {
		delete(metadata, MetadataSessionKeyKey)
	}
	if value := strings.TrimSpace(secrets.TrustedDeviceToken); value != "" {
		metadata[MetadataTrustedDeviceTokenKey] = value
	} else {
		delete(metadata, MetadataTrustedDeviceTokenKey)
	}
	if errMaterials := secrets.TelemetryMaterials.Validate(); errMaterials == nil {
		metadata[MetadataTelemetryMaterialsKey] = secrets.TelemetryMaterials
	}
	return nil
}

func TelemetryMaterialsFromMetadata(metadata map[string]any) (TelemetryMaterials, error) {
	if metadata == nil {
		return TelemetryMaterials{}, fmt.Errorf("Claude Desktop telemetry materials are unavailable")
	}
	raw, ok := metadata[MetadataTelemetryMaterialsKey]
	if !ok || raw == nil {
		return TelemetryMaterials{}, fmt.Errorf("Claude Desktop telemetry materials are unavailable")
	}
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return TelemetryMaterials{}, fmt.Errorf("marshal Claude Desktop telemetry materials: %w", errMarshal)
	}
	var materials TelemetryMaterials
	if errUnmarshal := json.Unmarshal(encoded, &materials); errUnmarshal != nil {
		return TelemetryMaterials{}, fmt.Errorf("parse Claude Desktop telemetry materials: %w", errUnmarshal)
	}
	if errValidate := materials.Validate(); errValidate != nil {
		return TelemetryMaterials{}, errValidate
	}
	return materials, nil
}

// SessionKeyFromMetadata returns the encrypted-at-rest Claude.ai browser
// session credential. Existing v1/v2 credential envelopes legitimately omit
// it and remain usable for OAuth inference, but cannot reproduce authenticated
// Desktop startup requests until the account is logged in again.
func SessionKeyFromMetadata(metadata map[string]any) (string, error) {
	value := metadataString(metadata, MetadataSessionKeyKey)
	if value == "" {
		return "", fmt.Errorf("Claude Desktop sessionKey is unavailable; log in again to enable authenticated startup requests")
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("Claude Desktop sessionKey is invalid")
	}
	return value, nil
}

// FindReusableTrustedDevice loads the newest valid trusted-device enrollment
// for the signed-in account. Claude Desktop persists this credential per
// account, so it may be reused when the same account switches organizations.
func FindReusableTrustedDevice(authDir string, identity AccountIdentity) (TrustedDevice, Enrollment, bool, error) {
	identity = normalizeIdentity(identity)
	if _, errAccount := uuid.Parse(identity.AccountUUID); errAccount != nil {
		return TrustedDevice{}, Enrollment{}, false, fmt.Errorf("find reusable Claude Desktop device: invalid account UUID")
	}
	resolvedDir, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		return TrustedDevice{}, Enrollment{}, false, errResolve
	}
	if _, errStat := os.Stat(resolvedDir); errStat != nil {
		if errors.Is(errStat, os.ErrNotExist) {
			return TrustedDevice{}, Enrollment{}, false, nil
		}
		return TrustedDevice{}, Enrollment{}, false, errStat
	}

	var newestDevice TrustedDevice
	var newestEnrollment Enrollment
	var newestTime time.Time
	found := false
	errWalk := filepath.WalkDir(resolvedDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if filepath.Clean(path) == filepath.Clean(resolvedDir) {
				return walkErr
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil || len(raw) == 0 {
			return nil
		}
		metadata := make(map[string]any)
		if errUnmarshal := json.Unmarshal(raw, &metadata); errUnmarshal != nil || !IsDesktopMetadata(metadata) {
			return nil
		}
		if !strings.EqualFold(metadataString(metadata, "account_uuid"), identity.AccountUUID) {
			return nil
		}
		if errHydrate := HydrateMetadata(path, metadata); errHydrate != nil {
			return nil
		}
		enrollment, errEnrollment := ParseEnrollment(metadata)
		if errEnrollment != nil {
			return nil
		}
		expectedAuthID, errAuthID := StableAuthID(enrollment.AccountUUID, enrollment.OrganizationUUID)
		if errAuthID != nil {
			return nil
		}
		if _, errValidate := ValidateTrustedDeviceEnrollment(expectedAuthID, metadata); errValidate != nil {
			return nil
		}
		updatedAt, errUpdated := time.Parse(time.RFC3339, enrollment.UpdatedAt)
		if errUpdated != nil {
			updatedAt, _ = time.Parse(time.RFC3339, enrollment.RegisteredAt)
		}
		if updatedAt.IsZero() {
			if info, errInfo := entry.Info(); errInfo == nil {
				updatedAt = info.ModTime()
			}
		}
		if found && !updatedAt.After(newestTime) {
			return nil
		}
		displayName := strings.TrimSpace(enrollment.DisplayName)
		if displayName == "" {
			displayName = defaultDisplayName()
		}
		newestDevice = TrustedDevice{
			DeviceID:    enrollment.DeviceID,
			DeviceToken: metadataString(metadata, MetadataTrustedDeviceTokenKey),
			DisplayName: displayName,
		}
		newestEnrollment = enrollment
		newestTime = updatedAt
		found = true
		return nil
	})
	if errWalk != nil {
		return TrustedDevice{}, Enrollment{}, false, errWalk
	}
	return newestDevice, newestEnrollment, found, nil
}

func protectedMetadata(path string, metadata map[string]any) (map[string]any, error) {
	secrets := credentialSecrets{
		AccessToken:        metadataString(metadata, "access_token"),
		RefreshToken:       metadataString(metadata, "refresh_token"),
		SessionKey:         metadataString(metadata, MetadataSessionKeyKey),
		TrustedDeviceToken: metadataString(metadata, MetadataTrustedDeviceTokenKey),
	}
	if materials, errMaterials := TelemetryMaterialsFromMetadata(metadata); errMaterials == nil {
		secrets.TelemetryMaterials = materials
	}
	if secrets.AccessToken == "" {
		return nil, fmt.Errorf("Claude Desktop credential secrets are incomplete")
	}
	plaintext, errMarshal := json.Marshal(secrets)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal Claude Desktop credential secrets: %w", errMarshal)
	}
	protector, ciphertext, errProtect := protectForPath(path, plaintext)
	if errProtect != nil {
		return nil, fmt.Errorf("protect Claude Desktop credential secrets: %w", errProtect)
	}
	prepared := make(map[string]any, len(metadata))
	for key, value := range metadata {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "access_token", "refreshtoken", "refresh_token", MetadataSessionKeyKey, MetadataTrustedDeviceTokenKey, MetadataTelemetryMaterialsKey, MetadataCredentialsKey:
			continue
		default:
			prepared[key] = value
		}
	}
	prepared[MetadataCredentialsKey] = credentialEnvelope{
		Version:    CredentialsEnvelopeVersion,
		Protector:  protector,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	return prepared, nil
}

func protectForPath(path string, plaintext []byte) (string, []byte, error) {
	if protector, ciphertext, errPlatform := platformProtect(plaintext); errPlatform == nil {
		return protector, ciphertext, nil
	} else if platformProtectionRequired() {
		return "", nil, fmt.Errorf("required OS credential protection failed: %w", errPlatform)
	}
	key, errKey := loadOrCreateFallbackKey(filepath.Dir(path))
	if errKey != nil {
		return "", nil, errKey
	}
	ciphertext, errEncrypt := encryptAESGCM(key, plaintext)
	return "file-key-aesgcm", ciphertext, errEncrypt
}

func unprotectForPath(path, protector string, ciphertext []byte) ([]byte, error) {
	switch protector {
	case platformProtectorName():
		return platformUnprotect(ciphertext)
	case "file-key-aesgcm":
		key, errKey := loadOrCreateFallbackKey(filepath.Dir(path))
		if errKey != nil {
			return nil, errKey
		}
		return decryptAESGCM(key, ciphertext)
	default:
		return nil, fmt.Errorf("unsupported credential protector %q", protector)
	}
}

// ProtectRuntimePayload encrypts account-scoped Claude Desktop runtime state
// with the same current-user protection policy used for Desktop credentials.
// Callers are responsible for storing the returned protector name alongside
// the ciphertext; plaintext identity or telemetry payloads must not be stored
// outside the protected envelope.
func ProtectRuntimePayload(path string, plaintext []byte) (string, []byte, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil, fmt.Errorf("Claude Desktop runtime state path is empty")
	}
	return protectForPath(path, plaintext)
}

// UnprotectRuntimePayload decrypts data produced by ProtectRuntimePayload.
func UnprotectRuntimePayload(path, protector string, ciphertext []byte) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("Claude Desktop runtime state path is empty")
	}
	return unprotectForPath(path, protector, ciphertext)
}

func loadOrCreateFallbackKey(directory string) ([]byte, error) {
	keyPath := filepath.Join(directory, fallbackKeyFileName)
	key, errRead := os.ReadFile(keyPath)
	if errRead == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("Claude Desktop credential key has invalid length")
		}
		return key, nil
	}
	if !errors.Is(errRead, os.ErrNotExist) {
		return nil, errRead
	}
	key = make([]byte, 32)
	if _, errRandom := io.ReadFull(rand.Reader, key); errRandom != nil {
		return nil, errRandom
	}
	file, errOpen := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrExist) {
			return loadOrCreateFallbackKey(directory)
		}
		return nil, errOpen
	}
	if _, errWrite := file.Write(key); errWrite != nil {
		_ = file.Close()
		_ = os.Remove(keyPath)
		return nil, errWrite
	}
	if errClose := file.Close(); errClose != nil {
		_ = os.Remove(keyPath)
		return nil, errClose
	}
	return key, nil
}

func encryptAESGCM(key, plaintext []byte) ([]byte, error) {
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return nil, errCipher
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return nil, errGCM
	}
	nonce := make([]byte, aead.NonceSize())
	if _, errRandom := io.ReadFull(rand.Reader, nonce); errRandom != nil {
		return nil, errRandom
	}
	return aead.Seal(nonce, nonce, plaintext, []byte(Provider+":"+AuthFlowDesktop)), nil
}

func decryptAESGCM(key, ciphertext []byte) ([]byte, error) {
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return nil, errCipher
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return nil, errGCM
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, fmt.Errorf("credential ciphertext is truncated")
	}
	nonce := ciphertext[:aead.NonceSize()]
	return aead.Open(nil, nonce, ciphertext[aead.NonceSize():], []byte(Provider+":"+AuthFlowDesktop))
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
