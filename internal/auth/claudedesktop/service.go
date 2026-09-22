package claudedesktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	credentialRequestTimeout    = 30 * time.Second
	trustedDeviceTimeout        = 10 * time.Second
	desktopTokenLifetime        = 365 * 24 * time.Hour
	coworkSessionsTokenLifetime = 30 * 24 * time.Hour
)

type requestHeaderProfile int

const (
	requestHeadersOAuthToken requestHeaderProfile = iota
	requestHeadersOAuthAuthorize
	requestHeadersTrustedDevice
)

var desktopRefreshGroup singleflight.Group

type Service struct {
	httpClient                *http.Client
	apiHost                   string
	claudeOrigin              string
	loginURL                  string
	appVersion                string
	now                       func() time.Time
	acquire                   func(context.Context, *http.Client, string, MagicLinkLoginOptions) (*DesktopSession, error)
	reuseDevice               func(AccountIdentity) (TrustedDevice, Enrollment, bool, error)
	resolveTelemetryMaterials func(context.Context, string) (TelemetryMaterials, error)
}

type MagicLinkLoginOptions struct {
	MagicLink string
	Locale    string
	Prompt    func(string) (string, error)
	Timeout   time.Duration
}

type LoginResult struct {
	AuthID             string
	SessionKey         string
	Token              TokenData
	Identity           AccountIdentity
	Enrollment         Enrollment
	Device             TrustedDevice
	TelemetryMaterials TelemetryMaterials
	DeviceReused       bool
}

type authorizeRequest struct {
	ResponseType        string `json:"response_type"`
	ClientID            string `json:"client_id"`
	OrganizationUUID    string `json:"organization_uuid"`
	RedirectURI         string `json:"redirect_uri"`
	Scope               string `json:"scope"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

type oauthAuthorizationConfig struct {
	ClientID      string
	RedirectURI   string
	Scope         string
	TokenLifetime time.Duration
}

type authorizationCodeRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

type refreshTokenRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		UUID         string `json:"uuid"`
		Email        string `json:"email"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

func NewService(cfg *config.Config) *Service {
	return NewServiceWithProxyURL(cfg, "")
}

func NewServiceWithProxyURL(cfg *config.Config, proxyURL string) *Service {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	authDir := ""
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		authDir = cfg.AuthDir
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	client := util.SetProxy(&sdkCfg, &http.Client{})
	return &Service{
		httpClient:                client,
		apiHost:                   DefaultAPIHost,
		claudeOrigin:              DefaultClaudeOrigin,
		loginURL:                  DefaultLoginURL,
		appVersion:                DefaultDesktopVersion,
		now:                       time.Now,
		acquire:                   AcquireMagicLinkSession,
		resolveTelemetryMaterials: ResolveTelemetryMaterials,
		reuseDevice: func(identity AccountIdentity) (TrustedDevice, Enrollment, bool, error) {
			return FindReusableTrustedDevice(authDir, identity)
		},
	}
}

func (s *Service) LoginURL() string {
	if s == nil || strings.TrimSpace(s.loginURL) == "" {
		return DefaultLoginURL
	}
	return s.loginURL
}

func (s *Service) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) Login(ctx context.Context, options MagicLinkLoginOptions) (*LoginResult, error) {
	if s == nil {
		return nil, fmt.Errorf("Claude Desktop login service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Timeout <= 0 {
		options.Timeout = 5 * time.Minute
	}
	loginCtx, cancelLogin := context.WithTimeout(ctx, options.Timeout)
	defer cancelLogin()

	acquire := s.acquire
	if acquire == nil {
		acquire = AcquireMagicLinkSession
	}
	session, errSession := acquire(loginCtx, s.httpClient, s.claudeOrigin, options)
	if errSession != nil {
		return nil, fmt.Errorf("Claude Desktop magic-link login failed: %w", errSession)
	}
	if session == nil {
		return nil, fmt.Errorf("Claude Desktop magic-link login returned no session")
	}
	token, identity, errAuthorize := s.AuthorizeSession(loginCtx, *session)
	if errAuthorize != nil {
		return nil, errAuthorize
	}
	sessionsToken, sessionsIdentity, errSessionsAuthorize := s.AuthorizeCoworkSessions(loginCtx, *session)
	if errSessionsAuthorize != nil {
		return nil, fmt.Errorf("Claude Desktop Cowork Sessions authorization failed: %w", errSessionsAuthorize)
	}
	if !sameAccountIdentity(identity, sessionsIdentity) {
		return nil, fmt.Errorf("Claude Desktop Cowork Sessions authorization returned a different account")
	}
	token = sessionsToken
	identity = sessionsIdentity
	authID, errAuthID := StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		return nil, errAuthID
	}
	var telemetryMaterials TelemetryMaterials
	if resolveMaterials := s.resolveTelemetryMaterials; resolveMaterials != nil {
		resolved, errMaterials := resolveMaterials(loginCtx, s.appVersion)
		if errMaterials != nil {
			log.WithError(errMaterials).Warn("Claude Desktop telemetry materials are unavailable; auxiliary telemetry will remain degraded")
		} else {
			telemetryMaterials = resolved
		}
	}

	if reuseDevice := s.reuseDevice; reuseDevice != nil {
		device, previousEnrollment, found, errReuse := reuseDevice(identity)
		if errReuse != nil {
			log.Warnf("Claude Desktop trusted-device reuse lookup failed; registering a new device: %v", errReuse)
		} else if found {
			enrollment := NewEnrollment(authID, identity, device, s.currentTime())
			if registeredAt := strings.TrimSpace(previousEnrollment.RegisteredAt); registeredAt != "" {
				enrollment.RegisteredAt = registeredAt
			}
			return &LoginResult{
				AuthID:             authID,
				SessionKey:         strings.TrimSpace(session.SessionKey),
				Token:              token,
				Identity:           identity,
				Enrollment:         enrollment,
				Device:             device,
				TelemetryMaterials: telemetryMaterials,
				DeviceReused:       true,
			}, nil
		}
	}

	device, errEnroll := s.EnrollTrustedDevice(loginCtx, token.AccessToken, "")
	if errEnroll != nil {
		return nil, fmt.Errorf("Claude Desktop trusted-device enrollment failed: %w", errEnroll)
	}
	enrollment := NewEnrollment(authID, identity, device, s.currentTime())
	return &LoginResult{
		AuthID:             authID,
		SessionKey:         strings.TrimSpace(session.SessionKey),
		Token:              token,
		Identity:           identity,
		Enrollment:         enrollment,
		Device:             device,
		TelemetryMaterials: telemetryMaterials,
	}, nil
}

