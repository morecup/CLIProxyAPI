package telemetry

import "testing"

func TestDesktopCodeIdleOutcomesRequireMeasuredActivity(t *testing.T) {
	manager := (*Manager)(nil)
	facts := DesktopCodeLifecycleFacts{SessionID: "session", ConsecutiveDeclines: 1}
	for name, record := range map[string]func() error{
		"timeout": func() error { return manager.RecordDesktopSessionIdleTimeoutStarted(nil, facts) },
		"decline": func() error { return manager.RecordDesktopSessionIdlePauseDeclined(nil, facts) },
		"remote":  func() error { return manager.RecordDesktopSessionPauseBlockedByRemoteControl(nil, facts) },
	} {
		if err := record(); err == nil {
			t.Fatalf("%s accepted an unknown activity timestamp", name)
		}
	}
	if err := manager.RecordDesktopSessionIdleTimeoutCancelled(nil, facts); err != nil {
		t.Fatalf("cancellation should preserve an unknown timestamp as null: %v", err)
	}
}
