package claudedesktop

import (
	"encoding/json"
	"fmt"
	"strings"
)

const magicLinkAttestationBinding = "cliproxyClaudeDesktopAttestation"

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

type magicLinkAttestationOptions struct {
	ProxyURL             string
	InteractiveSessionID string
}

type browserAttestationPayload struct {
	HCaptchaToken string `json:"hcaptcha_token"`
	Locale        string `json:"locale"`
	UserAgent     string `json:"user_agent"`
}

func decodeMagicLinkAttestationPayload(raw string) (magicLinkAttestation, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxAttestationSize+2048 {
		return magicLinkAttestation{}, errMagicLinkAttestationUnavailable
	}
	var payload browserAttestationPayload
	if errDecode := json.Unmarshal([]byte(raw), &payload); errDecode != nil {
		return magicLinkAttestation{}, fmt.Errorf("%w: invalid browser payload", errMagicLinkAttestationUnavailable)
	}
	return normalizeMagicLinkAttestation(magicLinkAttestation{
		HCaptchaToken: payload.HCaptchaToken,
		Locale:        payload.Locale,
		UserAgent:     payload.UserAgent,
	})
}

func normalizeMagicLinkAttestation(attestation magicLinkAttestation) (magicLinkAttestation, error) {
	attestation.HCaptchaToken = strings.TrimSpace(attestation.HCaptchaToken)
	attestation.Locale = strings.TrimSpace(attestation.Locale)
	attestation.UserAgent = strings.TrimSpace(attestation.UserAgent)
	if attestation.HCaptchaToken == "" || len(attestation.HCaptchaToken) > maxAttestationSize || len(attestation.Locale) > 64 || len(attestation.UserAgent) > 1024 {
		return magicLinkAttestation{}, errMagicLinkAttestationUnavailable
	}
	return attestation, nil
}

func magicLinkAnonymousCookieHook(anonymousID string) string {
	encodedAnonymousID, _ := json.Marshal(strings.TrimSpace(anonymousID))
	return `(function () {
  if (location.protocol === "https:" && location.hostname === "claude.ai") {
    document.cookie = "_cross_domain_anonymous_id=" + encodeURIComponent(` + string(encodedAnonymousID) + `) + "; Path=/; Max-Age=31536000; SameSite=Lax; Secure";
  }
})();`
}
