package claudedesktop

import (
	"strings"
	"testing"
	"time"
)

func TestValidateEnrollmentRejectsBindingDrift(t *testing.T) {
	identity := AccountIdentity{AccountUUID: testAccountUUID, OrganizationUUID: testOrgAUUID}
	device := TrustedDevice{DeviceID: testDeviceUUID, DeviceToken: "trusted-device", DisplayName: "Desktop"}
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := NewEnrollment(authID, identity, device, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	metadata := map[string]any{
		MetadataAuthFlowKey:           AuthFlowDesktop,
		MetadataEnrollmentKey:         enrollment,
		MetadataTrustedDeviceTokenKey: device.DeviceToken,
		"account_uuid":                identity.AccountUUID,
		"organization_uuid":           identity.OrganizationUUID,
		"claude_device_ids":           []string{RequestDeviceID(device.DeviceID)},
	}
	if _, errValidate := ValidateEnrollment(authID, metadata); errValidate != nil {
		t.Fatalf("valid enrollment rejected: %v", errValidate)
	}

	driftedAccount := cloneTestMetadata(metadata)
	driftedAccount["account_uuid"] = "55555555-5555-4555-8555-555555555555"
	if _, errValidate := ValidateEnrollment(authID, driftedAccount); errValidate == nil || !strings.Contains(errValidate.Error(), "account binding changed") {
		t.Fatalf("account drift error = %v", errValidate)
	}

	forgedEnrollment := enrollment
	forgedEnrollment.AuthID = "claude-desktop-forged.json"
	forged := cloneTestMetadata(metadata)
	forged[MetadataEnrollmentKey] = forgedEnrollment
	if _, errValidate := ValidateEnrollment(forgedEnrollment.AuthID, forged); errValidate == nil || !strings.Contains(errValidate.Error(), "stable auth binding") {
		t.Fatalf("forged auth binding error = %v", errValidate)
	}

	driftedDevice := cloneTestMetadata(metadata)
	driftedDevice["claude_device_ids"] = []string{"0000000000000000000000000000000000000000000000000000000000000000"}
	if _, errValidate := ValidateEnrollment(authID, driftedDevice); errValidate == nil || !strings.Contains(errValidate.Error(), "request device binding changed") {
		t.Fatalf("request device drift error = %v", errValidate)
	}
}

func TestEnrollmentTransitions(t *testing.T) {
	enrollment := Enrollment{State: EnrollmentProvisioning}
	ready, errReady := TransitionEnrollment(enrollment, EnrollmentReady, "", time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC))
	if errReady != nil || ready.State != EnrollmentReady {
		t.Fatalf("provisioning -> ready = %+v, %v", ready, errReady)
	}
	quarantined, errQuarantine := TransitionEnrollment(ready, EnrollmentQuarantined, "binding drift", time.Date(2026, 9, 2, 11, 1, 0, 0, time.UTC))
	if errQuarantine != nil || quarantined.QuarantineReason != "binding drift" {
		t.Fatalf("ready -> quarantined = %+v, %v", quarantined, errQuarantine)
	}
	if _, errInvalid := TransitionEnrollment(quarantined, EnrollmentActive, "", time.Now()); errInvalid == nil {
		t.Fatal("quarantined -> active unexpectedly succeeded")
	}
}

func TestValidateActiveEnrollmentRequiresActiveState(t *testing.T) {
	identity := AccountIdentity{AccountUUID: testAccountUUID, OrganizationUUID: testOrgAUUID}
	device := TrustedDevice{DeviceID: testDeviceUUID, DeviceToken: "trusted-device", DisplayName: "Desktop"}
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := NewEnrollment(authID, identity, device, time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC))
	metadata := map[string]any{
		MetadataAuthFlowKey:           AuthFlowDesktop,
		MetadataEnrollmentKey:         enrollment,
		MetadataTrustedDeviceTokenKey: device.DeviceToken,
		"account_uuid":                identity.AccountUUID,
		"organization_uuid":           identity.OrganizationUUID,
		"claude_device_ids":           []string{RequestDeviceID(device.DeviceID)},
	}
	if _, errValidate := ValidateActiveEnrollment(authID, metadata); errValidate == nil || !strings.Contains(errValidate.Error(), "ready") {
		t.Fatalf("ValidateActiveEnrollment(ready) error = %v", errValidate)
	}
	active, errTransition := TransitionMetadataEnrollment(metadata, authID, EnrollmentActive, "", time.Date(2026, 9, 4, 10, 0, 1, 0, time.UTC))
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	if active.State != EnrollmentActive {
		t.Fatalf("transitioned state = %q", active.State)
	}
	validated, errValidate := ValidateActiveEnrollment(authID, metadata)
	if errValidate != nil || validated.State != EnrollmentActive {
		t.Fatalf("ValidateActiveEnrollment(active) = %+v, %v", validated, errValidate)
	}
	delete(metadata, MetadataTrustedDeviceTokenKey)
	validated, errValidate = ValidateActiveEnrollment(authID, metadata)
	if errValidate != nil || validated.State != EnrollmentActive {
		t.Fatalf("ValidateActiveEnrollment(active without trusted device) = %+v, %v", validated, errValidate)
	}
	if _, errTrusted := ValidateTrustedDeviceEnrollment(authID, metadata); errTrusted == nil || !strings.Contains(errTrusted.Error(), "trusted-device credential is missing") {
		t.Fatalf("ValidateTrustedDeviceEnrollment() error = %v", errTrusted)
	}
}

func TestTransitionMetadataEnrollmentPreservesStableBinding(t *testing.T) {
	identity := AccountIdentity{AccountUUID: testAccountUUID, OrganizationUUID: testOrgAUUID}
	device := TrustedDevice{DeviceID: testDeviceUUID, DeviceToken: "trusted-device", DisplayName: "Desktop"}
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	original := NewEnrollment(authID, identity, device, time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC))
	metadata := map[string]any{
		MetadataAuthFlowKey:           AuthFlowDesktop,
		MetadataEnrollmentKey:         original,
		MetadataTrustedDeviceTokenKey: device.DeviceToken,
		"account_uuid":                identity.AccountUUID,
		"organization_uuid":           identity.OrganizationUUID,
		"claude_device_ids":           []string{RequestDeviceID(device.DeviceID)},
	}
	active, errTransition := TransitionMetadataEnrollment(metadata, authID, EnrollmentActive, "", time.Date(2026, 9, 4, 11, 1, 0, 0, time.UTC))
	if errTransition != nil {
		t.Fatal(errTransition)
	}
	if active.AuthID != original.AuthID || active.AccountUUID != original.AccountUUID || active.OrganizationUUID != original.OrganizationUUID || active.DeviceID != original.DeviceID || active.RegisteredAt != original.RegisteredAt {
		t.Fatalf("lifecycle transition changed stable binding: before=%+v after=%+v", original, active)
	}
	parsed, errParse := ParseEnrollment(metadata)
	if errParse != nil || parsed != active {
		t.Fatalf("metadata enrollment = %+v, %v; want %+v", parsed, errParse, active)
	}
}

func cloneTestMetadata(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
