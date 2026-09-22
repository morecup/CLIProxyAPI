package claudedesktop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	magicLinkTestAccountUUID = "11111111-1111-4111-8111-111111111111"
	magicLinkTestOrgUUID     = "22222222-2222-4222-8222-222222222222"
)

func TestParseMagicLinkAcceptsDesktopHTTPSAndDeepLink(t *testing.T) {
	nonce := strings.Repeat("n", 32)
	encodedEmail := base64.RawURLEncoding.EncodeToString([]byte("person@example.test"))
	paddedEmail := base64.StdEncoding.EncodeToString([]byte("person@example.test"))
	tests := []struct {
		name      string
		raw       string
		anonymous string
	}{
		{
			name: "https desktop link",
			raw:  "https://claude.ai/magic-link?client=desktop#" + nonce + ":" + encodedEmail,
		},
		{
			name: "current email link without client and with padding",
			raw:  "https://claude.ai/magic-link#" + nonce + ":" + paddedEmail,
		},
		{
			name:      "desktop deep link",
			raw:       "claude://claude.ai/magic-link?anon_id=anon-123#" + nonce + ":" + encodedEmail,
			anonymous: "anon-123",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentials, errParse := parseMagicLink(test.raw)
			if errParse != nil {
				t.Fatalf("parseMagicLink() error = %v", errParse)
			}
			if credentials.Nonce != nonce || credentials.EncodedEmailAddress != encodedEmail || credentials.AnonymousID != test.anonymous {
				t.Fatalf("credentials = %#v", credentials)
			}
			if credentials.LoginPageURL != "https://claude.ai/magic-link?client=desktop#"+nonce+":"+encodedEmail {
				t.Fatalf("LoginPageURL = %q", credentials.LoginPageURL)
			}
		})
	}
}

func TestParseMagicLinkRejectsNonDesktopOrUnsafeURLs(t *testing.T) {
	nonce := strings.Repeat("n", 32)
	encodedEmail := base64.RawURLEncoding.EncodeToString([]byte("person@example.test"))
	fragment := nonce + ":" + encodedEmail
	tests := []string{
		"http://claude.ai/magic-link?client=desktop#" + fragment,
		"https://evil.example/magic-link?client=desktop#" + fragment,
		"https://claude.ai/other?client=desktop#" + fragment,
		"https://claude.ai/magic-link?client=web#" + fragment,
		"https://claude.ai/magic-link?client=desktop&next=https%3A%2F%2Fevil.example#" + fragment,
		"https://claude.ai/magic-link?client=desktop;next=evil#" + fragment,
		"https://claude.ai/magic-link?client=desktop#" + fragment + ":extra",
		"claude://claude.ai/magic-link?client=desktop&anon_id=has%20space#" + fragment,
	}
	for _, raw := range tests {
		if _, errParse := parseMagicLink(raw); errParse == nil {
			t.Fatalf("parseMagicLink(%q) succeeded, want rejection", raw)
		}
	}
}

