//go:build !windows

package telemetry

func detectRendererPhysicalMemoryBytes() uint64 {
	return 0
}

func detectRendererHostSnapshot() HostSnapshot {
	return HostSnapshot{}
}