func (s *Service) AuthorizeSession(ctx context.Context, session DesktopSession) (TokenData, AccountIdentity, error) {
	return s.authorizeSession(ctx, session, oauthAuthorizationConfig{
		ClientID:      OAuthClientID,
		RedirectURI:   OAuthRedirectURI,
		Scope:         OAuthScope,
		TokenLifetime: desktopTokenLifetime,
	})
}

func (s *Service) AuthorizeCoworkSessions(ctx context.Context, session DesktopSession) (TokenData, AccountIdentity, error) {
	return s.authorizeSession(ctx, session, oauthAuthorizationConfig{
		ClientID:      CoworkSessionsOAuthClientID,
		RedirectURI:   CoworkSessionsOAuthRedirectURI,
		Scope:         CoworkSessionsOAuthScope,
		TokenLifetime: coworkSessionsTokenLifetime,
	})
}

func (s *Service) authorizeSession(ctx context.Context, session DesktopSession, oauthConfig oauthAuthorizationConfig) (TokenData, AccountIdentity, error) {
	identity := normalizeIdentity(session.Identity)
	if strings.TrimSpace(session.SessionKey) == "" {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop sessionKey cookie is missing")
	}
	if _, errOrganization := uuid.Parse(identity.OrganizationUUID); errOrganization != nil {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop lastActiveOrg cookie is invalid")
	}
	verifier, challenge, state, errPKCE := generatePKCE()
	if errPKCE != nil {
		return TokenData{}, identity, fmt.Errorf("generate Claude Desktop PKCE: %w", errPKCE)
	}
	authorizeBody := authorizeRequest{
		ResponseType:        "code",
		ClientID:            oauthConfig.ClientID,
		OrganizationUUID:    identity.OrganizationUUID,
		RedirectURI:         oauthConfig.RedirectURI,
		Scope:               oauthConfig.Scope,
		State:               state,
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	}
	authorizeURL := strings.TrimRight(s.apiHost, "/") + "/v1/oauth/" + url.PathEscape(identity.OrganizationUUID) + "/authorize"
	var authorizeResponse struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if errPost := s.postJSON(ctx, authorizeURL, session.SessionKey, authorizeBody, &authorizeResponse, requestHeadersOAuthAuthorize); errPost != nil {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop organization authorize failed: %w", errPost)
	}
	redirect, errRedirect := url.Parse(strings.TrimSpace(authorizeResponse.RedirectURI))
	if errRedirect != nil || redirect == nil {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop organization authorize returned an invalid redirect URI")
	}
	code := strings.TrimSpace(redirect.Query().Get("code"))
	if code == "" {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop organization authorize returned no code")
	}

	tokenRequest := authorizationCodeRequest{
		GrantType:    "authorization_code",
		ClientID:     oauthConfig.ClientID,
		Code:         code,
		RedirectURI:  oauthConfig.RedirectURI,
		State:        state,
		CodeVerifier: verifier,
		ExpiresIn:    int64(oauthConfig.TokenLifetime / time.Second),
	}
	tokenResponse, errToken := s.exchangeToken(ctx, session.SessionKey, tokenRequest)
	if errToken != nil {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop token exchange failed: %w", errToken)
	}
	identity = mergeTokenIdentity(identity, tokenResponse)
	if _, errAccount := uuid.Parse(identity.AccountUUID); errAccount != nil {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop login did not resolve an account UUID")
	}
	if strings.TrimSpace(identity.Email) == "" {
		return TokenData{}, identity, fmt.Errorf("Claude Desktop login did not resolve an account email")
	}
	return tokenDataFromResponse(tokenResponse, s.currentTime()), identity, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenData, error) {
	return s.refresh(ctx, refreshToken, oauthAuthorizationConfig{
		ClientID:      CoworkSessionsOAuthClientID,
		RedirectURI:   CoworkSessionsOAuthRedirectURI,
		Scope:         CoworkSessionsOAuthScope,
		TokenLifetime: coworkSessionsTokenLifetime,
	})
}

