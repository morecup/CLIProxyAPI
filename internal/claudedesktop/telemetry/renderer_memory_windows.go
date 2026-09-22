//go:build windows

package telemetry

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type rendererMemoryStatusEx struct {
	length                uint32
	memoryLoad            uint32
	totalPhysical         uint64
	availablePhysical     uint64
	totalPageFile         uint64
	availablePageFile     uint64
	totalVirtual          uint64
	availableVirtual      uint64
	availableExtendedVirt uint64
}

var rendererGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func detectRendererPhysicalMemoryBytes() uint64 {
	status, ok := rendererMemoryStatus()
	if !ok {
		return 0
	}
	return status.totalPhysical
}

func detectRendererHostSnapshot() HostSnapshot {
	status, _ := rendererMemoryStatus()
	snapshot := HostSnapshot{
		TotalMemoryBytes:     status.totalPhysical,
		AvailableMemoryBytes: status.availablePhysical,
	}
	if key, errOpen := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); errOpen == nil {
		snapshot.CPUModel, _, _ = key.GetStringValue("ProcessorNameString")
		snapshot.CPUModel = strings.TrimSpace(snapshot.CPUModel)
		_ = key.Close()
	}
	if key, errOpen := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); errOpen == nil {
		build, _, _ := key.GetStringValue("CurrentBuildNumber")
		major, _, _ := key.GetIntegerValue("CurrentMajorVersionNumber")
		minor, _, _ := key.GetIntegerValue("CurrentMinorVersionNumber")
		if build != "" {
			if major == 0 {
				major = 10
			}
			snapshot.OSRelease = fmt.Sprintf("%d.%d.%s", major, minor, build)
			snapshot.OSVersion = snapshot.OSRelease
		}
		snapshot.OSBuild, _, _ = key.GetStringValue("BuildLabEx")
		snapshot.OSBuild = strings.TrimSpace(snapshot.OSBuild)
		_ = key.Close()
	}
	return snapshot
}

func rendererMemoryStatus() (rendererMemoryStatusEx, bool) {
	status := rendererMemoryStatusEx{length: uint32(unsafe.Sizeof(rendererMemoryStatusEx{}))}
	result, _, _ := rendererGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if result == 0 {
		return rendererMemoryStatusEx{}, false
	}
	return status, true
}
