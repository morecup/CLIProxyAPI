package telemetry

import (
	"testing"
	"time"
)

func TestSDKProcessSnapshotAdvancesFromRuntimeStart(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)}
	manager := &Manager{
		now:       clock.Now,
		startedAt: clock.Now(),
		sdkProcessProvider: func() (SDKProcessSnapshot, bool) {
			return SDKProcessSnapshot{UptimeSeconds: 123.5, RSS: 42}, true
		},
	}

	first, okFirst := manager.sdkProcessSnapshot()
	if !okFirst || first.Uptime != 123.5 {
		t.Fatalf("initial process snapshot = %#v, %v", first, okFirst)
	}
	clock.Advance(10 * time.Second)
	second, okSecond := manager.sdkProcessSnapshot()
	if !okSecond || second.Uptime != 133.5 {
		t.Fatalf("advanced process snapshot = %#v, %v", second, okSecond)
	}
	if second.RSS != first.RSS {
		t.Fatalf("measured process baseline changed: first=%#v second=%#v", first, second)
	}
}
