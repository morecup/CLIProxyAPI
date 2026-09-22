//go:build windows

package claudedesktop

import (
	"crypto/sha256"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsDPAPIProtector = "windows-dpapi-user"

func platformProtectorName() string { return windowsDPAPIProtector }

func platformProtectionRequired() bool { return true }

func platformProtect(plaintext []byte) (string, []byte, error) {
	input := windows.DataBlob{Size: uint32(len(plaintext))}
	if len(plaintext) > 0 {
		input.Data = &plaintext[0]
	}
	entropyBytes := sha256.Sum256([]byte("CLIProxyAPI Claude Desktop credentials v1"))
	entropy := windows.DataBlob{Size: uint32(len(entropyBytes)), Data: &entropyBytes[0]}
	var output windows.DataBlob
	if errProtect := windows.CryptProtectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); errProtect != nil {
		return "", nil, errProtect
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	if output.Data == nil || output.Size == 0 {
		return "", nil, fmt.Errorf("DPAPI returned no ciphertext")
	}
	return windowsDPAPIProtector, append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func platformUnprotect(ciphertext []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(ciphertext))}
	if len(ciphertext) > 0 {
		input.Data = &ciphertext[0]
	}
	entropyBytes := sha256.Sum256([]byte("CLIProxyAPI Claude Desktop credentials v1"))
	entropy := windows.DataBlob{Size: uint32(len(entropyBytes)), Data: &entropyBytes[0]}
	var output windows.DataBlob
	if errUnprotect := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); errUnprotect != nil {
		return nil, errUnprotect
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	if output.Data == nil {
		return nil, fmt.Errorf("DPAPI returned no plaintext")
	}
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}
