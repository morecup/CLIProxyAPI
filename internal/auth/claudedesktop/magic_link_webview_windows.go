//go:build windows

package claudedesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

const magicLinkAttestationHook = `(function () {
  const endpointPath = "/api/auth/verify_magic_link";
  let submitted = false;

  function submit(body) {
    if (submitted || typeof body !== "string") return false;
    let parsed;
    try { parsed = JSON.parse(body); } catch (_) { return false; }
    const token = parsed && parsed.client_attestation && parsed.client_attestation.hcaptcha_token;
    if (typeof token !== "string" || token.length === 0) return false;
    submitted = true;
    const payload = JSON.stringify({
      hcaptcha_token: token,
      locale: typeof parsed.locale === "string" ? parsed.locale : (navigator.language || "en-US"),
      user_agent: navigator.userAgent || ""
    });
    void window.cliproxyClaudeDesktopAttestation(payload);
    return true;
  }

  function matches(input, init) {
    let rawURL = "";
    let method = "GET";
    let body = init && init.body;
    if (typeof input === "string" || input instanceof URL) {
      rawURL = String(input);
    } else if (input && typeof input.url === "string") {
      rawURL = input.url;
      method = input.method || method;
    }
    if (init && init.method) method = init.method;
    let parsedURL;
    try { parsedURL = new URL(rawURL, location.href); } catch (_) { return false; }
    if (String(method).toUpperCase() !== "POST" || parsedURL.origin !== "https://claude.ai" || parsedURL.pathname !== endpointPath) {
      return false;
    }
    return submit(body);
  }

  const originalFetch = window.fetch;
  window.fetch = function (input, init) {
    if (matches(input, init)) {
      return Promise.resolve(new Response("{}", {status: 202, headers: {"content-type": "application/json"}}));
    }
    return originalFetch.apply(this, arguments);
  };

  const originalOpen = XMLHttpRequest.prototype.open;
  const originalSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cliproxyMethod = method;
    this.__cliproxyURL = url;
    return originalOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function (body) {
    if (matches(this.__cliproxyURL, {method: this.__cliproxyMethod, body: body})) return;
    return originalSend.apply(this, arguments);
  };
})();`

const magicLinkAttestationHelperArg = "--claude-desktop-attestation-helper"

type webViewAttestationPayload struct {
	HCaptchaToken string `json:"hcaptcha_token"`
	Locale        string `json:"locale"`
	UserAgent     string `json:"user_agent"`
}

type webViewAttestationResult struct {
	attestation magicLinkAttestation
	err         error
}

type webViewAttestationHelperRequest struct {
	Credentials magicLinkCredentials `json:"credentials"`
	ProfilePath string               `json:"profile_path"`
}

type webViewAttestationHelperResponse struct {
	HCaptchaToken string `json:"hcaptcha_token,omitempty"`
	Locale        string `json:"locale,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`
	Error         string `json:"error,omitempty"`
}

func acquireMagicLinkAttestation(ctx context.Context, credentials magicLinkCredentials) (magicLinkAttestation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	profilePath, errProfile := os.MkdirTemp("", "cliproxy-claude-desktop-webview-")
	if errProfile != nil {
		return magicLinkAttestation{}, fmt.Errorf("create isolated WebView2 profile: %w", errProfile)
	}
	defer cleanupMagicLinkWebViewProfile(profilePath)

	executable, errExecutable := os.Executable()
	if errExecutable != nil || strings.TrimSpace(executable) == "" {
		return magicLinkAttestation{}, fmt.Errorf("%w: locate helper executable", errMagicLinkAttestationUnavailable)
	}
	payload, errMarshal := json.Marshal(webViewAttestationHelperRequest{Credentials: credentials, ProfilePath: profilePath})
	if errMarshal != nil {
		return magicLinkAttestation{}, fmt.Errorf("marshal Claude Desktop WebView helper request: %w", errMarshal)
	}

	command := exec.CommandContext(ctx, executable, magicLinkAttestationHelperArg)
	command.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if errRun := command.Run(); errRun != nil {
		return magicLinkAttestation{}, fmt.Errorf("%w: isolated WebView2 helper did not complete", errMagicLinkAttestationUnavailable)
	}
	if stdout.Len() == 0 || stdout.Len() > maxAttestationSize+2048 {
		return magicLinkAttestation{}, errMagicLinkAttestationUnavailable
	}
	var response webViewAttestationHelperResponse
	if errDecode := json.Unmarshal(stdout.Bytes(), &response); errDecode != nil {
		return magicLinkAttestation{}, fmt.Errorf("%w: invalid isolated WebView2 helper response", errMagicLinkAttestationUnavailable)
	}
	if strings.TrimSpace(response.Error) != "" {
		return magicLinkAttestation{}, errMagicLinkAttestationUnavailable
	}
	attestation := magicLinkAttestation{
		HCaptchaToken: strings.TrimSpace(response.HCaptchaToken),
		Locale:        strings.TrimSpace(response.Locale),
		UserAgent:     strings.TrimSpace(response.UserAgent),
	}
	if attestation.HCaptchaToken == "" || len(attestation.HCaptchaToken) > maxAttestationSize || len(attestation.Locale) > 64 || len(attestation.UserAgent) > 1024 {
		return magicLinkAttestation{}, errMagicLinkAttestationUnavailable
	}
	return attestation, nil
}

