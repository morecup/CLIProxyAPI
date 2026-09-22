package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRemoteUnarchiveDispositionAndOwnership(t *testing.T) {
	for _, test := range []struct {
		code              int
		resource          string
		success, fallback bool
	}{
		{200, "", true, false}, {204, "", true, false}, {409, "", true, false},
		{400, "", false, true}, {403, "", false, true}, {404, "", false, true},
		{403, "untrusted_device", false, false}, {403, "session_stale_relogin", false, false},
		{429, "", false, false}, {502, "", false, false},
	} {
		t.Run(fmt.Sprintf("%d/%s", test.code, test.resource), func(t *testing.T) {
			bundle, err := claudeprofile.BuiltinV140609()
			if err != nil {
				t.Fatal(err)
			}
			r := claudesessions.NewRegistry(nil)
			grant, value, err := r.CreateRemote(t.Context(), strings.Repeat("a", 64), "cse_remote", `C:\synthetic`, "claude-sonnet-5", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
			if err != nil {
				t.Fatal(err)
			}
			_, host, _, _, _, _ := grant.Read()
			defer host.Close()
			var requests []string
			m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
				return credentialBudgetDoerFunc(func(req *http.Request) (*http.Response, error) {
					requests = append(requests, req.Method+" "+req.URL.Path)
					body, code := `{}`, 200
					switch {
					case strings.HasSuffix(req.URL.Path, "/unarchive"):
						if req.URL.Path != "/v1/sessions/session_remote/unarchive" {
							t.Error("wrong compatibility route")
						}
						payload, _ := io.ReadAll(req.Body)
						if string(payload) != "{}" {
							t.Error("invented unarchive body")
						}
						code = test.code
						body = fmt.Sprintf(`{"error":{"resource":%q}}`, test.resource)
					case req.URL.Path == "/v1/code/sessions":
						body = `{"session":{"id":"cse_replacement"}}`
					case strings.HasSuffix(req.URL.Path, "/bridge"):
						body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
					}
					return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}), nil
			}})
			err = m.AttachRemoteQuery(t.Context(), testDesktopAuth(t), grant, &InboundConsumer{}, func(id string) error { return r.BindBridge(host, id) })
			if (err == nil) != test.success {
				t.Fatal("wrong unarchive disposition", err)
			}
			if test.fallback && !errors.Is(err, claudesessions.ErrRemoteMismatch) {
				t.Fatal("replacement did not detach origin", err)
			}
			if test.success {
				before := len(requests)
				if err := m.AttachRemoteQuery(t.Context(), testDesktopAuth(t), grant, &InboundConsumer{}, func(id string) error { return r.BindBridge(host, id) }); err != nil || len(requests) != before {
					t.Fatal("duplicate attachment restarted worker", err)
				}
			}
			host.Close()
			if err := m.RetireQuery(t.Context(), value.ID, value.QueryID); err != nil {
				t.Fatal(err)
			}
			m.Close()
			joined := strings.Join(requests, "\n")
			if strings.Contains(joined, "POST /v1/code/sessions\n") != test.fallback {
				t.Fatal("wrong remint", joined)
			}
			if strings.Contains(joined, "/session_remote/archive") != test.success || strings.Contains(joined, "/session_replacement/archive") != test.fallback {
				t.Fatal("cleanup crossed original/replacement ownership", joined)
			}
		})
	}
}

func TestRemoteUnarchiveRefreshUsesCurrentOwnerOnce(t *testing.T) {
	bundle, _ := claudeprofile.BuiltinV140609()
	auth := testActiveControlAuth(t)
	updated := auth.Clone()
	updated.Metadata["access_token"] = "updated-access"
	credentials := &testControlCredentials{current: auth}
	credentials.refresh = func(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		credentials.current = updated
		return updated, nil
	}
	r := claudesessions.NewRegistry(nil)
	grant, _, err := r.CreateRemote(t.Context(), strings.Repeat("b", 64), "cse_remote", `C:\synthetic`, "claude-sonnet-5", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
	if err != nil {
		t.Fatal(err)
	}
	_, host, _, _, _, _ := grant.Read()
	defer host.Close()
	var calls int
	m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, Credentials: credentials, DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return credentialBudgetDoerFunc(func(req *http.Request) (*http.Response, error) {
			body, code := `{}`, 200
			if strings.HasSuffix(req.URL.Path, "/unarchive") {
				calls++
				if calls == 1 {
					code = 401
				} else if req.Header.Get("Authorization") != "Bearer updated-access" {
					t.Error("stale credential replay")
				}
			}
			if strings.HasSuffix(req.URL.Path, "/bridge") {
				body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
			}
			return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}), nil
	}})
	defer m.Close()
	if err := m.AttachRemoteQuery(t.Context(), auth, grant, &InboundConsumer{}, func(id string) error { return r.BindBridge(host, id) }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || credentials.refreshes != 1 {
		t.Fatal("wrong refresh budget", calls, credentials.refreshes)
	}
}
