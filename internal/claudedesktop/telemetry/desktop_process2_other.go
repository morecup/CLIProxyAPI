//go:build !windows

package telemetry

// probeDesktopWindowsElevation has no process token elevation type to read
// outside Windows; the event stays unobserved rather than invented.
func probeDesktopWindowsElevation() (string, bool) {
	return "", false
}
