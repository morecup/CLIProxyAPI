//go:build windows

package helps

import "golang.org/x/sys/windows"

func atomicReplaceFile(source, destination string) error {
	sourcePointer, errSource := windows.UTF16PtrFromString(source)
	if errSource != nil {
		return errSource
	}
	destinationPointer, errDestination := windows.UTF16PtrFromString(destination)
	if errDestination != nil {
		return errDestination
	}
	return windows.MoveFileEx(
		sourcePointer,
		destinationPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
