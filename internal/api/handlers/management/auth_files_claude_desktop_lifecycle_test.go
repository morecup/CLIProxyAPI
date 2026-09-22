package management

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyAuthDisabledStateTransitionsClaudeDesktopLifecycle(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	if errDisable := applyAuthDisabledState(auth, true); errDisable != nil {
		t.Fatalf("disable error = %v", errDisable)
	}
	disabledEnrollment, errParse := claudedesktop.ParseEnrollment(auth.Metadata)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if disabledEnrollment.State != claudedesktop.EnrollmentDisabled || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("disabled auth = %+v enrollment=%+v", auth, disabledEnrollment)
	}
	if metadataDisabled, _ := auth.Metadata["disabled"].(bool); !metadataDisabled {
		t.Fatal("disabled metadata was not synchronized")
	}

	if errEnable := applyAuthDisabledState(auth, false); errEnable != nil {
		t.Fatalf("enable error = %v", errEnable)
	}
	activeEnrollment, errParse := claudedesktop.ParseEnrollment(auth.Metadata)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if activeEnrollment.State != claudedesktop.EnrollmentActive || auth.Disabled || auth.Status != coreauth.StatusActive || auth.StatusMessage != "" {
		t.Fatalf("enabled auth = %+v enrollment=%+v", auth, activeEnrollment)
	}
}

func TestApplyAuthDisabledStateRejectsQuarantinedClaudeDesktopEnable(t *testing.T) {
	auth := newManagementClaudeDesktopAuth(t)
	if _, errTransition := claudedesktop.TransitionMetadataEnrollment(
		auth.Metadata,
		auth.ID,
		claudedesktop.EnrollmentQuarantined,
		"runtime binding drift",
		time.Date(2026, 9, 4, 12, 1, 0, 0, time.UTC),
	); errTransition != nil {
		t.Fatal(errTransition)
	}
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	auth.StatusMessage = "held for operator review"
	auth.Metadata["disabled"] = true

	errEnable := applyAuthDisabledState(auth, false)
	if errEnable == nil || !strings.Contains(errEnable.Error(), "explicitly promote") {
		t.Fatalf("enable quarantine error = %v", errEnable)
	}
	enrollment, errParse := claudedesktop.ParseEnrollment(auth.Metadata)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if enrollment.State != claudedesktop.EnrollmentQuarantined || !auth.Disabled || auth.Status != coreauth.StatusDisabled || auth.StatusMessage != "held for operator review" {
		t.Fatalf("rejected enable mutated auth = %+v enrollment=%+v", auth, enrollment)
	}
}

func newManagementClaudeDesktopAuth(t *testing.T) *coreauth.Auth {
	t.Helper()
	identity := claudedesktop.AccountIdentity{
		AccountUUID:      "a1000000-0000-4000-8000-000000000001",
		OrganizationUUID: "b1000000-0000-4000-8000-000000000001",
	}
	device := claudedesktop.TrustedDevice{
		DeviceID:    "c1000000-0000-4000-8000-000000000001",
		DeviceToken: "synthetic-trusted-device-token",
		DisplayName: "Desktop",
	}
	authID, errAuthID := claudedesktop.StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := claudedesktop.NewEnrollment(authID, identity, device, time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	enrollment, errTransition := claudedesktop.TransitionEnrollment(enrollment, claudedesktop.EnrollmentActive, "", time.Date(2026, 9, 4, 12, 0, 1, 0, time.UTC))
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	return &coreauth.Auth{
		ID:       authID,
		Provider: claudedesktop.Provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"account_uuid":      identity.AccountUUID,
			"organization_uuid": identity.OrganizationUUID,
			"claude_device_ids": []string{claudedesktop.RequestDeviceID(device.DeviceID)},
		},
	}
}
