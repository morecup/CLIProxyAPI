package claudedesktop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	Provider                       = "claude"
	AuthFlowDesktop                = "desktop"
	OAuthClientID                  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	OAuthRedirectURI               = "https://console.anthropic.com/oauth/code/callback"
	OAuthScope                     = "user:inference user:file_upload user:profile"
	CoworkSessionsOAuthClientID    = OAuthClientID
	CoworkSessionsOAuthRedirectURI = OAuthRedirectURI
	CoworkSessionsOAuthScope       = "user:inference user:file_upload user:profile user:sessions:claude_code"
	DefaultAPIHost                 = "https://api.anthropic.com"
	DefaultClaudeOrigin            = "https://claude.ai"
	DefaultLoginURL                = "https://claude.ai/login?client=desktop"
	DefaultDesktopVersion          = "1.40609.0.0"
	EnrollmentSchemaVersion        = 1
	CredentialsEnvelopeVersion     = 3
	MetadataAuthFlowKey            = "auth_flow"
	MetadataEnrollmentKey          = "claude_desktop_enrollment"
	MetadataCredentialsKey         = "claude_desktop_credentials"
	MetadataSessionKeyKey          = "claude_desktop_session_key"
	MetadataTrustedDeviceTokenKey  = "claude_desktop_trusted_device_token"
	MetadataTelemetryMaterialsKey  = "claude_desktop_telemetry_materials"
)

type EnrollmentState string

const (
	EnrollmentProvisioning EnrollmentState = "provisioning"
	EnrollmentReady        EnrollmentState = "ready"
	EnrollmentActive       EnrollmentState = "active"
	EnrollmentDisabled     EnrollmentState = "disabled"
	EnrollmentQuarantined  EnrollmentState = "quarantined"
	EnrollmentRetired      EnrollmentState = "retired"
)

type AccountIdentity struct {
	AccountUUID      string
	Email            string
	OrganizationUUID string
	OrganizationName string
}

type DesktopSession struct {
	SessionKey string
	Identity   AccountIdentity
}

type TokenData struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Expire       string
}

type TrustedDevice struct {
	DeviceID    string
	DeviceToken string
	DisplayName string
}

// TelemetryMaterials contains the public ingestion credentials and runtime
// identifiers used by the four auxiliary Claude Desktop telemetry senders.
// They remain inside the same current-user encrypted envelope as OAuth and
// trusted-device credentials and are never serialized as plain metadata.
type TelemetryMaterials struct {
	SegmentWriteKey         string `json:"segment_write_key"`
	DatadogLogsAPIKey       string `json:"datadog_logs_api_key"`
	DatadogRUMClientToken   string `json:"datadog_rum_client_token"`
	DatadogRUMApplicationID string `json:"datadog_rum_application_id"`
	SentryPublicKey         string `json:"sentry_public_key"`
}

func (m TelemetryMaterials) Value(name string) string {
	switch strings.TrimSpace(name) {
	case "segment_write_key":
		return strings.TrimSpace(m.SegmentWriteKey)
	case "datadog_logs_api_key":
		return strings.TrimSpace(m.DatadogLogsAPIKey)
	case "datadog_rum_client_token":
		return strings.TrimSpace(m.DatadogRUMClientToken)
	case "datadog_rum_application_id":
		return strings.TrimSpace(m.DatadogRUMApplicationID)
	case "sentry_public_key":
		return strings.TrimSpace(m.SentryPublicKey)
	default:
		return ""
	}
}

func (m TelemetryMaterials) Validate() error {
	values := map[string]string{
		"segment_write_key":          m.SegmentWriteKey,
		"datadog_logs_api_key":       m.DatadogLogsAPIKey,
		"datadog_rum_client_token":   m.DatadogRUMClientToken,
		"datadog_rum_application_id": m.DatadogRUMApplicationID,
		"sentry_public_key":          m.SentryPublicKey,
	}
	for name, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("Claude Desktop telemetry material %q is missing", name)
		}
		if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("Claude Desktop telemetry material %q is invalid", name)
		}
		for _, character := range value {
			if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
				return fmt.Errorf("Claude Desktop telemetry material %q is invalid", name)
			}
		}
	}
	if _, errApplicationID := uuid.Parse(strings.TrimSpace(m.DatadogRUMApplicationID)); errApplicationID != nil {
		return fmt.Errorf("Claude Desktop telemetry material %q is invalid", "datadog_rum_application_id")
	}
	return nil
}

type Enrollment struct {
	Version          int             `json:"version"`
	State            EnrollmentState `json:"state"`
	AuthID           string          `json:"auth_id"`
	AccountUUID      string          `json:"account_uuid"`
	OrganizationUUID string          `json:"organization_uuid"`
	DeviceID         string          `json:"device_id"`
	DisplayName      string          `json:"display_name"`
	ProfileVersion   string          `json:"profile_version"`
	RegisteredAt     string          `json:"registered_at"`
	UpdatedAt        string          `json:"updated_at"`
	QuarantineReason string          `json:"quarantine_reason,omitempty"`
}

