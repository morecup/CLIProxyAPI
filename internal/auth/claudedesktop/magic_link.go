package claudedesktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/mail"
	"net/url"
	"strings"
	"unicode"
)

const (
	magicLinkPath          = "/magic-link"
	magicLinkVerifyPath    = "/api/auth/verify_magic_link"
	bootstrapPath          = "/edge-api/bootstrap"
	bootstrapQuery         = "statsig_hashing_algorithm=djb2&growthbook_format=sdk"
	bootstrapClientVersion = "1.40609.0"
	bootstrapSecCHUA       = `"Not/A)Brand";v="99", "Chromium";v="148"`
	bootstrapUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.40609.0 Chrome/148.0.7778.280 Electron/42.10.0 Safari/537.36 MSIX"
	maxMagicLinkLength     = 8192
	maxAttestationSize     = 16384
)

var errMagicLinkAttestationUnavailable = errors.New("Claude Desktop magic-link client attestation is unavailable")

type magicLinkCredentials struct {
	Nonce               string
	EncodedEmailAddress string
	AnonymousID         string
	LoginPageURL        string
}

type magicLinkAttestation struct {
	HCaptchaToken string
	Locale        string
	UserAgent     string
}

type magicLinkVerifyRequest struct {
	Credentials struct {
		Method              string `json:"method"`
		Nonce               string `json:"nonce"`
		EncodedEmailAddress string `json:"encoded_email_address"`
	} `json:"credentials"`
	Locale            string  `json:"locale"`
	OAuthClientID     *string `json:"oauth_client_id"`
	ClientAttestation struct {
		HCaptchaToken string `json:"hcaptcha_token"`
	} `json:"client_attestation"`
	Source string `json:"source"`
}

// AcquireMagicLinkSession uses an isolated platform browser profile to let the
// official Claude magic-link page obtain its hCaptcha attestation. The browser
// does not submit the magic-link request or expose cookies; this function
// performs the one-time exchange with a private Go CookieJar instead.
func AcquireMagicLinkSession(ctx context.Context, baseClient *http.Client, claudeOrigin string, options MagicLinkLoginOptions) (*DesktopSession, error) {
	credentials, errCredentials := readMagicLinkCredentials(options)
	if errCredentials != nil {
		return nil, errCredentials
	}
	attestationOptions := magicLinkAttestationOptions{
		ProxyURL:             strings.TrimSpace(options.ProxyURL),
		InteractiveSessionID: strings.TrimSpace(options.InteractiveSessionID),
	}
	return acquireMagicLinkSession(ctx, baseClient, claudeOrigin, credentials, func(ctx context.Context, credentials magicLinkCredentials) (magicLinkAttestation, error) {
		return acquireMagicLinkAttestation(ctx, credentials, attestationOptions)
	})
}

func readMagicLinkCredentials(options MagicLinkLoginOptions) (magicLinkCredentials, error) {
	rawMagicLink := strings.TrimSpace(options.MagicLink)
	if rawMagicLink == "" {
		if options.Prompt == nil {
			return magicLinkCredentials{}, fmt.Errorf("Claude Desktop magic link is required")
		}
		value, errPrompt := options.Prompt("Paste the complete Claude Desktop magic link from your email: ")
		if errPrompt != nil {
			return magicLinkCredentials{}, errPrompt
		}
		rawMagicLink = strings.TrimSpace(value)
	}
	return parseMagicLink(rawMagicLink)
}

func parseMagicLink(rawMagicLink string) (magicLinkCredentials, error) {
	rawMagicLink = strings.TrimSpace(rawMagicLink)
	if rawMagicLink == "" || len(rawMagicLink) > maxMagicLinkLength {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic link")
	}
	parsed, errParse := url.Parse(rawMagicLink)
	if errParse != nil || parsed == nil {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic link")
	}
	if parsed.User != nil || !strings.EqualFold(parsed.Hostname(), "claude.ai") || parsed.Path != magicLinkPath {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic link")
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "https" && scheme != "claude" {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link scheme")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link port")
	}
	query, errQuery := url.ParseQuery(parsed.RawQuery)
	if errQuery != nil {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link parameter")
	}
	for key, values := range query {
		switch key {
		case "client":
			if len(values) != 1 || !strings.EqualFold(strings.TrimSpace(values[0]), "desktop") {
				return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link client")
			}
		case "anon_id":
			if len(values) != 1 || !isSafeAnonymousID(values[0]) {
				return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop anonymous id")
			}
		default:
			return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link parameter")
		}
	}
	// Current Claude email links can omit the client query parameter even when
	// the request originated from /login?client=desktop. When present it must
	// still name the desktop client; the isolated verification page below is
	// always normalized back to client=desktop.
	if client := strings.TrimSpace(query.Get("client")); client != "" && !strings.EqualFold(client, "desktop") {
		return magicLinkCredentials{}, fmt.Errorf("Claude Desktop magic link must target the desktop client")
	}
	fragment := strings.TrimSpace(parsed.Fragment)
	nonce, encodedEmail, found := strings.Cut(fragment, ":")
	normalizedEmail, validEmail := normalizeEncodedEmail(encodedEmail)
	if !found || strings.Contains(encodedEmail, ":") || !isSafeNonce(nonce) || !validEmail {
		return magicLinkCredentials{}, fmt.Errorf("invalid Claude Desktop magic-link credentials")
	}
	loginPage := &url.URL{Scheme: "https", Host: "claude.ai", Path: magicLinkPath, RawQuery: "client=desktop", Fragment: nonce + ":" + normalizedEmail}
	return magicLinkCredentials{
		Nonce:               nonce,
		EncodedEmailAddress: normalizedEmail,
		AnonymousID:         strings.TrimSpace(query.Get("anon_id")),
		LoginPageURL:        loginPage.String(),
	}, nil
}

