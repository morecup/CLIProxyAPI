package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

// TestClaudeDesktopCurrentCaptureProbe is a temporary, opt-in diagnostic used
// during live acceptance. It reports only structural facts and hashes; it does
// not print captured prompt text, response text, or credentials.
func TestClaudeDesktopCurrentCaptureProbe(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CLAUDE_DESKTOP_CAPTURE_BODY"))
	if path == "" {
		t.Skip("CLAUDE_DESKTOP_CAPTURE_BODY is not set")
	}
	source, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}

	executor := NewClaudeExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	var outgoing []byte
	var outgoingHeaders http.Header
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/claude_cli/bootstrap":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"client_data":{"experimentKey":"probe","atis":"0123456789abcdef"},"oauth_account":{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"22222222-2222-4222-8222-222222222222"}}`)),
				Request:    request,
			}, nil
		case "/v1/messages/count_tokens":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    request,
			}, nil
		default:
			var errBody error
			outgoing, errBody = io.ReadAll(request.Body)
			if errBody != nil {
				return nil, errBody
			}
			outgoingHeaders = request.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "request-id": []string{"req_probe"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_probe\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")),
				Request:    request,
			}, nil
		}
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(string(source)))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	applyCapturedCodeHeaders(t, request.Header, filepath.Join(filepath.Dir(path), "request.json"))
	response, errDo := executor.HttpRequest(ctx, auth, request)
	if errDo != nil {
		t.Fatal(errDo)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if len(outgoing) == 0 {
		t.Fatal("no outgoing Messages body was captured")
	}

	t.Logf("source_bytes=%d source_sha256=%s outgoing_bytes=%d outgoing_sha256=%s", len(source), digestHex(source), len(outgoing), digestHex(outgoing))
	for _, key := range []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking", "context_management", "output_config", "diagnostics", "stream"} {
		sourceValue, outgoingValue := gjson.GetBytes(source, key), gjson.GetBytes(outgoing, key)
		t.Logf("field=%s source_present=%t outgoing_present=%t exact=%t", key, sourceValue.Exists(), outgoingValue.Exists(), sourceValue.Raw == outgoingValue.Raw)
	}
	t.Logf("source_cache=%s outgoing_cache=%s", strings.Join(cacheControlPaths(source), ","), strings.Join(cacheControlPaths(outgoing), ","))
	t.Logf("source_system_hashes=%s outgoing_system_hashes=%s", strings.Join(systemTextHashes(source), ","), strings.Join(systemTextHashes(outgoing), ","))
	t.Logf("source_messages=%d outgoing_messages=%d source_tools=%d outgoing_tools=%d", len(gjson.GetBytes(source, "messages").Array()), len(gjson.GetBytes(outgoing, "messages").Array()), len(gjson.GetBytes(source, "tools").Array()), len(gjson.GetBytes(outgoing, "tools").Array()))

	headerNames := make([]string, 0, len(outgoingHeaders))
	for name := range outgoingHeaders {
		headerNames = append(headerNames, strings.ToLower(name))
	}
	sort.Strings(headerNames)
	t.Logf("headers=%s", strings.Join(headerNames, ","))
	t.Logf("user_agent=%q client_platform=%q client_version=%q request_class=%q x_app=%q has_atis=%t",
		headerValueFold(outgoingHeaders, "User-Agent"),
		headerValueFold(outgoingHeaders, "anthropic-client-platform"),
		headerValueFold(outgoingHeaders, "anthropic-client-version"),
		headerValueFold(outgoingHeaders, "x-claude-code-request-class"),
		headerValueFold(outgoingHeaders, "x-app"),
		headerValueFold(outgoingHeaders, "x-cc-atis") != "",
	)
}

func applyCapturedCodeHeaders(t *testing.T, destination http.Header, path string) {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var metadata struct {
		Headers [][]json.RawMessage `json:"headers"`
	}
	if errJSON := json.Unmarshal(raw, &metadata); errJSON != nil {
		t.Fatal(errJSON)
	}
	for _, pair := range metadata.Headers {
		if len(pair) != 2 {
			continue
		}
		var headerName string
		if errName := json.Unmarshal(pair[0], &headerName); errName != nil || !claudeDesktopCodeHeaderAllowed(headerName) {
			continue
		}
		if strings.EqualFold(headerName, "authorization") || strings.EqualFold(headerName, "x-cc-atis") || strings.EqualFold(headerName, "host") || strings.EqualFold(headerName, "content-length") || strings.EqualFold(headerName, "connection") {
			continue
		}
		var values []string
		if errValues := json.Unmarshal(pair[1], &values); errValues == nil && len(values) > 0 {
			destination[headerName] = append([]string(nil), values...)
			continue
		}
		var value string
		if errValue := json.Unmarshal(pair[1], &value); errValue == nil {
			destination.Set(headerName, value)
		}
	}
}

func digestHex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func cacheControlPaths(payload []byte) []string {
	var paths []string
	for index, block := range gjson.GetBytes(payload, "system").Array() {
		if block.Get("cache_control").Exists() {
			paths = append(paths, "system["+jsonNumber(index)+"]:"+cacheShape(block.Get("cache_control")))
		}
	}
	for messageIndex, message := range gjson.GetBytes(payload, "messages").Array() {
		for contentIndex, block := range message.Get("content").Array() {
			if block.Get("cache_control").Exists() {
				paths = append(paths, "messages["+jsonNumber(messageIndex)+"].content["+jsonNumber(contentIndex)+"]:"+cacheShape(block.Get("cache_control")))
			}
		}
	}
	for index, tool := range gjson.GetBytes(payload, "tools").Array() {
		if tool.Get("cache_control").Exists() {
			paths = append(paths, "tools["+jsonNumber(index)+"]:"+cacheShape(tool.Get("cache_control")))
		}
	}
	return paths
}

func cacheShape(cache gjson.Result) string {
	keys := make([]string, 0, 3)
	cache.ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, key.String())
		return true
	})
	sort.Strings(keys)
	return strings.Join(keys, "+")
}

func systemTextHashes(payload []byte) []string {
	blocks := gjson.GetBytes(payload, "system").Array()
	hashes := make([]string, 0, len(blocks))
	for _, block := range blocks {
		hashes = append(hashes, digestHex([]byte(block.Get("text").String()))[:16])
	}
	return hashes
}

func jsonNumber(value int) string {
	return strconv.Itoa(value)
}