func (s *Service) refresh(ctx context.Context, refreshToken string, oauthConfig oauthAuthorizationConfig) (TokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return TokenData{}, fmt.Errorf("Claude Desktop refresh token is missing")
	}
	refreshKey := oauthConfig.ClientID + "\x00" + oauthConfig.Scope + "\x00" + refreshToken
	result, errRefresh, _ := desktopRefreshGroup.Do(refreshKey, func() (any, error) {
		request := refreshTokenRequest{
			GrantType:    "refresh_token",
			ClientID:     oauthConfig.ClientID,
			RefreshToken: refreshToken,
			Scope:        oauthConfig.Scope,
			ExpiresIn:    int64(oauthConfig.TokenLifetime / time.Second),
		}
		response, errExchange := s.exchangeToken(context.WithoutCancel(ctx), "", request)
		if errExchange != nil {
			return nil, errExchange
		}
		if strings.TrimSpace(response.RefreshToken) == "" {
			response.RefreshToken = refreshToken
		}
		return tokenDataFromResponse(response, s.currentTime()), nil
	})
	if errRefresh != nil {
		return TokenData{}, errRefresh
	}
	token, ok := result.(TokenData)
	if !ok {
		return TokenData{}, fmt.Errorf("Claude Desktop refresh returned an invalid result")
	}
	return token, nil
}

func sameAccountIdentity(left, right AccountIdentity) bool {
	return strings.EqualFold(strings.TrimSpace(left.AccountUUID), strings.TrimSpace(right.AccountUUID)) &&
		strings.EqualFold(strings.TrimSpace(left.OrganizationUUID), strings.TrimSpace(right.OrganizationUUID))
}

func (s *Service) EnrollTrustedDevice(ctx context.Context, accessToken, displayName string) (TrustedDevice, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return TrustedDevice{}, fmt.Errorf("access token is missing")
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = defaultDisplayName()
	}
	body := struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: displayName}
	var response struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	endpoint := strings.TrimRight(s.apiHost, "/") + "/api/auth/trusted_devices"
	enrollCtx, cancelEnroll := context.WithTimeout(ctx, trustedDeviceTimeout)
	defer cancelEnroll()
	if errPost := s.postJSON(enrollCtx, endpoint, accessToken, body, &response, requestHeadersTrustedDevice); errPost != nil {
		return TrustedDevice{}, errPost
	}
	response.DeviceID = strings.ToLower(strings.TrimSpace(response.DeviceID))
	response.DeviceToken = strings.TrimSpace(response.DeviceToken)
	if _, errDevice := uuid.Parse(response.DeviceID); errDevice != nil {
		return TrustedDevice{}, fmt.Errorf("trusted-device response did not contain a valid device_id")
	}
	if response.DeviceToken == "" {
		return TrustedDevice{}, fmt.Errorf("trusted-device response did not contain device_token")
	}
	return TrustedDevice{DeviceID: response.DeviceID, DeviceToken: response.DeviceToken, DisplayName: displayName}, nil
}

func (s *Service) exchangeToken(ctx context.Context, sessionKey string, payload any) (oauthTokenResponse, error) {
	endpoint := strings.TrimRight(s.apiHost, "/") + "/v1/oauth/token"
	var response oauthTokenResponse
	errExchange := s.postJSON(ctx, endpoint, sessionKey, payload, &response, requestHeadersOAuthToken)
	if errExchange != nil && strings.Contains(strings.ToLower(errExchange.Error()), "expires_in") {
		switch request := payload.(type) {
		case authorizationCodeRequest:
			request.ExpiresIn = 0
			payload = request
		case refreshTokenRequest:
			request.ExpiresIn = 0
			payload = request
		}
		errExchange = s.postJSON(ctx, endpoint, sessionKey, payload, &response, requestHeadersOAuthToken)
	}
	if errExchange != nil {
		return oauthTokenResponse{}, errExchange
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		return oauthTokenResponse{}, fmt.Errorf("token endpoint returned no access_token")
	}
	if response.ExpiresIn <= 0 {
		return oauthTokenResponse{}, fmt.Errorf("token endpoint returned invalid expires_in")
	}
	return response, nil
}