// RunMagicLinkAttestationHelper handles the private helper invocation used to
// isolate WebView2 initialization. It returns true only for that invocation,
// allowing the normal CLI startup to return before parsing user arguments.
func RunMagicLinkAttestationHelper() bool {
	if len(os.Args) != 2 || os.Args[1] != magicLinkAttestationHelperArg {
		return false
	}
	response := webViewAttestationHelperResponse{Error: "unavailable"}
	defer func() {
		_ = json.NewEncoder(os.Stdout).Encode(response)
	}()
	var request webViewAttestationHelperRequest
	if errDecode := json.NewDecoder(io.LimitReader(os.Stdin, maxMagicLinkLength+4096)).Decode(&request); errDecode != nil {
		return true
	}
	if strings.TrimSpace(request.Credentials.Nonce) == "" || strings.TrimSpace(request.Credentials.EncodedEmailAddress) == "" || strings.TrimSpace(request.Credentials.LoginPageURL) == "" || !isMagicLinkWebViewProfile(request.ProfilePath) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result := make(chan webViewAttestationResult, 1)
	runMagicLinkAttestationWebView(ctx, request.Credentials, request.ProfilePath, result)
	value := <-result
	if value.err != nil {
		return true
	}
	response = webViewAttestationHelperResponse{
		HCaptchaToken: value.attestation.HCaptchaToken,
		Locale:        value.attestation.Locale,
		UserAgent:     value.attestation.UserAgent,
	}
	return true
}

func runMagicLinkAttestationWebView(ctx context.Context, credentials magicLinkCredentials, profilePath string, result chan<- webViewAttestationResult) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		DataPath:  profilePath,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Claude Desktop authentication",
			Width:  900,
			Height: 700,
		},
	})
	if view == nil {
		result <- webViewAttestationResult{err: fmt.Errorf("%w: WebView2 runtime is unavailable", errMagicLinkAttestationUnavailable)}
		return
	}
	showMagicLinkWebViewWindow(view.Window())
	var reportOnce sync.Once
	report := func(value webViewAttestationResult) {
		reportOnce.Do(func() {
			result <- value
			view.Terminate()
		})
	}
	if errBind := view.Bind("cliproxyClaudeDesktopAttestation", func(raw string) (bool, error) {
		var payload webViewAttestationPayload
		if errDecode := json.Unmarshal([]byte(raw), &payload); errDecode != nil {
			report(webViewAttestationResult{err: fmt.Errorf("decode Claude Desktop WebView attestation: %w", errDecode)})
			return false, nil
		}
		payload.HCaptchaToken = strings.TrimSpace(payload.HCaptchaToken)
		payload.Locale = strings.TrimSpace(payload.Locale)
		payload.UserAgent = strings.TrimSpace(payload.UserAgent)
		if payload.HCaptchaToken == "" || len(payload.HCaptchaToken) > maxAttestationSize || len(payload.Locale) > 64 || len(payload.UserAgent) > 1024 {
			report(webViewAttestationResult{err: errMagicLinkAttestationUnavailable})
			return false, nil
		}
		report(webViewAttestationResult{attestation: magicLinkAttestation{
			HCaptchaToken: payload.HCaptchaToken,
			Locale:        payload.Locale,
			UserAgent:     payload.UserAgent,
		}})
		return true, nil
	}); errBind != nil {
		view.Destroy()
		result <- webViewAttestationResult{err: fmt.Errorf("bind Claude Desktop WebView attestation bridge: %w", errBind)}
		return
	}
	view.Init(magicLinkAttestationHook)
	if anonymousID := strings.TrimSpace(credentials.AnonymousID); anonymousID != "" {
		encodedAnonymousID, _ := json.Marshal(anonymousID)
		view.Init(`(function () {
  if (location.protocol === "https:" && location.hostname === "claude.ai") {
    document.cookie = "_cross_domain_anonymous_id=" + encodeURIComponent(` + string(encodedAnonymousID) + `) + "; Path=/; Max-Age=31536000; SameSite=Lax; Secure";
  }
})();`)
	}

	stopCancellation := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			view.Terminate()
		case <-stopCancellation:
		}
	}()
	view.Navigate(credentials.LoginPageURL)
	view.Run()
	close(stopCancellation)
	view.Destroy()
	report(webViewAttestationResult{err: fmt.Errorf("%w: WebView2 closed before hCaptcha completed", errMagicLinkAttestationUnavailable)})
}

func showMagicLinkWebViewWindow(window unsafe.Pointer) {
	if window == nil {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	showWindow := user32.NewProc("ShowWindow")
	setWindowPos := user32.NewProc("SetWindowPos")
	setForegroundWindow := user32.NewProc("SetForegroundWindow")
	const (
		swRestore     = 9
		swpNoSize     = 0x0001
		swpNoMove     = 0x0002
		swpShowWindow = 0x0040
	)
	_, _, _ = showWindow.Call(uintptr(window), swRestore)
	_, _, _ = setWindowPos.Call(uintptr(window), ^uintptr(0), 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
	_, _, _ = setForegroundWindow.Call(uintptr(window))
}

func cleanupMagicLinkWebViewProfile(path string) {
	if !isMagicLinkWebViewProfile(path) {
		return
	}
	_ = os.RemoveAll(path)
}

func isMagicLinkWebViewProfile(path string) bool {
	absPath, errAbs := filepath.Abs(path)
	if errAbs != nil {
		return false
	}
	tempPath, errTemp := filepath.Abs(os.TempDir())
	if errTemp != nil {
		return false
	}
	relative, errRel := filepath.Rel(tempPath, absPath)
	if errRel != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	if !strings.HasPrefix(filepath.Base(absPath), "cliproxy-claude-desktop-webview-") {
		return false
	}
	return true
}
