package claudedesktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	testAccountUUID = "11111111-1111-4111-8111-111111111111"
	testOrgAUUID    = "22222222-2222-4222-8222-222222222222"
	testOrgBUUID    = "33333333-3333-4333-8333-333333333333"
	testDeviceUUID  = "44444444-4444-4444-8444-444444444444"
)

func TestServiceLoginUsesDesktopOAuthAndEnrollsTrustedDevice(t *testing.T) {
	authorizeStates := make(map[string]string)
	var authorizeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/oauth/" + testOrgAUUID + "/authorize":
			if request.Header.Get("Authorization") != "Bearer browser-session" {
				t.Errorf("authorize Authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("anthropic-version") != "2023-06-01" {
				t.Errorf("authorize anthropic-version = %q", request.Header.Get("anthropic-version"))
			}
			if request.Header.Get("anthropic-client-platform") != "desktop_app" {
				t.Errorf("authorize platform = %q", request.Header.Get("anthropic-client-platform"))
			}
			if request.Header.Get("anthropic-client-version") != DefaultOAuthClientVersion {
				t.Errorf("authorize version = %q", request.Header.Get("anthropic-client-version"))
			}
			var body authorizeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			if body.ResponseType != "code" || body.ClientID != OAuthClientID || body.OrganizationUUID != testOrgAUUID || body.RedirectURI != OAuthRedirectURI {
				t.Errorf("unexpected authorize body: %+v", body)
			}
			if body.CodeChallengeMethod != "S256" || body.CodeChallenge == "" || body.State == "" {
				t.Errorf("authorize PKCE fields are incomplete: %+v", body)
			}
			authorizeCalls.Add(1)
			code := ""
			switch body.Scope {
			case OAuthScope:
				code = "desktop-code"
			case CoworkSessionsOAuthScope:
				code = "sessions-code"
			default:
				t.Errorf("unexpected authorize scope %q", body.Scope)
				http.Error(w, "unexpected scope", http.StatusBadRequest)
				return
			}
			authorizeStates[code] = body.State
			writeTestJSON(w, map[string]any{"redirect_uri": OAuthRedirectURI + "?code=" + code + "&state=" + body.State})

		case "/v1/oauth/token":
			if request.Header.Get("Authorization") != "Bearer browser-session" {
				t.Errorf("token Authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("anthropic-version") != "2023-06-01" {
				t.Errorf("token anthropic-version = %q", request.Header.Get("anthropic-version"))
			}
			if request.Header.Get("anthropic-client-platform") != "" {
				t.Errorf("token unexpectedly inherited Desktop platform header")
			}
			var body authorizationCodeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			if body.GrantType != "authorization_code" || body.ClientID != OAuthClientID || body.RedirectURI != OAuthRedirectURI || body.State != authorizeStates[body.Code] || body.CodeVerifier == "" {
				t.Errorf("unexpected token body: %+v", body)
			}
			expectedLifetime := desktopTokenLifetime
			response := testTokenResponse(testOrgAUUID)
			if body.Code == "sessions-code" {
				expectedLifetime = coworkSessionsTokenLifetime
				response["access_token"] = "sessions-access"
				response["refresh_token"] = "sessions-refresh"
			}
			if body.ExpiresIn != int64(expectedLifetime/time.Second) {
				t.Errorf("token expires_in = %d", body.ExpiresIn)
			}
			writeTestJSON(w, response)

		case "/api/auth/trusted_devices":
			if request.Header.Get("Authorization") != "Bearer sessions-access" {
				t.Errorf("enrollment Authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("anthropic-version") != "" || request.Header.Get("anthropic-client-platform") != "" {
				t.Errorf("enrollment inherited OAuth-only headers: %v", request.Header)
			}
			var body struct {
				DisplayName string `json:"display_name"`
			}
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			if !strings.HasPrefix(body.DisplayName, "Claude Desktop on ") {
				t.Errorf("display_name = %q", body.DisplayName)
			}
			writeTestJSON(w, map[string]any{"device_id": testDeviceUUID, "device_token": "trusted-device"})

		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	service := NewService(&config.Config{AuthDir: t.TempDir()})
	service.apiHost = server.URL
	service.httpClient = server.Client()
	service.now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	service.acquire = func(context.Context, *http.Client, string, MagicLinkLoginOptions) (*DesktopSession, error) {
		return &DesktopSession{SessionKey: "browser-session", Identity: AccountIdentity{OrganizationUUID: testOrgAUUID}}, nil
	}
	service.resolveTelemetryMaterials = func(context.Context, string) (TelemetryMaterials, error) {
		return testTelemetryMaterials(), nil
	}
	service.reuseDevice = func(AccountIdentity) (TrustedDevice, Enrollment, bool, error) {
		return TrustedDevice{}, Enrollment{}, false, nil
	}

	result, errLogin := service.Login(context.Background(), MagicLinkLoginOptions{Timeout: time.Minute})
	if errLogin != nil {
		t.Fatalf("Login() error = %v", errLogin)
	}
	if result.DeviceReused {
		t.Fatal("new enrollment was marked as reused")
	}
	if authorizeCalls.Load() != 2 {
		t.Fatalf("authorize calls = %d, want 2", authorizeCalls.Load())
	}
	if result.Token.AccessToken != "sessions-access" || result.Token.RefreshToken != "sessions-refresh" {
		t.Fatalf("unexpected token result: %+v", result.Token)
	}
	if result.Identity.AccountUUID != testAccountUUID || result.Identity.OrganizationUUID != testOrgAUUID {
		t.Fatalf("unexpected identity: %+v", result.Identity)
	}
	if result.Device.DeviceID != testDeviceUUID || result.Device.DeviceToken != "trusted-device" {
		t.Fatalf("unexpected device: %+v", result.Device)
	}
	if result.TelemetryMaterials != testTelemetryMaterials() {
		t.Fatal("login did not attach the resolved telemetry materials")
	}
	if result.Enrollment.ProfileVersion != DefaultDesktopVersion {
		t.Fatalf("enrollment profile version = %q, want historical runtime profile %q", result.Enrollment.ProfileVersion, DefaultDesktopVersion)
	}
	if _, errValidate := ValidateEnrollment(result.AuthID, MetadataFromLogin(result)); errValidate != nil {
		t.Fatalf("new enrollment validation failed: %v", errValidate)
	}
}

func TestServiceAuthorizeCoworkSessionsUsesSessionsOAuthProfile(t *testing.T) {
	var authorizeState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/oauth/" + testOrgAUUID + "/authorize":
			var body authorizeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			if body.ClientID != CoworkSessionsOAuthClientID || body.RedirectURI != CoworkSessionsOAuthRedirectURI || body.Scope != CoworkSessionsOAuthScope {
				t.Errorf("unexpected Cowork Sessions authorize body: %+v", body)
			}
			authorizeState = body.State
			writeTestJSON(w, map[string]any{"redirect_uri": CoworkSessionsOAuthRedirectURI + "?code=sessions-code&state=" + body.State})
		case "/v1/oauth/token":
			var body authorizationCodeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			if body.ClientID != CoworkSessionsOAuthClientID || body.RedirectURI != CoworkSessionsOAuthRedirectURI || body.Code != "sessions-code" || body.State != authorizeState {
				t.Errorf("unexpected Cowork Sessions token body: %+v", body)
			}
			if body.ExpiresIn != int64(coworkSessionsTokenLifetime/time.Second) {
				t.Errorf("Cowork Sessions token expires_in = %d", body.ExpiresIn)
			}
			response := testTokenResponse(testOrgAUUID)
			response["access_token"] = "sessions-access"
			response["refresh_token"] = "sessions-refresh"
			writeTestJSON(w, response)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	service := NewService(nil)
	service.apiHost = server.URL
	service.httpClient = server.Client()
	token, identity, errAuthorize := service.AuthorizeCoworkSessions(context.Background(), DesktopSession{
		SessionKey: "browser-session",
		Identity:   AccountIdentity{OrganizationUUID: testOrgAUUID},
	})
	if errAuthorize != nil {
		t.Fatalf("AuthorizeCoworkSessions() error = %v", errAuthorize)
	}
	if token.AccessToken != "sessions-access" || token.RefreshToken != "sessions-refresh" {
		t.Fatalf("unexpected Cowork Sessions token: %+v", token)
	}
	if identity.AccountUUID != testAccountUUID || identity.OrganizationUUID != testOrgAUUID {
		t.Fatalf("unexpected Cowork Sessions identity: %+v", identity)
	}
}

func TestServiceLoginStoresCoworkSessionsTokenForTrustedDevice(t *testing.T) {
	var authorizeCalls atomic.Int32
	var enrollmentCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/oauth/" + testOrgAUUID + "/authorize":
			var body authorizeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			authorizeCalls.Add(1)
			if body.ClientID != OAuthClientID || body.RedirectURI != OAuthRedirectURI {
				t.Errorf("unexpected OAuth profile: %+v", body)
				http.Error(w, "unexpected OAuth profile", http.StatusBadRequest)
				return
			}
			switch body.Scope {
			case OAuthScope:
				writeTestJSON(w, map[string]any{"redirect_uri": OAuthRedirectURI + "?code=desktop-code&state=" + body.State})
			case CoworkSessionsOAuthScope:
				writeTestJSON(w, map[string]any{"redirect_uri": CoworkSessionsOAuthRedirectURI + "?code=sessions-code&state=" + body.State})
			default:
				t.Errorf("unexpected OAuth scope %q", body.Scope)
				http.Error(w, "unexpected OAuth scope", http.StatusBadRequest)
			}
		case "/v1/oauth/token":
			var body authorizationCodeRequest
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
				http.Error(w, errDecode.Error(), http.StatusBadRequest)
				return
			}
			response := testTokenResponse(testOrgAUUID)
			if body.Code == "sessions-code" {
				response["access_token"] = "sessions-access"
				response["refresh_token"] = "sessions-refresh"
			}
			writeTestJSON(w, response)
		case "/api/auth/trusted_devices":
			enrollmentCalls.Add(1)
			switch request.Header.Get("Authorization") {
			case "Bearer sessions-access":
				writeTestJSON(w, map[string]any{"device_id": testDeviceUUID, "device_token": "trusted-device"})
			default:
				http.Error(w, "unexpected enrollment token", http.StatusUnauthorized)
			}
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	service := NewService(&config.Config{AuthDir: t.TempDir()})
	service.apiHost = server.URL
	service.httpClient = server.Client()
	service.acquire = func(context.Context, *http.Client, string, MagicLinkLoginOptions) (*DesktopSession, error) {
		return &DesktopSession{SessionKey: "browser-session", Identity: AccountIdentity{OrganizationUUID: testOrgAUUID}}, nil
	}
	service.resolveTelemetryMaterials = func(context.Context, string) (TelemetryMaterials, error) {
		return TelemetryMaterials{}, nil
	}
	service.reuseDevice = func(AccountIdentity) (TrustedDevice, Enrollment, bool, error) {
		return TrustedDevice{}, Enrollment{}, false, nil
	}

	result, errLogin := service.Login(context.Background(), MagicLinkLoginOptions{Timeout: time.Minute})
	if errLogin != nil {
		t.Fatalf("Login() error = %v", errLogin)
	}
	if authorizeCalls.Load() != 2 || enrollmentCalls.Load() != 1 {
		t.Fatalf("authorize calls = %d, enrollment calls = %d", authorizeCalls.Load(), enrollmentCalls.Load())
	}
	if result.Token.AccessToken != "sessions-access" || result.Token.RefreshToken != "sessions-refresh" {
		t.Fatalf("login did not retain the Cowork Sessions token: %+v", result.Token)
	}
	if result.Device.DeviceToken != "trusted-device" {
		t.Fatalf("unexpected fallback device: %+v", result.Device)
	}
}

func TestServiceLoginReusesAccountDeviceAcrossOrganizations(t *testing.T) {
	authDir := t.TempDir()
	registeredAt := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	oldIdentity := AccountIdentity{AccountUUID: testAccountUUID, Email: "desktop@example.com", OrganizationUUID: testOrgAUUID}
	oldDevice := TrustedDevice{DeviceID: testDeviceUUID, DeviceToken: "persisted-device", DisplayName: "Claude Desktop persisted"}
	oldAuthID, errAuthID := StableAuthID(oldIdentity.AccountUUID, oldIdentity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	oldResult := &LoginResult{
		AuthID: oldAuthID,
		Token: TokenData{
			AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresIn: 3600,
			Expire: registeredAt.Add(time.Hour).Format(time.RFC3339),
		},
		Identity:   oldIdentity,
		Enrollment: NewEnrollment(oldAuthID, oldIdentity, oldDevice, registeredAt),
		Device:     oldDevice,
	}
	if errSave := SaveMetadataFile(filepath.Join(authDir, oldAuthID), MetadataFromLogin(oldResult)); errSave != nil {
		t.Fatalf("save existing Desktop credential: %v", errSave)
	}

	var enrollmentCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/oauth/" + testOrgBUUID + "/authorize":
			var body authorizeRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			writeTestJSON(w, map[string]any{"redirect_uri": OAuthRedirectURI + "?code=relogin-code&state=" + body.State})
		case "/v1/oauth/token":
			writeTestJSON(w, testTokenResponse(testOrgBUUID))
		case "/api/auth/trusted_devices":
			enrollmentCalls.Add(1)
			writeTestJSON(w, map[string]any{"device_id": "55555555-5555-4555-8555-555555555555", "device_token": "new-device"})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	service := NewService(&config.Config{AuthDir: authDir})
	service.apiHost = server.URL
	service.httpClient = server.Client()
	service.now = func() time.Time { return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) }
	service.acquire = func(context.Context, *http.Client, string, MagicLinkLoginOptions) (*DesktopSession, error) {
		return &DesktopSession{SessionKey: "browser-session", Identity: AccountIdentity{OrganizationUUID: testOrgBUUID}}, nil
	}
	service.resolveTelemetryMaterials = func(context.Context, string) (TelemetryMaterials, error) {
		return testTelemetryMaterials(), nil
	}

	result, errLogin := service.Login(context.Background(), MagicLinkLoginOptions{Timeout: time.Minute})
	if errLogin != nil {
		t.Fatalf("Login() error = %v", errLogin)
	}
	if !result.DeviceReused {
		t.Fatal("existing account device was not reused")
	}
	if enrollmentCalls.Load() != 0 {
		t.Fatalf("trusted-device endpoint called %d times", enrollmentCalls.Load())
	}
	if result.Device.DeviceID != testDeviceUUID || result.Device.DeviceToken != "persisted-device" {
		t.Fatalf("unexpected reused device: %+v", result.Device)
	}
	if result.Enrollment.OrganizationUUID != testOrgBUUID || result.Enrollment.RegisteredAt != registeredAt.Format(time.RFC3339) {
		t.Fatalf("unexpected rebound enrollment: %+v", result.Enrollment)
	}
	if _, errValidate := ValidateEnrollment(result.AuthID, MetadataFromLogin(result)); errValidate != nil {
		t.Fatalf("reused enrollment validation failed: %v", errValidate)
	}
}

func testTelemetryMaterials() TelemetryMaterials {
	return TelemetryMaterials{
		SegmentWriteKey:         "segment0123456789abcdef01234567",
		DatadogLogsAPIKey:       "datadog-logs-0123456789abcdef012345",
		DatadogRUMClientToken:   "datadog-rum-0123456789abcdef0123456",
		DatadogRUMApplicationID: "55555555-5555-4555-8555-555555555555",
		SentryPublicKey:         "abcdef0123456789abcdef0123456789",
	}
}

func TestServiceRefreshUsesCoworkSessionsOAuthProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, request)
			return
		}
		if request.Header.Get("Authorization") != "" {
			t.Errorf("refresh unexpectedly sent Authorization header")
		}
		if request.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("refresh anthropic-version = %q", request.Header.Get("anthropic-version"))
		}
		var body refreshTokenRequest
		if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
			http.Error(w, errDecode.Error(), http.StatusBadRequest)
			return
		}
		if body.GrantType != "refresh_token" || body.ClientID != CoworkSessionsOAuthClientID || body.RefreshToken != "unique-refresh-token" || body.Scope != CoworkSessionsOAuthScope {
			t.Errorf("unexpected refresh body: %+v", body)
		}
		if body.ExpiresIn != int64(coworkSessionsTokenLifetime/time.Second) {
			t.Errorf("refresh expires_in = %d", body.ExpiresIn)
		}
		writeTestJSON(w, map[string]any{"access_token": "refreshed-access", "expires_in": 7200})
	}))
	defer server.Close()

	service := NewService(nil)
	service.apiHost = server.URL
	service.httpClient = server.Client()
	service.now = func() time.Time { return time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC) }
	token, errRefresh := service.Refresh(context.Background(), "unique-refresh-token")
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if token.AccessToken != "refreshed-access" || token.RefreshToken != "unique-refresh-token" || token.ExpiresIn != 7200 {
		t.Fatalf("unexpected refreshed token: %+v", token)
	}
}

