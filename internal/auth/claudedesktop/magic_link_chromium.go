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

	cdpinput "github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	magicLinkChromiumPathEnv      = "CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_PATH"
	magicLinkChromiumHeadlessEnv  = "CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_HEADLESS"
	magicLinkChromiumNoSandboxEnv = "CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_NO_SANDBOX"
	magicLinkChromiumProfile      = "cliproxy-claude-desktop-chromium-"
	magicLinkChromiumWidth        = 900
	magicLinkChromiumHeight       = 700
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
	noSandbox, errNoSandbox := magicLinkChromiumNoSandbox()
	if errNoSandbox != nil {
		return magicLinkAttestation{}, errNoSandbox
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
		chromedp.WindowSize(magicLinkChromiumWidth, magicLinkChromiumHeight),
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
	if noSandbox {
		allocatorOptions = append(allocatorOptions, chromedp.NoSandbox)
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

	var interactive *MagicLinkBrowserSession
	interactiveState := strings.TrimSpace(options.InteractiveSessionID)
	if interactiveState != "" {
		interactive = registerMagicLinkBrowserSession(interactiveState, cancelBrowser, func(event MagicLinkBrowserInput) error {
			return dispatchMagicLinkBrowserInput(browserCtx, event)
		})
		if interactive == nil {
			return magicLinkAttestation{}, fmt.Errorf("%w: initialize interactive Chromium session", errMagicLinkAttestationUnavailable)
		}
		defer unregisterMagicLinkBrowserSession(interactiveState, interactive)
	}

	payloads := make(chan string, 1)
	chromedp.ListenTarget(browserCtx, func(event any) {
		switch value := event.(type) {
		case *cdpruntime.EventBindingCalled:
			if value.Name != magicLinkAttestationBinding {
				return
			}
			select {
			case payloads <- value.Payload:
			default:
			}
		case *page.EventScreencastFrame:
			if interactive == nil {
				return
			}
			width, height := float64(magicLinkChromiumWidth), float64(magicLinkChromiumHeight)
			if value.Metadata != nil {
				width = value.Metadata.DeviceWidth
				height = value.Metadata.DeviceHeight
			}
			interactive.publishFrame(value.Data, width, height)
			go acknowledgeMagicLinkBrowserFrame(browserCtx, value.SessionID)
		case *page.EventFrameNavigated:
			if interactive == nil || value.Frame == nil || value.Frame.ParentID != "" {
				return
			}
			if !magicLinkBrowserMainFrameAllowed(value.Frame.URL) {
				interactive.abort("Claude verification navigated outside the allowed page")
			}
		}
	})

	if interactive != nil {
		errScreencast := chromedp.Run(browserCtx, chromedp.ActionFunc(func(actionCtx context.Context) error {
			if errFront := page.BringToFront().Do(actionCtx); errFront != nil {
				return errFront
			}
			return page.StartScreencast().
				WithFormat(page.ScreencastFormatJpeg).
				WithQuality(72).
				WithMaxWidth(magicLinkChromiumWidth).
				WithMaxHeight(magicLinkChromiumHeight).
				WithEveryNthFrame(2).
				Do(actionCtx)
		}))
		if errScreencast != nil {
			return magicLinkAttestation{}, chromiumAttestationError(ctx, proxySettings, "start Claude verification stream", errScreencast)
		}
		interactive.markReady(magicLinkChromiumWidth, magicLinkChromiumHeight)
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
		if interactive != nil {
			if reason := interactive.CloseReason(); reason != "" {
				return magicLinkAttestation{}, fmt.Errorf("%w: %s", errMagicLinkAttestationUnavailable, reason)
			}
		}
		return magicLinkAttestation{}, chromiumAttestationError(ctx, proxySettings, "wait for hCaptcha", context.Cause(browserCtx))
	}
}

func dispatchMagicLinkBrowserInput(browserCtx context.Context, event MagicLinkBrowserInput) error {
	if browserCtx == nil {
		return errMagicLinkBrowserSessionUnavailable
	}
	return chromedp.Run(browserCtx, chromedp.ActionFunc(func(actionCtx context.Context) error {
		var command *cdpinput.DispatchMouseEventParams
		switch event.Action {
		case "move":
			command = cdpinput.DispatchMouseEvent(cdpinput.MouseMoved, event.X, event.Y)
		case "down":
			clickCount := event.ClickCount
			if clickCount == 0 {
				clickCount = 1
			}
			command = cdpinput.DispatchMouseEvent(cdpinput.MousePressed, event.X, event.Y).
				WithButton(cdpinput.Left).
				WithButtons(1).
				WithClickCount(clickCount)
		case "up":
			clickCount := event.ClickCount
			if clickCount == 0 {
				clickCount = 1
			}
			command = cdpinput.DispatchMouseEvent(cdpinput.MouseReleased, event.X, event.Y).
				WithButton(cdpinput.Left).
				WithClickCount(clickCount)
		case "wheel":
			command = cdpinput.DispatchMouseEvent(cdpinput.MouseWheel, event.X, event.Y).
				WithDeltaX(event.DeltaX).
				WithDeltaY(event.DeltaY)
		default:
			return fmt.Errorf("unsupported Claude verification pointer action")
		}
		return command.Do(actionCtx)
	}))
}

func acknowledgeMagicLinkBrowserFrame(browserCtx context.Context, sessionID int64) {
	if browserCtx == nil {
		return
	}
	_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(actionCtx context.Context) error {
		return page.ScreencastFrameAck(sessionID).Do(actionCtx)
	}))
}

func magicLinkBrowserMainFrameAllowed(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || rawURL == "about:blank" {
		return true
	}
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil || parsed == nil {
		return false
	}
	if parsed.Scheme == "chrome-error" {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "claude.ai" || strings.HasSuffix(host, ".claude.ai")
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
	return fmt.Errorf("%w: %s: %v", errMagicLinkAttestationUnavailable, stage, cause)
}

func magicLinkChromiumHeadless() (bool, error) {
	return magicLinkChromiumBoolEnv(magicLinkChromiumHeadlessEnv, true)
}

func magicLinkChromiumNoSandbox() (bool, error) {
	return magicLinkChromiumBoolEnv(magicLinkChromiumNoSandboxEnv, false)
}

func magicLinkChromiumBoolEnv(name string, defaultValue bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue, nil
	}
	switch strings.ToLower(raw) {
	case "yes", "on":
		return true, nil
	case "no", "off":
		return false, nil
	}
	value, errParse := strconv.ParseBool(raw)
	if errParse != nil {
		return false, fmt.Errorf("%w: %s must be true or false", errMagicLinkAttestationUnavailable, name)
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
	candidates := []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "chrome", "msedge", "headless-shell", "headless_shell"}
	if goruntime.GOOS == "windows" {
		var windowsCandidates []string
		for _, base := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			base = strings.TrimSpace(base)
			if base == "" {
				continue
			}
			windowsCandidates = append(windowsCandidates,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			)
		}
		candidates = append(windowsCandidates, candidates...)
	}
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
