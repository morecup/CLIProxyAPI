package telemetry

import (
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestSDKSkillLoadedUsesPublishedInventoryOncePerActivation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 9, 30, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.ControlPlane.Worker.Skills = []string{" alpha ", "beta", "alpha", "", "   "}
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	if err := manager.Activate(auth); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(auth); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var skills []sdkEventWrapper
	for _, event := range sdkEventsFromRequests(t, doer.Requests()) {
		if event.EventData.EventName == "tengu_skill_loaded" {
			skills = append(skills, event)
		}
	}
	if len(skills) != 2 {
		t.Fatalf("skill_loaded count = %d", len(skills))
	}
	for index, want := range []string{"alpha", "beta"} {
		if skills[index].EventData.SkillName == nil || *skills[index].EventData.SkillName != want {
			t.Fatalf("skill %d name = %v, want %q", index, skills[index].EventData.SkillName, want)
		}
		if got := sdkMetadataJSON(t, skills[index]); got != `{"subscription_type":"pro"}` {
			t.Fatalf("skill %q metadata = %s", want, got)
		}
	}
}
