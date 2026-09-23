package claudedesktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	magicLinkChromiumPathEnv     = "CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_PATH"
	magicLinkChromiumHeadlessEnv = "CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_HEADLESS"
	magicLinkChromiumProfile     = "cliproxy-claude-desktop-chromium-"
)

type chromiumProxySettings struct {
	serverURL string
	bypass    bool
	bridge    *browserConnectProxy
}

func (s *chromiumProxySettings) Close() {
	if s != nil && s.bridge != nil {
		_ = s.bridge.Close()
	}
}

func acquireMagicLinkAttestationWithChromium(ctx context.Context, credentials magicLinkCredentials, options magicLinkAttestationOptions) (magicLinkAttestation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	executable, errExecutable := findMagicLinkChromiumExecutable()
	if errExecutable != nil {
		return magicLinkAttestation{}, errExecutable
	}
	headless, errHeadless := magicLinkChromiumHeadless()
	if errHeadless != nil {
		return magicLinkAttestation{}, errHeadless
	}
	proxySettings, errProxy := prepareChromiumProxy(ctx, options.ProxyURL)
	if errProxy != nil {
		return magicLinkAttestation{}, errProxy
	}
	defer proxySettings.Close()

	profilePath, errProfile := os.MkdirTemp("", magicLinkChromiumProfile)
	if errProfile != nil {
		return magicLinkAttestation{}, fmt.Errorf("create isolated Chromium profile: %w", errProfile)
	}
	defer cleanupMagicLinkChromiumProfile(profilePath)

	allocatorOptions := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(executable),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(profilePath),
		chromedp.WindowSize(900, 700),
		chromedp.WSURLReadTimeout(30 * time.Second),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-quic", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("safebrowsing-disable-auto-update", true),
		chromedp.Flag("use-mock-keychain", true),
	}
	if headless {
		allocatorOptions = append(allocatorOptions, chromedp.Headless)
	}
	if proxySettings.serverURL != "" {
		allocatorOptions = append(allocatorOptions, chromedp.ProxyServer(proxySettings.serverURL))
	} else if proxySettings.bypass {
		allocatorOptions = append(allocatorOptions, chromedp.Flag("no-proxy-server", true))
	}

	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(ctx, allocatorOptions...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()

	payloads := make(chan string, 1)
	chromedp.ListenTarget(browserCtx, func(event any) {
		binding, ok := event.(*cdpruntime.EventBindingCalled)
		if !ok || binding.Name != magicLinkAttestationBinding {
			return
		}
		select {
		case payloads <- binding.Payload:
		default:
		}
	})

	initialScript := magicLinkAttestationHook
	if anonymousID := strings.TrimSpace(credentials.AnonymousID); anonymousID != "" {
		initialScript += "\n" + magicLinkAnonymousCookieHook(anonymousID)
	}
	if errSetup := chromedp.Run(browserCtx, chromedp.ActionFunc(func(actionCtx context.Context) error {
		if errEnable := cdpruntime.Enable().Do(actionCtx); errEnable != nil {
			return errEnable
		}
		if errEnable := page.Enable().Do(actionCtx); errEnable != nil {
			return errEnable
		}
		if errBinding := cdpruntime.AddBinding(magicLinkAttestationBinding).Do(actionCtx); errBinding != nil {
			return errBinding
		}
		_, errScript := page.AddScriptToEvaluateOnNewDocument(initialScript).Do(actionCtx)
		return errScript
	})); errSetup != nil {
		return magicLinkAttestation{}, chromiumAttestationError(ctx, proxySettings, "start isolated Chromium", errSetup)
	}

	errNavigate := chromedp.Run(browserCtx, chromedp.Navigate(credentials.LoginPageURL))
	if errNavigate != nil {
		select {
		case raw := <-payloads:
			return decodeMagicLinkAttestationPayload(raw)
		default:
		}
		return magicLinkAttestation{}, chromiumAttestationError(ctx, proxySettings, "open Claude magic-link page", errNavigate)
	}

	select {
	case raw := <-payloads:
		return decodeMagicLinkAttestationPayload(raw)
	case <-browserCtx.Done():
		return magicLinkAttestation{}, chromiumAttestationError(ctx, proxySettings, "wait for hCaptcha", context.Cause(browserCtx))
	}
}

