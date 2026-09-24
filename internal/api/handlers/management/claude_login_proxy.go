package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	maxClaudeLoginProxyBody      = 8 << 10
	maxClaudeLoginProxyURLLength = 4096
)

type claudeLoginProxyRequest struct {
	ProxyURL string `json:"proxy_url"`
}

func readClaudeLoginProxy(c *gin.Context) (string, error) {
	if c == nil || c.Request == nil || c.Request.Method != http.MethodPost {
		return "", nil
	}

	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxClaudeLoginProxyBody))
	decoder.DisallowUnknownFields()
	var body claudeLoginProxyRequest
	if errDecode := decoder.Decode(&body); errDecode != nil {
		if errors.Is(errDecode, io.EOF) {
			return "", nil
		}
		return "", errors.New("invalid Claude login proxy request")
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		return "", errors.New("invalid Claude login proxy request")
	}
	return normalizeClaudeLoginProxyURL(body.ProxyURL)
}

func normalizeClaudeLoginProxyURL(raw string) (string, error) {
	proxyURL := strings.TrimSpace(raw)
	if proxyURL == "" {
		return "", nil
	}
	if len(proxyURL) > maxClaudeLoginProxyURLLength {
		return "", fmt.Errorf("Claude login proxy URL is too long")
	}
	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil || setting.Mode == proxyutil.ModeInvalid || setting.Mode == proxyutil.ModeInherit {
		return "", fmt.Errorf("Claude login proxy must be direct, none, or a valid http, https, socks5, or socks5h URL")
	}
	return proxyURL, nil
}

func cloneClaudeLoginConfig(cfg *config.Config, proxyURL string) *config.Config {
	if cfg == nil {
		return nil
	}
	cloned := cfg.CloneForRuntime()
	if proxyURL != "" {
		cloned.ProxyURL = proxyURL
	}
	return cloned
}

func applyClaudeLoginProxy(record *coreauth.Auth, proxyURL string) {
	if record == nil || proxyURL == "" {
		return
	}
	record.ProxyURL = proxyURL
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	record.Metadata["proxy_url"] = proxyURL
}
