package startup

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
)

const persistentIdentityVersion = 1

type persistentIdentity struct {
	Version        int               `json:"version"`
	InstallationID string            `json:"installation_id"`
	CreatedAt      string            `json:"created_at"`
	ETags          map[string]string `json:"etags,omitempty"`
}

type protectedIdentityEnvelope struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext string `json:"ciphertext"`
}

func loadOrCreateIdentity(path string, now time.Time) (persistentIdentity, error) {
	identity, errRead := readIdentity(path)
	if errRead == nil {
		return identity, nil
	}
	if !errors.Is(errRead, os.ErrNotExist) {
		return persistentIdentity{}, errRead
	}
	identity = persistentIdentity{
		Version: persistentIdentityVersion, InstallationID: uuid.New().String(),
		CreatedAt: now.UTC().Format(time.RFC3339Nano), ETags: make(map[string]string),
	}
	if errSave := saveIdentity(path, identity); errSave != nil {
		return persistentIdentity{}, errSave
	}
	return identity, nil
}

func readIdentity(path string) (persistentIdentity, error) {
	payload, errRead := os.ReadFile(path)
	if errRead != nil {
		return persistentIdentity{}, errRead
	}
	var envelope protectedIdentityEnvelope
	if errJSON := json.Unmarshal(payload, &envelope); errJSON != nil || envelope.Version != persistentIdentityVersion {
		return persistentIdentity{}, fmt.Errorf("Claude Desktop startup identity envelope is invalid")
	}
	ciphertext, errDecode := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if errDecode != nil {
		return persistentIdentity{}, fmt.Errorf("decode Claude Desktop startup identity: %w", errDecode)
	}
	plaintext, errUnprotect := claudedesktop.UnprotectRuntimePayload(path, envelope.Protector, ciphertext)
	if errUnprotect != nil {
		return persistentIdentity{}, fmt.Errorf("decrypt Claude Desktop startup identity: %w", errUnprotect)
	}
	var identity persistentIdentity
	if errJSON := json.Unmarshal(plaintext, &identity); errJSON != nil || identity.Version != persistentIdentityVersion {
		return persistentIdentity{}, fmt.Errorf("parse Claude Desktop startup identity")
	}
	if _, errUUID := uuid.Parse(strings.TrimSpace(identity.InstallationID)); errUUID != nil {
		return persistentIdentity{}, fmt.Errorf("Claude Desktop startup installation identity is invalid")
	}
	if identity.ETags == nil {
		identity.ETags = make(map[string]string)
	}
	return identity, nil
}

func saveIdentity(path string, identity persistentIdentity) error {
	if identity.Version != persistentIdentityVersion {
		return fmt.Errorf("Claude Desktop startup identity version is invalid")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return errMkdir
	}
	plaintext, errJSON := json.Marshal(identity)
	if errJSON != nil {
		return errJSON
	}
	protector, ciphertext, errProtect := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if errProtect != nil {
		return errProtect
	}
	payload, errEnvelope := json.Marshal(protectedIdentityEnvelope{
		Version: persistentIdentityVersion, Protector: protector, Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if errEnvelope != nil {
		return errEnvelope
	}
	temporaryPath := filepath.Join(filepath.Dir(path), ".startup-identity-"+uuid.NewString()+".tmp")
	temporary, errOpen := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		return errOpen
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, errWrite := temporary.Write(payload); errWrite != nil {
		_ = temporary.Close()
		return errWrite
	}
	if errSync := temporary.Sync(); errSync != nil {
		_ = temporary.Close()
		return errSync
	}
	if errClose := temporary.Close(); errClose != nil {
		return errClose
	}
	if errReplace := replaceIdentityFile(temporaryPath, path); errReplace != nil {
		return errReplace
	}
	removeTemporary = false
	return nil
}
