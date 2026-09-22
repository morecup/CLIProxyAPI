package controlplane

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
)

const protectedStateVersion = 1

type protectedStateEnvelope struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext string `json:"ciphertext"`
}

func protectedSessionPath(root, localSessionID string) string {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(localSessionID) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("claude-desktop-control-session-v1\x00" + strings.TrimSpace(localSessionID)))
	return filepath.Join(root, "control-plane", "sessions", hex.EncodeToString(digest[:16])+".json")
}

func writeProtectedState(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("Claude Desktop control-plane state path is empty")
	}
	plaintext, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop control-plane state: %w", errMarshal)
	}
	protector, ciphertext, errProtect := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if errProtect != nil {
		return fmt.Errorf("protect Claude Desktop control-plane state: %w", errProtect)
	}
	encoded, errEnvelope := json.Marshal(protectedStateEnvelope{
		Version:    protectedStateVersion,
		Protector:  protector,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if errEnvelope != nil {
		return fmt.Errorf("marshal Claude Desktop control-plane state envelope: %w", errEnvelope)
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop control-plane state directory: %w", errMkdir)
	}
	temporary := filepath.Join(filepath.Dir(path), ".tmp-"+uuid.NewString())
	file, errOpen := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		return fmt.Errorf("create Claude Desktop control-plane state file: %w", errOpen)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, errWrite := file.Write(encoded); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write Claude Desktop control-plane state: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return fmt.Errorf("sync Claude Desktop control-plane state: %w", errSync)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close Claude Desktop control-plane state: %w", errClose)
	}
	if errRename := os.Rename(temporary, path); errRename != nil {
		return fmt.Errorf("publish Claude Desktop control-plane state: %w", errRename)
	}
	removeTemporary = false
	return nil
}

func readProtectedState(path string, value any) error {
	encoded, errRead := os.ReadFile(path)
	if errRead != nil {
		return errRead
	}
	var envelope protectedStateEnvelope
	if errUnmarshal := json.Unmarshal(encoded, &envelope); errUnmarshal != nil {
		return fmt.Errorf("parse Claude Desktop control-plane state envelope: %w", errUnmarshal)
	}
	if envelope.Version != protectedStateVersion {
		return fmt.Errorf("unsupported Claude Desktop control-plane state envelope version %d", envelope.Version)
	}
	ciphertext, errDecode := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if errDecode != nil {
		return fmt.Errorf("decode Claude Desktop control-plane state: %w", errDecode)
	}
	plaintext, errUnprotect := claudedesktop.UnprotectRuntimePayload(path, envelope.Protector, ciphertext)
	if errUnprotect != nil {
		return fmt.Errorf("unprotect Claude Desktop control-plane state: %w", errUnprotect)
	}
	if errJSON := json.Unmarshal(plaintext, value); errJSON != nil {
		return fmt.Errorf("parse Claude Desktop control-plane state: %w", errJSON)
	}
	return nil
}