func acquireMagicLinkSession(
	ctx context.Context,
	baseClient *http.Client,
	claudeOrigin string,
	credentials magicLinkCredentials,
	attestationAcquirer func(context.Context, magicLinkCredentials) (magicLinkAttestation, error),
) (*DesktopSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if attestationAcquirer == nil {
		return nil, errMagicLinkAttestationUnavailable
	}
	origin, errOrigin := parseClaudeOrigin(claudeOrigin)
	if errOrigin != nil {
		return nil, errOrigin
	}
	attestation, errAttestation := attestationAcquirer(ctx, credentials)
	if errAttestation != nil {
		return nil, fmt.Errorf("obtain Claude Desktop magic-link attestation: %w", errAttestation)
	}
	if strings.TrimSpace(attestation.HCaptchaToken) == "" || len(attestation.HCaptchaToken) > maxAttestationSize {
		return nil, errMagicLinkAttestationUnavailable
	}
	client, jar, errClient := newMagicLinkHTTPClient(baseClient, origin)
	if errClient != nil {
		return nil, errClient
	}
	if credentials.AnonymousID != "" {
		jar.SetCookies(origin, []*http.Cookie{{
			Name:     "_cross_domain_anonymous_id",
			Value:    credentials.AnonymousID,
			Path:     "/",
			Secure:   origin.Scheme == "https",
			SameSite: http.SameSiteLaxMode,
		}})
	}
	if errVerify := verifyMagicLink(ctx, client, origin, credentials, attestation); errVerify != nil {
		return nil, errVerify
	}
	sessionKey := sessionKeyFromJar(jar, origin)
	if sessionKey == "" {
		return nil, fmt.Errorf("Claude Desktop magic-link exchange did not establish a session")
	}
	identity, errIdentity := fetchBootstrapIdentity(ctx, client, origin)
	if errIdentity != nil {
		return nil, errIdentity
	}
	return &DesktopSession{SessionKey: sessionKey, Identity: identity}, nil
}

func parseClaudeOrigin(rawOrigin string) (*url.URL, error) {
	if strings.TrimSpace(rawOrigin) == "" {
		rawOrigin = DefaultClaudeOrigin
	}
	origin, errParse := url.Parse(rawOrigin)
	if errParse != nil || origin == nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil {
		return nil, fmt.Errorf("invalid Claude Desktop origin")
	}
	if origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" {
		return nil, fmt.Errorf("invalid Claude Desktop origin")
	}
	origin.Path = ""
	return origin, nil
}

func newMagicLinkHTTPClient(baseClient *http.Client, origin *url.URL) (*http.Client, http.CookieJar, error) {
	if origin == nil {
		return nil, nil, fmt.Errorf("Claude Desktop origin is missing")
	}
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	clone := *baseClient
	jar, errJar := cookiejar.New(nil)
	if errJar != nil {
		return nil, nil, fmt.Errorf("create Claude Desktop magic-link cookie jar: %w", errJar)
	}
	clone.Jar = jar
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request == nil || request.URL == nil || request.URL.Scheme != origin.Scheme || !strings.EqualFold(request.URL.Host, origin.Host) {
			return fmt.Errorf("Claude Desktop magic-link redirect left the Claude origin")
		}
		if len(via) > 4 {
			return fmt.Errorf("Claude Desktop magic-link redirect limit exceeded")
		}
		return nil
	}
	return &clone, jar, nil
}

