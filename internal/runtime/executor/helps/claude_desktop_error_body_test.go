package helps

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type inspectedErrorBody struct {
	io.Reader
	closed int
}

func (b *inspectedErrorBody) Close() error { b.closed++; return nil }

func TestClaudeDesktopErrorInspectionPreservesBytesAndFailure(t *testing.T) {
	for _, mode := range []string{"complete", "oversized", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("synthetic read failure")
			var reader io.Reader = strings.NewReader("synthetic-body")
			if mode == "read-error" {
				reader = io.MultiReader(reader, &claudeDesktopReplayReadError{err: failure})
			}
			original := &inspectedErrorBody{Reader: reader}
			response := &http.Response{Body: original, Header: http.Header{"X-Synthetic": {"unchanged"}}, ContentLength: 1234}
			limit := int64(100)
			if mode == "oversized" {
				limit = 3
			}
			_, complete := PeekClaudeDesktopErrorBody(response, limit)
			if complete != (mode == "complete") || original.closed != 0 {
				t.Fatal("inspection accepted incomplete content or closed caller body")
			}
			body, err := io.ReadAll(response.Body)
			if string(body) != "synthetic-body" || (mode == "read-error") != errors.Is(err, failure) {
				t.Fatalf("changed caller-visible response: bytes=%q err=%v", body, err)
			}
			if response.Header.Get("X-Synthetic") != "unchanged" || response.ContentLength != 1234 {
				t.Fatal("inspection rewrote response headers")
			}
			if errClose := response.Body.Close(); errClose != nil || original.closed != 1 {
				t.Fatal("lost original close ownership")
			}
		})
	}
}

func TestClaudeDesktopDefaultCreditBetaClassifierAndStrip(t *testing.T) {
	header := http.Header{"aNtHrOpIc-BeTa": {"oauth-2025-04-20, fallback-credit-2026-06-01", "another-beta"}}
	body := []byte(`{"model":"claude-opus-5","messages":[]}`)
	if !CanStripDefaultClaudeDesktopCreditBeta(header, body) {
		t.Fatal("owned default credit beta was not recognized")
	}
	StripClaudeDesktopFallbackCreditBeta(header)
	if got := header["aNtHrOpIc-BeTa"]; len(got) != 2 || got[0] != "oauth-2025-04-20" || got[1] != "another-beta" {
		t.Fatalf("strip changed unrelated beta values: %v", got)
	}
	for name, input := range map[string]struct {
		header http.Header
		body   string
	}{
		"fallbacks":        {http.Header{"Anthropic-Beta": {ClaudeDesktopFallbackCreditBeta}}, `{"fallbacks":[]}`},
		"credit token":     {http.Header{"Anthropic-Beta": {ClaudeDesktopFallbackCreditBeta}}, `{"fallback_credit_token":"token"}`},
		"companion header": {http.Header{"Anthropic-Beta": {ClaudeDesktopFallbackCreditBeta + ",server-side-fallback-test"}}, `{}`},
		"missing header":   {http.Header{}, `{}`},
		"invalid body":     {http.Header{"Anthropic-Beta": {ClaudeDesktopFallbackCreditBeta}}, `{`},
	} {
		t.Run(name, func(t *testing.T) {
			if CanStripDefaultClaudeDesktopCreditBeta(input.header, []byte(input.body)) {
				t.Fatal("unsupported credit mode was admitted")
			}
		})
	}
	accepted := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"fallback-credit-2026-06-01 is not allowed in anthropic-beta"}}`)
	if !ClaudeDesktopCreditBetaRejected(accepted) {
		t.Fatal("attributed string rejection was not classified")
	}
	for _, rejected := range [][]byte{
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"unrelated"}}`),
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":{"text":"fallback-credit-2026-06-01 anthropic-beta"}}}`),
		[]byte(`{"type":"error","error":{"type":"authentication_error","message":"fallback-credit-2026-06-01 anthropic-beta"}}`),
	} {
		if ClaudeDesktopCreditBetaRejected(rejected) {
			t.Fatal("unattributed or non-string rejection was classified")
		}
	}
}