func StableAuthID(accountUUID, organizationUUID string) (string, error) {
	accountUUID = strings.ToLower(strings.TrimSpace(accountUUID))
	organizationUUID = strings.ToLower(strings.TrimSpace(organizationUUID))
	if _, errParse := uuid.Parse(accountUUID); errParse != nil {
		return "", fmt.Errorf("Claude Desktop auth id: invalid account UUID: %w", errParse)
	}
	if _, errParse := uuid.Parse(organizationUUID); errParse != nil {
		return "", fmt.Errorf("Claude Desktop auth id: invalid organization UUID: %w", errParse)
	}
	digest := sha256.Sum256([]byte(Provider + "\x00" + AuthFlowDesktop + "\x00" + accountUUID + "\x00" + organizationUUID))
	return "claude-desktop-" + hex.EncodeToString(digest[:12]) + ".json", nil
}

// RequestDeviceID derives the stable 64-hex identity carried by Desktop API
// requests from the server-issued trusted-device UUID.
func RequestDeviceID(deviceID string) string {
	digest := sha256.Sum256([]byte("claude-desktop-device\x00" + strings.ToLower(strings.TrimSpace(deviceID))))
	return hex.EncodeToString(digest[:])
}

func NewEnrollment(authID string, identity AccountIdentity, device TrustedDevice, now time.Time) Enrollment {
	if now.IsZero() {
		now = time.Now()
	}
	timestamp := now.UTC().Format(time.RFC3339)
	return Enrollment{
		Version:          EnrollmentSchemaVersion,
		State:            EnrollmentReady,
		AuthID:           strings.TrimSpace(authID),
		AccountUUID:      strings.ToLower(strings.TrimSpace(identity.AccountUUID)),
		OrganizationUUID: strings.ToLower(strings.TrimSpace(identity.OrganizationUUID)),
		DeviceID:         strings.ToLower(strings.TrimSpace(device.DeviceID)),
		DisplayName:      strings.TrimSpace(device.DisplayName),
		ProfileVersion:   DefaultDesktopVersion,
		RegisteredAt:     timestamp,
		UpdatedAt:        timestamp,
	}
}

func ParseEnrollment(metadata map[string]any) (Enrollment, error) {
	if metadata == nil {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment metadata is missing")
	}
	raw, ok := metadata[MetadataEnrollmentKey]
	if !ok || raw == nil {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment metadata is missing")
	}
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return Enrollment{}, fmt.Errorf("marshal Claude Desktop enrollment metadata: %w", errMarshal)
	}
	var enrollment Enrollment
	if errUnmarshal := json.Unmarshal(encoded, &enrollment); errUnmarshal != nil {
		return Enrollment{}, fmt.Errorf("parse Claude Desktop enrollment metadata: %w", errUnmarshal)
	}
	return enrollment, nil
}

func ValidateEnrollment(authID string, metadata map[string]any) (Enrollment, error) {
	enrollment, errValidate := ValidateEnrollmentBinding(authID, metadata)
	if errValidate != nil {
		return Enrollment{}, errValidate
	}
	if enrollment.State != EnrollmentReady && enrollment.State != EnrollmentActive {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment state is %q", enrollment.State)
	}
	return enrollment, nil
}

// ValidateActiveEnrollment validates the immutable Desktop account/device
// binding and requires the virtual application runtime to be active.
func ValidateActiveEnrollment(authID string, metadata map[string]any) (Enrollment, error) {
	enrollment, errValidate := ValidateEnrollmentBinding(authID, metadata)
	if errValidate != nil {
		return Enrollment{}, errValidate
	}
	if enrollment.State != EnrollmentActive {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment state is %q", enrollment.State)
	}
	return enrollment, nil
}

