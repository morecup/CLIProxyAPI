package prompt

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type sdkCompactVectors struct {
	ResponseText      string                             `json:"response_text"`
	NormalizedSummary string                             `json:"normalized_summary"`
	Normalization     []struct{ Input, Expected string } `json:"normalization"`
	Wrappers          []struct {
		Variant int    `json:"variant"`
		Text    string `json:"text"`
	} `json:"wrappers"`
}

func readSDKCompactVectors(t *testing.T) sdkCompactVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/sdk-compaction-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture sdkCompactVectors
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func sdkCompactJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compactResponseJSON(t *testing.T, text string) []byte {
	return sdkCompactJSON(t, map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn",
		"content": []map[string]any{{"type": "thinking", "thinking": "not summary"}, {"type": "text", "text": text}, {"type": "text", "text": "not the selected text"}}})
}

func compactResponseSSE(t *testing.T, text string) []string {
	t.Helper()
	return []string{
		`data: {"type":"message_start","message":{"role":"assistant"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		"data: " + string(sdkCompactJSON(t, map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": text}})),
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"not selected"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	}
}

func compactSummaryFixture(t *testing.T) (SDKCompactionSummary, sdkCompactVectors) {
	t.Helper()
	vectors := readSDKCompactVectors(t)
	var response SDKCompactionResponse
	response.ObserveJSON(compactResponseJSON(t, vectors.ResponseText))
	return response.TakeSummary("1.40609.0.0", "2.1.247"), vectors
}

func TestSDKCompactionNativeNormalizationAndWholeWrapperVectors(t *testing.T) {
	summary, vectors := compactSummaryFixture(t)
	if !summary.reviewed || summary.bytes != len(vectors.NormalizedSummary) || summary.sha256 != sdkCompactionHash(vectors.NormalizedSummary) {
		t.Fatal("summary fingerprint differs from the native synthetic output")
	}
	for index, row := range vectors.Normalization {
		if sdkNormalizeCompactionSummary(row.Input) != row.Expected {
			t.Errorf("normalization vector %d differs", index)
		}
	}
	for _, row := range vectors.Wrappers {
		t.Run(strconv.Itoa(row.Variant), func(t *testing.T) {
			if !summary.matches(row.Text) {
				t.Fatal("complete native synthetic wrapper did not match")
			}
			for _, altered := range []string{"Explain: " + row.Text, row.Text + "\nextra input", row.Text[:sdkCompactPrefixBytes+summary.bytes-1],
				strings.Replace(row.Text, vectors.NormalizedSummary, "different summary", 1)} {
				if summary.matches(altered) {
					t.Fatal("partial, quoted or changed wrapper was accepted")
				}
			}
		})
	}
}

func TestSDKCompactionResponseRequiresClosedSelectedText(t *testing.T) {
	_, vectors := compactSummaryFixture(t)
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			var response SDKCompactionResponse
			if mode == "json" {
				response.ObserveJSON(compactResponseJSON(t, vectors.ResponseText))
			} else {
				for _, line := range compactResponseSSE(t, vectors.ResponseText) {
					response.ObserveStreamLine([]byte(line))
				}
			}
			got := response.TakeSummary("1.40609.0.0", "2.1.247")
			if !got.matches(vectors.Wrappers[0].Text) || response.text != nil || response.blocks != nil {
				t.Fatal("first-text selection or transient response erasure failed")
			}
			if response.TakeSummary("1.40609.0.0", "2.1.247").reviewed {
				t.Fatal("a cleared response supplied another fingerprint")
			}
		})
	}
	for _, bad := range []string{"truncated", "unclosed", "duplicate-index", "wrong-role", "error", "malformed", "trailing", "empty", "too-large", "unknown-version"} {
		t.Run(bad, func(t *testing.T) {
			text := vectors.ResponseText
			if bad == "empty" {
				text = " "
			} else if bad == "too-large" {
				text = strings.Repeat("x", maxSDKCompactionTextBytes+1)
			}
			lines := compactResponseSSE(t, text)
			switch bad {
			case "truncated":
				lines = lines[:len(lines)-1]
			case "unclosed":
				lines = append(lines[:5], lines[6:]...)
			case "duplicate-index":
				lines[6] = strings.ReplaceAll(lines[6], `"index":2`, `"index":1`)
			case "wrong-role":
				lines[0] = strings.ReplaceAll(lines[0], "assistant", "user")
			case "error":
				lines[8] = `data: {"type":"error"}`
			case "malformed":
				lines[8] = `data: {`
			case "trailing":
				lines = append(lines, `data: {"type":"message_delta"}`)
			}
			var response SDKCompactionResponse
			for _, line := range lines {
				response.ObserveStreamLine([]byte(line))
			}
			version := "2.1.247"
			if bad == "unknown-version" {
				version = "unreviewed"
			}
			if response.TakeSummary("1.40609.0.0", version).reviewed || response.text != nil {
				t.Fatal("unsupported response supplied or retained summary content")
			}
		})
	}
}

func TestSDKCompactionActualCanonicalResponseAndAdoptedWrapper(t *testing.T) {
	root := os.Getenv("CLAUDE_DESKTOP_CAPTURE_CORPUS")
	if root == "" {
		t.Skip("requires an explicitly selected read-only recorder corpus")
	}
	dir := filepath.Join(root, "h7-v140609-message-opus-compaction-chunk-11", "flow-006302")
	meta, err := os.ReadFile(filepath.Join(dir, "response.json"))
	if err != nil {
		t.Fatal("canonical metadata unavailable")
	}
	var responseMeta struct {
		BodyFile string `json:"body_file"`
	}
	if json.Unmarshal(meta, &responseMeta) != nil || responseMeta.BodyFile == "" || filepath.Base(responseMeta.BodyFile) != responseMeta.BodyFile {
		t.Fatal("canonical response reference invalid")
	}
	raw, err := os.ReadFile(filepath.Join(dir, responseMeta.BodyFile))
	if err != nil || sdkCompactionHash(string(raw)) != "396452289917ef19f128575c2746a13af4dc59105fea955c4a1d551756d20c3c" {
		t.Fatal("canonical response hash mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal("canonical response gzip invalid")
	}
	decoded, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal("canonical response incomplete")
	}
	var response SDKCompactionResponse
	for _, line := range bytes.Split(decoded, []byte{'\n'}) {
		response.ObserveStreamLine(line)
	}
	summary := response.TakeSummary("1.40609.0.0", "2.1.247")
	if !summary.reviewed || summary.sha256 != "7a6fad4e8a782993da607047324c88d3aa00422d3862de32227830a855bc097f" {
		t.Fatal("canonical normalized summary mismatch")
	}
	adopted, err := os.ReadFile(filepath.Join(root, "h7-v140609-ui-compaction-paste-chunk-12", "flow-006317", "request-body.raw.bin"))
	if err != nil || sdkCompactionHash(string(adopted)) != "6f93d689ef0025f67808aab9c4fa374405c121df70d394b067e3b2eff92e963c" {
		t.Fatal("canonical adopted request hash mismatch")
	}
	history := observeSDKCompactionHistory(adopted)
	matches := 0
	for _, text := range history.firstUserTexts {
		if summary.matches(text) {
			matches++
		}
	}
	if !history.known || history.simple || matches != 1 {
		t.Fatal("canonical wrapper or nontrivial history boundary was misclassified")
	}
}

func TestSDKCompactionReplacementBudget(t *testing.T) {
	text := "left<summary>body</summary>right"
	start, end := 4, len(text)-5
	for _, row := range []struct{ token, want string }{
		{"$$", "$"}, {"$&", "<summary>body</summary>"}, {"$`", "left"}, {"$'", "right"},
		{"$1", "$1"}, {"$<x>", "$<x>"}, {"$$&", "$&"}, {"$", "$"},
	} {
		if got := sdkCompactJSReplacement(row.token, text, start, end, len(row.want)); got != row.want {
			t.Errorf("token %q: got %q, want %q", row.token, got, row.want)
		}
		if sdkCompactJSReplacement(row.token, text, start, end, len(row.want)-1) != "" {
			t.Errorf("token %q exceeded its replacement budget", row.token)
		}
	}
	for _, token := range []string{"$&", "$`", "$'"} {
		t.Run(token, func(t *testing.T) {
			input := strings.Repeat("L", 2048) + "<summary>" + strings.Repeat(token, 2048) + "</summary>" + strings.Repeat("R", 2048)
			var response SDKCompactionResponse
			response.ObserveJSON(compactResponseJSON(t, input))
			if response.TakeSummary("1.40609.0.0", "2.1.247").reviewed || response.text != nil {
				t.Fatal("replacement expansion escaped the observer budget")
			}
		})
	}
	// A single substitution fits, but the unchanged outer text pushes the
	// normalized response past the bound. That outer text is budgeted too.
	input := strings.Repeat("x", maxSDKCompactionTextBytes/2) + "<summary>$`</summary>"
	if sdkNormalizeCompactionSummary(input) != "" {
		t.Fatal("outer summary text was omitted from the replacement budget")
	}
	if got := sdkNormalizeCompactionSummary(strings.Repeat("x", maxSDKCompactionTextBytes)); len(got) != maxSDKCompactionTextBytes {
		t.Fatal("valid boundary-length summary was rejected")
	}
}
