package config

import "testing"

func TestSanitizeClaudeDesktopMachineProfiles(t *testing.T) {
	cfg := &Config{ClaudeDesktop: ClaudeDesktopConfig{
		MachineProfiles: []ClaudeDesktopMachineProfile{
			{
				ID:                   " machine-a ",
				TotalMemoryBytes:     16,
				AvailableMemoryBytes: 32,
				CPUModel:             " cpu-a ",
				OSBuild:              " build-a ",
				OSRelease:            " release-a ",
				OSVersion:            " version-a ",
			},
			{ID: "machine-a", CPUModel: "duplicate"},
			{ID: "   "},
			{ID: "machine-b", AvailableMemoryBytes: 8},
		},
		MachineProfileBindings: map[string]string{
			" auth-a ": " machine-a ",
			"":         "machine-b",
			"auth-b":   "   ",
		},
	}}

	cfg.SanitizeClaudeDesktop()

	if len(cfg.ClaudeDesktop.MachineProfiles) != 2 {
		t.Fatalf("machine profiles = %#v, want two normalized unique profiles", cfg.ClaudeDesktop.MachineProfiles)
	}
	first := cfg.ClaudeDesktop.MachineProfiles[0]
	if first.ID != "machine-a" || first.CPUModel != "cpu-a" || first.OSBuild != "build-a" || first.OSRelease != "release-a" || first.OSVersion != "version-a" {
		t.Fatalf("first machine profile was not trimmed: %#v", first)
	}
	if first.AvailableMemoryBytes != first.TotalMemoryBytes {
		t.Fatalf("available memory = %d, want clamp to %d", first.AvailableMemoryBytes, first.TotalMemoryBytes)
	}
	if got := cfg.ClaudeDesktop.MachineProfileBindings; len(got) != 1 || got["auth-a"] != "machine-a" {
		t.Fatalf("machine profile bindings = %#v, want normalized auth-a binding", got)
	}
}

func TestCloneForRuntimeDoesNotShareClaudeDesktopMachinePool(t *testing.T) {
	cfg := &Config{ClaudeDesktop: ClaudeDesktopConfig{
		MachineProfiles: []ClaudeDesktopMachineProfile{{ID: "machine-a", CPUModel: "cpu-a"}},
		MachineProfileBindings: map[string]string{
			"auth-a": "machine-a",
		},
	}}

	clone := cfg.CloneForRuntime()
	cfg.ClaudeDesktop.MachineProfiles[0].CPUModel = "mutated-original"
	cfg.ClaudeDesktop.MachineProfileBindings["auth-a"] = "mutated-original"
	if clone.ClaudeDesktop.MachineProfiles[0].CPUModel != "cpu-a" {
		t.Fatalf("clone machine profile changed with original: %#v", clone.ClaudeDesktop.MachineProfiles)
	}
	if clone.ClaudeDesktop.MachineProfileBindings["auth-a"] != "machine-a" {
		t.Fatalf("clone machine binding changed with original: %#v", clone.ClaudeDesktop.MachineProfileBindings)
	}

	clone.ClaudeDesktop.MachineProfiles[0].CPUModel = "mutated-clone"
	clone.ClaudeDesktop.MachineProfileBindings["auth-a"] = "mutated-clone"
	if cfg.ClaudeDesktop.MachineProfiles[0].CPUModel != "mutated-original" {
		t.Fatalf("original machine profile changed with clone: %#v", cfg.ClaudeDesktop.MachineProfiles)
	}
	if cfg.ClaudeDesktop.MachineProfileBindings["auth-a"] != "mutated-original" {
		t.Fatalf("original machine binding changed with clone: %#v", cfg.ClaudeDesktop.MachineProfileBindings)
	}
}
