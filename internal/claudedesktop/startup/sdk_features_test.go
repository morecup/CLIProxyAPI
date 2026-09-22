package startup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestStartupSDKFeatureResponseReachesOwnedObserver(t *testing.T) {
	for _, scenario := range []string{"identity", "gzip", "truncated", "too-large", "observer-failure", "http-error"} {
		t.Run(scenario, func(t *testing.T) {
			bundle, err := claudeprofile.Load("")
			if err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range bundle.Startup.Endpoints {
				if endpoint.EndpointRole == "startup-sdk-eval" {
					bundle.Startup.Endpoints = []claudeprofile.StartupEndpointProfile{endpoint}
					break
				}
			}
			auth := startupTestAuth(t, true)
			payload := []byte(`{"features":{"tengu_kestrel_moor":{"value":false}}}`)
			wire := bytes.Clone(payload)
			encoding := ""
			if scenario == "gzip" || scenario == "truncated" {
				var buffer bytes.Buffer
				writer := gzip.NewWriter(&buffer)
				_, _ = writer.Write(payload)
				_ = writer.Close()
				wire, encoding = buffer.Bytes(), "gzip"
				if scenario == "truncated" {
					wire = wire[:len(wire)-5]
				}
			}
			if scenario == "too-large" {
				bundle.Startup.MaxResponseBytes = int64(len(payload) - 1)
			}
			var calls atomic.Int32
			manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle,
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
					return startupDoerFunc(func(*http.Request) (*http.Response, error) {
						status := http.StatusOK
						if scenario == "http-error" {
							status = http.StatusBadGateway
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Encoding": {encoding}}, Body: io.NopCloser(bytes.NewReader(wire))}, nil
					}), nil
				},
				SDKFeaturesObserver: func(observed *cliproxyauth.Auth, body []byte) error {
					calls.Add(1)
					if observed == auth || observed.ID != auth.ID || oauthAccessToken(observed) != oauthAccessToken(auth) || !bytes.Equal(body, payload) {
						t.Error("feature observer lost its owned auth snapshot or received undecoded/incomplete bytes")
					}
					if scenario == "observer-failure" {
						return errors.New("synthetic protected feature store failure")
					}
					return nil
				}})
			t.Cleanup(manager.Close)
			manager.Activate(auth)
			waitForStartupStatus(t, manager, func(status Status) bool { return status.Completed+status.Failed == 1 })
			status := manager.Status()
			wantCalls := int32(0)
			if scenario == "identity" || scenario == "gzip" || scenario == "observer-failure" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatal("feature response was lost or an invalid body reached the observer")
			}
			if scenario == "identity" || scenario == "gzip" {
				if status.State != "ready" || status.Completed != 1 {
					t.Fatal("accepted SDK feature response did not finish startup")
				}
			} else if status.State != "degraded" || status.Failed != 1 {
				t.Fatal("feature decode/persistence failure disappeared from startup health")
			}
		})
	}
}
