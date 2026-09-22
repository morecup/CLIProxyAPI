//go:build !windows

package claudedesktop

import "fmt"

func platformProtectorName() string { return "unavailable" }

func platformProtectionRequired() bool { return false }

func platformProtect([]byte) (string, []byte, error) {
	return "", nil, fmt.Errorf("OS credential protection is unavailable")
}

func platformUnprotect([]byte) ([]byte, error) {
	return nil, fmt.Errorf("OS credential protection is unavailable")
}
