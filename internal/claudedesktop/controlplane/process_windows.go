//go:build windows

package controlplane

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/windows"
)

func inspectProcess(pid int) (processIdentity, error) {
	if pid <= 1 || uint64(pid) > 2147483647 {
		return processIdentity{}, fmt.Errorf("invalid process identity")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return processIdentity{Dead: true}, nil
	}
	if err != nil {
		return processIdentity{}, fmt.Errorf("process identity cannot be inspected")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return processIdentity{}, fmt.Errorf("process creation identity is unavailable")
	}
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		return processIdentity{Dead: true}, nil
	}
	ticks := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if ticks == 0 {
		return processIdentity{}, fmt.Errorf("process creation identity is empty")
	}
	return processIdentity{Start: "windows-filetime:" + strconv.FormatUint(ticks, 10)}, nil
}