func verifyMagicLink(ctx context.Context, client *http.Client, origin *url.URL, credentials magicLinkCredentials, attestation magicLinkAttestation) error {
	if client == nil || origin == nil {
		return fmt.Errorf("Claude Desktop magic-link client is unavailable")
	}
	locale := strings.TrimSpace(attestation.Locale)
	if locale == "" {
		locale = "en-US"
	}
	payload := magicLinkVerifyRequest{Locale: locale, Source: "claude"}
	payload.Credentials.Method = "nonce"
	payload.Credentials.Nonce = credentials.Nonce
	payload.Credentials.EncodedEmailAddress = credentials.EncodedEmailAddress
	payload.ClientAttestation.HCaptchaToken = strings.TrimSpace(attestation.HCaptchaToken)
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop magic-link exchange: %w", errMarshal)
	}
	endpoint := origin.ResolveReference(&url.URL{Path: magicLinkVerifyPath})
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if errRequest != nil {
		return fmt.Errorf("create Claude Desktop magic-link exchange: %w", errRequest)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
	request.Header.Set("Referer", origin.Scheme+"://"+origin.Host+magicLinkPath+"?client=desktop")
	request.Header.Set("Accept-Language", locale)
	if userAgent := strings.TrimSpace(attestation.UserAgent); userAgent != "" {
		request.Header.Set("User-Agent", userAgent)
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return fmt.Errorf("Claude Desktop magic-link exchange request failed: %w", errDo)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	_, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errRead != nil {
		return fmt.Errorf("read Claude Desktop magic-link exchange: %w", errRead)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Claude Desktop magic-link exchange failed with HTTP %d", response.StatusCode)
	}
	return nil
}

func fetchBootstrapIdentity(ctx context.Context, client *http.Client, origin *url.URL) (AccountIdentity, error) {
	endpoint := origin.ResolveReference(&url.URL{Path: bootstrapPath, RawQuery: bootstrapQuery})
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if errRequest != nil {
		return AccountIdentity{}, fmt.Errorf("create Claude Desktop bootstrap request: %w", errRequest)
	}
	setBootstrapRequestHeaders(request, origin)
	response, errDo := client.Do(request)
	if errDo != nil {
		return AccountIdentity{}, fmt.Errorf("Claude Desktop bootstrap request failed: %w", errDo)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errRead != nil {
		return AccountIdentity{}, fmt.Errorf("read Claude Desktop bootstrap response: %w", errRead)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return AccountIdentity{}, fmt.Errorf("Claude Desktop bootstrap returned HTTP %d", response.StatusCode)
	}
	identity, errIdentity := extractBootstrapIdentity(body, "")
	if errIdentity != nil {
		return AccountIdentity{}, fmt.Errorf("parse Claude Desktop bootstrap identity: %w", errIdentity)
	}
	if strings.TrimSpace(identity.OrganizationUUID) == "" {
		return AccountIdentity{}, fmt.Errorf("Claude Desktop bootstrap contained no organization UUID")
	}
	return identity, nil
}

// setBootstrapRequestHeaders mirrors the accepted Claude Desktop browser
// profile. Claude's edge rejects the default Go HTTP identity even when the
// sessionKey itself is valid, so bootstrap must look like the first-party
// Desktop fetch that owns the session.
func setBootstrapRequestHeaders(request *http.Request, origin *url.URL) {
	if request == nil || origin == nil {
		return
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "en-US")
	request.Header.Set("Referer", origin.Scheme+"://"+origin.Host+"/")
	request.Header.Set("User-Agent", bootstrapUserAgent)
	request.Header.Set("Sec-CH-UA", bootstrapSecCHUA)
	request.Header.Set("Sec-CH-UA-Mobile", "?0")
	request.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("anthropic-client-platform", "desktop_app")
	request.Header.Set("anthropic-client-version", bootstrapClientVersion)
}

func sessionKeyFromJar(jar http.CookieJar, origin *url.URL) string {
	if jar == nil || origin == nil {
		return ""
	}
	for _, cookie := range jar.Cookies(origin) {
		if cookie.Name == "sessionKey" && strings.TrimSpace(cookie.Value) != "" {
			return cookie.Value
		}
	}
	return ""
}

func isSafeNonce(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func normalizeEncodedEmail(value string) (string, bool) {
	if value == "" || len(value) > 1024 || strings.TrimSpace(value) != value || strings.ContainsAny(value, ":\\") {
		return "", false
	}
	raw := strings.TrimRight(value, "=")
	if raw == "" || len(value)-len(raw) > 2 || strings.Contains(raw, "=") {
		return "", false
	}
	var decoded []byte
	var errDecode error
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		decoded, errDecode = encoding.DecodeString(raw)
		if errDecode == nil {
			break
		}
	}
	if errDecode != nil {
		return "", false
	}
	email := string(decoded)
	address, errAddress := mail.ParseAddress(email)
	if errAddress != nil || address.Address != email {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(decoded), true
}

func isSafeAnonymousID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
