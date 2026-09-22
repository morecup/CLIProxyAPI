package startup

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// readUpdateResult deliberately recognizes only the captured no-update shape.
// A new version, unknown response, truncated body or decoding failure must not
// be converted into desktop_update_not_available. Limits are local resource
// guards, not claims about Desktop compression or update-selection policy.
func readUpdateResult(response *http.Response, limit int64, currentVersion string) (bool, error) {
	body, err := readStartupJSONBody(response, limit)
	if err != nil {
		return false, err
	}
	var result struct {
		CurrentRelease string            `json:"currentRelease"`
		Releases       []json.RawMessage `json:"releases"`
	}
	if errJSON := json.Unmarshal(body, &result); errJSON != nil {
		return false, errJSON
	}
	if result.CurrentRelease == "" || result.Releases == nil {
		return false, fmt.Errorf("update response has missing fields")
	}
	return result.CurrentRelease == currentVersion && len(result.Releases) == 0, nil
}

func readStartupJSONBody(response *http.Response, limit int64) ([]byte, error) {
	if limit <= 0 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("invalid startup body limit")
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, limit+1))
	errClose := response.Body.Close()
	if errRead != nil || errClose != nil {
		return nil, errors.Join(errRead, errClose)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("startup body exceeds limit")
	}
	encoding := strings.Join(response.Header.Values("Content-Encoding"), ",")
	if response.Uncompressed {
		encoding = ""
	}
	encodings := strings.Split(encoding, ",")
	if len(encodings) > 4 {
		return nil, fmt.Errorf("startup encoding stack exceeds limit")
	}
	for i := len(encodings) - 1; i >= 0; i-- {
		var reader io.Reader = bytes.NewReader(body)
		closeReader := func() error { return nil }
		switch strings.ToLower(strings.TrimSpace(encodings[i])) {
		case "", "identity":
			continue
		case "gzip":
			decoder, errDecode := gzip.NewReader(reader)
			if errDecode != nil {
				return nil, errDecode
			}
			reader, closeReader = decoder, decoder.Close
		case "deflate":
			decoder, errDecode := zlib.NewReader(reader)
			if errDecode != nil {
				return nil, errDecode
			}
			reader, closeReader = decoder, decoder.Close
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, errDecode := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
			if errDecode != nil {
				return nil, errDecode
			}
			reader = decoder
			closeReader = func() error { decoder.Close(); return nil }
		default:
			return nil, fmt.Errorf("unsupported startup content encoding")
		}
		body, errRead = io.ReadAll(io.LimitReader(reader, limit+1))
		errClose = closeReader()
		if errRead != nil || errClose != nil {
			return nil, errors.Join(errRead, errClose)
		}
		if int64(len(body)) > limit {
			return nil, fmt.Errorf("decoded startup body exceeds limit")
		}
	}
	return body, nil
}
