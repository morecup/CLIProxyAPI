//go:build windows

package telemetry

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// probeDesktopWindowsElevation reads TOKEN_ELEVATION_TYPE of the current
// process token, which is what the native addon's getWindowsElevationType()
// reports for the Desktop main process.
func probeDesktopWindowsElevation() (string, bool) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", false
	}
	defer token.Close()
	var kind uint32
	var returned uint32
	if err := windows.GetTokenInformation(token, windows.TokenElevationType, (*byte)(unsafe.Pointer(&kind)), uint32(unsafe.Sizeof(kind)), &returned); err != nil {
		return "", false
	}
	return desktopElevationTypeName(kind)
}