func TestAcquireMagicLinkSessionExchangesOnceWithPrivateJar(t *testing.T) {
	nonce := strings.Repeat("n", 32)
	encodedEmail := base64.RawURLEncoding.EncodeToString([]byte("person@example.test"))
	var exchangeCalls atomic.Int32
	var bootstrapCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case magicLinkVerifyPath:
			exchangeCalls.Add(1)
			if request.Method != http.MethodPost {
				t.Errorf("verify method = %s", request.Method)
			}
			if request.Header.Get("Origin") != server.URL {
				t.Errorf("verify Origin = %q", request.Header.Get("Origin"))
			}
			if request.Header.Get("Referer") != server.URL+magicLinkPath+"?client=desktop" {
				t.Errorf("verify Referer = %q", request.Header.Get("Referer"))
			}
			if request.Header.Get("User-Agent") != "CLIProxyAPI WebView test" {
				t.Errorf("verify User-Agent = %q", request.Header.Get("User-Agent"))
			}
			if request.Header.Get("Accept-Language") != "en-US" {
				t.Errorf("verify Accept-Language = %q", request.Header.Get("Accept-Language"))
			}
			if cookie, errCookie := request.Cookie("_cross_domain_anonymous_id"); errCookie != nil || cookie.Value != "anon-123" {
				t.Errorf("verify anonymous cookie = %v, %v", cookie, errCookie)
			}
			body, errRead := io.ReadAll(request.Body)
			if errRead != nil {
				t.Errorf("read verify payload: %v", errRead)
			}
			var payload magicLinkVerifyRequest
			if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
				t.Errorf("decode verify payload: %v", errDecode)
			}
			if payload.Credentials.Method != "nonce" || payload.Credentials.Nonce != nonce || payload.Credentials.EncodedEmailAddress != encodedEmail || payload.Locale != "en-US" || payload.OAuthClientID != nil || payload.ClientAttestation.HCaptchaToken != "official-sdk-token" || payload.Source != "claude" {
				t.Errorf("unexpected verify payload: %#v", payload)
			}
			var raw map[string]any
			if errDecode := json.Unmarshal(body, &raw); errDecode != nil {
				t.Errorf("decode raw verify payload: %v", errDecode)
			}
			if value, exists := raw["oauth_client_id"]; !exists || value != nil {
				t.Errorf("oauth_client_id = %#v, exists=%v; want explicit null", value, exists)
			}
			http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "private-session", Path: "/"})
			w.WriteHeader(http.StatusNoContent)
		case bootstrapPath:
			bootstrapCalls.Add(1)
			if cookie, errCookie := request.Cookie("sessionKey"); errCookie != nil || cookie.Value != "private-session" {
				t.Errorf("bootstrap session cookie = %v, %v", cookie, errCookie)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"account":{"uuid":"` + magicLinkTestAccountUUID + `","email":"person@example.test"},"organization":{"uuid":"` + magicLinkTestOrgUUID + `","name":"Test organization"}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	credentials := magicLinkCredentials{
		Nonce:               nonce,
		EncodedEmailAddress: encodedEmail,
		AnonymousID:         "anon-123",
		LoginPageURL:        "https://claude.ai/magic-link?client=desktop#" + nonce + ":" + encodedEmail,
	}
	session, errAcquire := acquireMagicLinkSession(context.Background(), server.Client(), server.URL, credentials, func(_ context.Context, got magicLinkCredentials) (magicLinkAttestation, error) {
		if got != credentials {
			t.Errorf("attestation credentials = %#v, want %#v", got, credentials)
		}
		return magicLinkAttestation{HCaptchaToken: "official-sdk-token", Locale: "en-US", UserAgent: "CLIProxyAPI WebView test"}, nil
	})
	if errAcquire != nil {
		t.Fatalf("acquireMagicLinkSession() error = %v", errAcquire)
	}
	if exchangeCalls.Load() != 1 || bootstrapCalls.Load() != 1 {
		t.Fatalf("exchange/bootstrap calls = %d/%d, want 1/1", exchangeCalls.Load(), bootstrapCalls.Load())
	}
	if session.SessionKey != "private-session" || session.Identity.AccountUUID != magicLinkTestAccountUUID || session.Identity.OrganizationUUID != magicLinkTestOrgUUID || session.Identity.Email != "person@example.test" {
		t.Fatalf("session = %#v", session)
	}
}

func TestAcquireMagicLinkSessionRejectsOffOriginRedirectWithoutSecretLeak(t *testing.T) {
	nonce := strings.Repeat("n", 32)
	encodedEmail := base64.RawURLEncoding.EncodeToString([]byte("person@example.test"))
	var redirected atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		redirected.Add(1)
		http.Error(w, "unexpected redirect", http.StatusInternalServerError)
	}))
	defer redirectTarget.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == magicLinkVerifyPath {
			http.Redirect(w, request, redirectTarget.URL+"/capture", http.StatusFound)
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()

	credentials := magicLinkCredentials{Nonce: nonce, EncodedEmailAddress: encodedEmail, LoginPageURL: "https://claude.ai/magic-link?client=desktop#" + nonce + ":" + encodedEmail}
	_, errAcquire := acquireMagicLinkSession(context.Background(), server.Client(), server.URL, credentials, func(context.Context, magicLinkCredentials) (magicLinkAttestation, error) {
		return magicLinkAttestation{HCaptchaToken: "official-sdk-token"}, nil
	})
	if errAcquire == nil {
		t.Fatal("acquireMagicLinkSession() succeeded after off-origin redirect")
	}
	if redirected.Load() != 0 {
		t.Fatalf("off-origin redirect was requested %d times", redirected.Load())
	}
	for _, secret := range []string{nonce, encodedEmail, "official-sdk-token"} {
		if strings.Contains(errAcquire.Error(), secret) {
			t.Fatalf("exchange error leaked secret %q: %v", secret, errAcquire)
		}
	}
}

func TestAcquireMagicLinkSessionDoesNotEchoServerErrorBody(t *testing.T) {
	nonce := strings.Repeat("n", 32)
	encodedEmail := base64.RawURLEncoding.EncodeToString([]byte("person@example.test"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != magicLinkVerifyPath {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"official-sdk-token ` + nonce + ` ` + encodedEmail + `"}}`))
	}))
	defer server.Close()

	credentials := magicLinkCredentials{Nonce: nonce, EncodedEmailAddress: encodedEmail, LoginPageURL: "https://claude.ai/magic-link?client=desktop#" + nonce + ":" + encodedEmail}
	_, errAcquire := acquireMagicLinkSession(context.Background(), server.Client(), server.URL, credentials, func(context.Context, magicLinkCredentials) (magicLinkAttestation, error) {
		return magicLinkAttestation{HCaptchaToken: "official-sdk-token"}, nil
	})
	if errAcquire == nil {
		t.Fatal("acquireMagicLinkSession() succeeded after an exchange error")
	}
	for _, secret := range []string{nonce, encodedEmail, "official-sdk-token"} {
		if strings.Contains(errAcquire.Error(), secret) {
			t.Fatalf("exchange error leaked secret %q: %v", secret, errAcquire)
		}
	}
}