func TestServiceRefreshRetriesWithoutCustomExpiryWhenRejected(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil {
			http.Error(w, errDecode.Error(), http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		if call == 1 {
			if _, exists := body["expires_in"]; !exists {
				t.Error("first refresh attempt omitted expires_in")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("expires_in is not supported"))
			return
		}
		if _, exists := body["expires_in"]; exists {
			t.Error("fallback refresh retained expires_in")
		}
		writeTestJSON(w, map[string]any{"access_token": "fallback-access", "refresh_token": "fallback-refresh", "expires_in": 3600})
	}))
	defer server.Close()

	service := NewService(nil)
	service.apiHost = server.URL
	service.httpClient = server.Client()
	token, errRefresh := service.Refresh(context.Background(), "expiry-rejection-refresh-token")
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if calls.Load() != 2 || token.AccessToken != "fallback-access" {
		t.Fatalf("calls = %d, token = %+v", calls.Load(), token)
	}
}

func TestGeneratePKCEMatchesDesktopShape(t *testing.T) {
	verifier, challenge, state, errGenerate := generatePKCE()
	if errGenerate != nil {
		t.Fatal(errGenerate)
	}
	if len(verifier) != 43 || len(challenge) != 43 || len(state) != 32 {
		t.Fatalf("PKCE lengths = verifier:%d challenge:%d state:%d", len(verifier), len(challenge), len(state))
	}
	for _, value := range state {
		if (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') {
			t.Fatalf("state contains non-letter %q", value)
		}
	}
}

func testTokenResponse(organizationUUID string) map[string]any {
	return map[string]any{
		"access_token":  "desktop-access",
		"refresh_token": "desktop-refresh",
		"expires_in":    3600,
		"account": map[string]any{
			"uuid":  testAccountUUID,
			"email": "desktop@example.com",
		},
		"organization": map[string]any{
			"uuid": organizationUUID,
			"name": fmt.Sprintf("Organization %s", organizationUUID[:8]),
		},
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