// ValidateEnrollmentBinding validates stable identity fields for every
// lifecycle state. Scheduling decisions must additionally require active.
func ValidateEnrollmentBinding(authID string, metadata map[string]any) (Enrollment, error) {
	if flow, _ := metadata[MetadataAuthFlowKey].(string); !strings.EqualFold(strings.TrimSpace(flow), AuthFlowDesktop) {
		return Enrollment{}, fmt.Errorf("credential was not acquired through Claude Desktop login")
	}
	enrollment, errParse := ParseEnrollment(metadata)
	if errParse != nil {
		return Enrollment{}, errParse
	}
	if enrollment.Version != EnrollmentSchemaVersion {
		return Enrollment{}, fmt.Errorf("unsupported Claude Desktop enrollment version %d", enrollment.Version)
	}
	switch enrollment.State {
	case EnrollmentProvisioning, EnrollmentReady, EnrollmentActive, EnrollmentDisabled, EnrollmentQuarantined, EnrollmentRetired:
	default:
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment state is %q", enrollment.State)
	}
	if strings.TrimSpace(authID) == "" || enrollment.AuthID != strings.TrimSpace(authID) {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment auth binding does not match credential")
	}
	if _, errAccount := uuid.Parse(enrollment.AccountUUID); errAccount != nil {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment account binding is invalid")
	}
	if _, errOrganization := uuid.Parse(enrollment.OrganizationUUID); errOrganization != nil {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment organization binding is invalid")
	}
	if _, errDevice := uuid.Parse(enrollment.DeviceID); errDevice != nil {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment device binding is invalid")
	}
	expectedAuthID, errAuthID := StableAuthID(enrollment.AccountUUID, enrollment.OrganizationUUID)
	if errAuthID != nil || enrollment.AuthID != expectedAuthID {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment stable auth binding is invalid")
	}
	accountUUID, _ := metadata["account_uuid"].(string)
	organizationUUID, _ := metadata["organization_uuid"].(string)
	if !strings.EqualFold(strings.TrimSpace(accountUUID), enrollment.AccountUUID) {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment account binding changed")
	}
	if !strings.EqualFold(strings.TrimSpace(organizationUUID), enrollment.OrganizationUUID) {
		return Enrollment{}, fmt.Errorf("Claude Desktop enrollment organization binding changed")
	}
	if !matchesRequestDeviceID(metadata["claude_device_ids"], RequestDeviceID(enrollment.DeviceID)) {
		return Enrollment{}, fmt.Errorf("Claude Desktop request device binding changed")
	}
	return enrollment, nil
}

// ValidateTrustedDeviceEnrollment validates an enrollment for Desktop remote
// control. Ordinary Messages API inference only needs the stable account and
// local device binding; remote control additionally requires the server-issued
// trusted-device credential.
func ValidateTrustedDeviceEnrollment(authID string, metadata map[string]any) (Enrollment, error) {
	enrollment, errValidate := ValidateEnrollment(authID, metadata)
	if errValidate != nil {
		return Enrollment{}, errValidate
	}
	trustedDeviceToken, _ := metadata[MetadataTrustedDeviceTokenKey].(string)
	if strings.TrimSpace(trustedDeviceToken) == "" {
		return Enrollment{}, fmt.Errorf("Claude Desktop trusted-device credential is missing")
	}
	return enrollment, nil
}

// TransitionMetadataEnrollment applies a lifecycle transition to the
// enrollment embedded in credential metadata without touching secret fields.
func TransitionMetadataEnrollment(metadata map[string]any, authID string, to EnrollmentState, reason string, now time.Time) (Enrollment, error) {
	enrollment, errValidate := ValidateEnrollmentBinding(authID, metadata)
	if errValidate != nil {
		return Enrollment{}, errValidate
	}
	next, errTransition := TransitionEnrollment(enrollment, to, reason, now)
	if errTransition != nil {
		return Enrollment{}, errTransition
	}
	metadata[MetadataEnrollmentKey] = next
	return next, nil
}

func matchesRequestDeviceID(raw any, expected string) bool {
	switch values := raw.(type) {
	case []string:
		return len(values) == 1 && values[0] == expected
	case []any:
		if len(values) != 1 {
			return false
		}
		value, ok := values[0].(string)
		return ok && value == expected
	default:
		return false
	}
}

func CanTransitionEnrollment(from, to EnrollmentState) bool {
	if from == to {
		return true
	}
	switch from {
	case EnrollmentProvisioning:
		return to == EnrollmentReady || to == EnrollmentQuarantined || to == EnrollmentRetired
	case EnrollmentReady:
		return to == EnrollmentActive || to == EnrollmentDisabled || to == EnrollmentQuarantined || to == EnrollmentRetired
	case EnrollmentActive:
		return to == EnrollmentDisabled || to == EnrollmentQuarantined || to == EnrollmentRetired
	case EnrollmentDisabled:
		return to == EnrollmentReady || to == EnrollmentRetired
	case EnrollmentQuarantined:
		return to == EnrollmentProvisioning || to == EnrollmentRetired
	case EnrollmentRetired:
		return false
	default:
		return false
	}
}

func TransitionEnrollment(enrollment Enrollment, to EnrollmentState, reason string, now time.Time) (Enrollment, error) {
	if !CanTransitionEnrollment(enrollment.State, to) {
		return Enrollment{}, fmt.Errorf("invalid Claude Desktop enrollment transition %q -> %q", enrollment.State, to)
	}
	if now.IsZero() {
		now = time.Now()
	}
	enrollment.State = to
	enrollment.UpdatedAt = now.UTC().Format(time.RFC3339)
	if to == EnrollmentQuarantined {
		enrollment.QuarantineReason = strings.TrimSpace(reason)
	} else {
		enrollment.QuarantineReason = ""
	}
	return enrollment, nil
}