func (s *Service) postJSON(ctx context.Context, endpoint, bearer string, payload, output any, headerProfile requestHeaderProfile) error {
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("marshal request: %w", errMarshal)
	}
	requestCtx, cancel := context.WithTimeout(ctx, credentialRequestTimeout)
	defer cancel()
	request, errRequest := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	request.Header.Set("Content-Type", "application/json")
	if headerProfile != requestHeadersTrustedDevice {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	if bearer = strings.TrimSpace(bearer); bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if headerProfile == requestHeadersOAuthAuthorize {
		request.Header.Set("anthropic-client-platform", "desktop_app")
		request.Header.Set("anthropic-client-version", s.appVersion)
	}
	response, errDo := s.httpClient.Do(request)
	if errDo != nil {
		return fmt.Errorf("request failed: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debugf("Claude Desktop auth: close response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errRead != nil {
		return fmt.Errorf("read response: %w", errRead)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var apiError struct {
			Error struct {
				Message string `json:"message"`
				Details struct {
					ErrorCode string `json:"error_code"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiError)
		detail := strings.TrimSpace(apiError.Error.Details.ErrorCode)
		if detail == "" {
			detail = strings.TrimSpace(apiError.Error.Message)
		}
		if detail == "" {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 512 {
				detail = detail[:512]
			}
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return &HTTPStatusError{Status: response.StatusCode, Detail: detail}
	}
	if output == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(body, output); errUnmarshal != nil {
		return fmt.Errorf("parse response: %w", errUnmarshal)
	}
	return nil
}

// HTTPStatusError is the non-2xx outcome of a credential endpoint call. It keeps
// the status so callers can classify the failure the way the Desktop OAuth
// flows do (server_error >= 500, otherwise auth_error).
type HTTPStatusError struct {
	Status int
	Detail string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
}

func isOAuthScopeInsufficient(err error) bool {
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(statusError.Detail), "oauth_scope_insufficient")
}

func generatePKCE() (verifier, challenge, state string, err error) {
	verifierBytes := make([]byte, 32)
	stateBytes := make([]byte, 32)
	if _, err = rand.Read(verifierBytes); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(stateBytes); err != nil {
		return "", "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	const stateAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	stateRunes := make([]byte, len(stateBytes))
	for index, value := range stateBytes {
		stateRunes[index] = stateAlphabet[int(value)%len(stateAlphabet)]
	}
	state = string(stateRunes)
	return verifier, challenge, state, nil
}

func tokenDataFromResponse(response oauthTokenResponse, now time.Time) TokenData {
	if now.IsZero() {
		now = time.Now()
	}
	return TokenData{
		AccessToken:  strings.TrimSpace(response.AccessToken),
		RefreshToken: strings.TrimSpace(response.RefreshToken),
		ExpiresIn:    response.ExpiresIn,
		Expire:       now.Add(time.Duration(response.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
	}
}

func normalizeIdentity(identity AccountIdentity) AccountIdentity {
	identity.AccountUUID = strings.ToLower(strings.TrimSpace(identity.AccountUUID))
	identity.Email = strings.TrimSpace(identity.Email)
	identity.OrganizationUUID = strings.ToLower(strings.TrimSpace(identity.OrganizationUUID))
	identity.OrganizationName = strings.TrimSpace(identity.OrganizationName)
	return identity
}

func mergeTokenIdentity(identity AccountIdentity, response oauthTokenResponse) AccountIdentity {
	if strings.TrimSpace(identity.AccountUUID) == "" {
		identity.AccountUUID = response.Account.UUID
	}
	if strings.TrimSpace(identity.Email) == "" {
		identity.Email = response.Account.Email
		if strings.TrimSpace(identity.Email) == "" {
			identity.Email = response.Account.EmailAddress
		}
	}
	if strings.TrimSpace(identity.OrganizationUUID) == "" {
		identity.OrganizationUUID = response.Organization.UUID
	}
	if strings.TrimSpace(identity.OrganizationName) == "" {
		identity.OrganizationName = response.Organization.Name
	}
	return normalizeIdentity(identity)
}

func defaultDisplayName() string {
	hostname := ""
	if value, errHostname := os.Hostname(); errHostname == nil {
		hostname = strings.TrimSpace(value)
	}
	if hostname == "" {
		hostname = "Desktop"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return fmt.Sprintf("Claude Desktop on %s · %s", hostname, platform)
}