func prepareChromiumProxy(ctx context.Context, rawProxyURL string) (*chromiumProxySettings, error) {
	setting, errParse := proxyutil.Parse(rawProxyURL)
	if errParse != nil {
		return nil, fmt.Errorf("%w: invalid Chromium proxy configuration", errMagicLinkAttestationUnavailable)
	}
	if setting.Mode == proxyutil.ModeInherit {
		target := &url.URL{Scheme: "https", Host: "claude.ai"}
		environmentProxy, errEnvironment := http.ProxyFromEnvironment(&http.Request{URL: target})
		if errEnvironment != nil {
			return nil, fmt.Errorf("%w: resolve Chromium environment proxy", errMagicLinkAttestationUnavailable)
		}
		if environmentProxy == nil {
			return &chromiumProxySettings{bypass: true}, nil
		}
		rawProxyURL = environmentProxy.String()
		setting, errParse = proxyutil.Parse(rawProxyURL)
		if errParse != nil {
			return nil, fmt.Errorf("%w: invalid Chromium environment proxy", errMagicLinkAttestationUnavailable)
		}
	}
	if setting.Mode == proxyutil.ModeDirect {
		return &chromiumProxySettings{bypass: true}, nil
	}
	if setting.Mode != proxyutil.ModeProxy {
		return nil, fmt.Errorf("%w: unsupported Chromium proxy configuration", errMagicLinkAttestationUnavailable)
	}
	dialer, mode, errDialer := proxyutil.BuildDialer(rawProxyURL)
	if errDialer != nil || mode != proxyutil.ModeProxy || dialer == nil {
		return nil, fmt.Errorf("%w: initialize Chromium proxy bridge", errMagicLinkAttestationUnavailable)
	}
	bridge, errBridge := startBrowserConnectProxy(ctx, dialer)
	if errBridge != nil {
		return nil, fmt.Errorf("%w: %v", errMagicLinkAttestationUnavailable, errBridge)
	}
	return &chromiumProxySettings{serverURL: bridge.URL(), bridge: bridge}, nil
}

func chromiumAttestationError(ctx context.Context, proxySettings *chromiumProxySettings, stage string, cause error) error {
	if proxySettings != nil && proxySettings.bridge != nil {
		if errProxy := proxySettings.bridge.LastError(); errProxy != nil {
			return fmt.Errorf("%w: %s failed through the configured proxy: %v", errMagicLinkAttestationUnavailable, stage, errProxy)
		}
	}
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return fmt.Errorf("%w: Chromium did not complete hCaptcha before the login timeout", errMagicLinkAttestationUnavailable)
	}
	if cause == nil {
		return fmt.Errorf("%w: Chromium closed before hCaptcha completed", errMagicLinkAttestationUnavailable)
	}
	return fmt.Errorf("%w: %s", errMagicLinkAttestationUnavailable, stage)
}

func magicLinkChromiumHeadless() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(magicLinkChromiumHeadlessEnv))
	if raw == "" {
		return true, nil
	}
	switch strings.ToLower(raw) {
	case "yes", "on":
		return true, nil
	case "no", "off":
		return false, nil
	}
	value, errParse := strconv.ParseBool(raw)
	if errParse != nil {
		return false, fmt.Errorf("%w: %s must be true or false", errMagicLinkAttestationUnavailable, magicLinkChromiumHeadlessEnv)
	}
	return value, nil
}

func findMagicLinkChromiumExecutable() (string, error) {
	if override := strings.TrimSpace(os.Getenv(magicLinkChromiumPathEnv)); override != "" {
		if executable, errLookPath := exec.LookPath(override); errLookPath == nil {
			return executable, nil
		}
		return "", fmt.Errorf("%w: Chromium configured by %s was not found", errMagicLinkAttestationUnavailable, magicLinkChromiumPathEnv)
	}
	candidates := []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "headless-shell", "headless_shell"}
	if goruntime.GOOS == "darwin" {
		candidates = append([]string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}, candidates...)
	}
	for _, candidate := range candidates {
		if executable, errLookPath := exec.LookPath(candidate); errLookPath == nil {
			return executable, nil
		}
	}
	return "", fmt.Errorf("%w: Chromium was not found; install Chromium or set %s", errMagicLinkAttestationUnavailable, magicLinkChromiumPathEnv)
}

func cleanupMagicLinkChromiumProfile(path string) {
	if !isMagicLinkChromiumProfile(path) {
		return
	}
	_ = os.RemoveAll(path)
}

func isMagicLinkChromiumProfile(path string) bool {
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
	return strings.HasPrefix(filepath.Base(absPath), magicLinkChromiumProfile)
}
