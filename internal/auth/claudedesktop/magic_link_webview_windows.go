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

const magicLinkAttestationHelperArg = "--claude-desktop-attestation-helper"

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

func acquireMagicLinkAttestation(ctx context.Context, credentials magicLinkCredentials, options magicLinkAttestationOptions) (magicLinkAttestation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(options.InteractiveSessionID) != "" {
		return acquireMagicLinkAttestationWithChromium(ctx, credentials, options)
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
	attestation, errAttestation := normalizeMagicLinkAttestation(magicLinkAttestation{
		HCaptchaToken: strings.TrimSpace(response.HCaptchaToken),
		Locale:        strings.TrimSpace(response.Locale),
		UserAgent:     strings.TrimSpace(response.UserAgent),
	})
	if errAttestation != nil {
		return magicLinkAttestation{}, errAttestation
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
	if errBind := view.Bind(magicLinkAttestationBinding, func(raw string) (bool, error) {
		attestation, errDecode := decodeMagicLinkAttestationPayload(raw)
		if errDecode != nil {
			report(webViewAttestationResult{err: errDecode})
			return false, nil
		}
		report(webViewAttestationResult{attestation: attestation})
		return true, nil
	}); errBind != nil {
		view.Destroy()
		result <- webViewAttestationResult{err: fmt.Errorf("bind Claude Desktop WebView attestation bridge: %w", errBind)}
		return
	}
	view.Init(magicLinkAttestationHook)
	if anonymousID := strings.TrimSpace(credentials.AnonymousID); anonymousID != "" {
		view.Init(magicLinkAnonymousCookieHook(anonymousID))
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
