package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type homeOwnedCredentialExecutor struct{ cliproxyauth.ProviderExecutor }

func (*homeOwnedCredentialExecutor) Identifier() string { return "claude" }
func (*homeOwnedCredentialExecutor) Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return nil, errors.New("application executor must not be reentered by teardown")
}

type homeOwnedControlDoer func(*http.Request) (*http.Response, error)

func (d homeOwnedControlDoer) Do(request *http.Request) (*http.Response, error) { return d(request) }

func TestDesktopRegisteredHomeTeardownUsesActualRefreshParser(t *testing.T) {
	for _, variant := range []string{"same-owner", "remote-source", "omitted-source", "no-local-refresh-token", "incomplete-remote-secrets", "bare-auth", "foreign-id", "foreign-index", "foreign-account", "foreign-proxy", "disabled", "missing-binding", "acquisition-error", "authority-disabled"} {
		t.Run(variant, func(t *testing.T) {
			identity := claudedesktop.AccountIdentity{AccountUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", OrganizationUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
			device := claudedesktop.TrustedDevice{DeviceID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", DeviceToken: "synthetic-device", DisplayName: "Synthetic Desktop"}
			id, err := claudedesktop.StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
			if err != nil {
				t.Fatal(err)
			}
			enrollment := claudedesktop.NewEnrollment(id, identity, device, time.Now())
			enrollment.State = claudedesktop.EnrollmentActive
			auth := &cliproxyauth.Auth{ID: id, Provider: "claude", Status: cliproxyauth.StatusActive, FileName: "synthetic-home-desktop.json", Label: "local label", Metadata: map[string]any{
				"access_token": "sk-ant-oat-synthetic-old", "refresh_token": "synthetic-refresh", "type": "claude", "account_uuid": identity.AccountUUID, "organization_uuid": identity.OrganizationUUID,
				"claude_device_ids": []string{claudedesktop.RequestDeviceID(device.DeviceID)}, "binding_number": json.Number("18446744073709551615"),
				claudedesktop.MetadataAuthFlowKey: claudedesktop.AuthFlowDesktop, claudedesktop.MetadataEnrollmentKey: enrollment, claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			}}
			if variant == "no-local-refresh-token" || variant == "incomplete-remote-secrets" {
				delete(auth.Metadata, "refresh_token")
			}
			store := sdkauth.NewFileTokenStore()
			storeDirectory := t.TempDir()
			store.SetBaseDir(storeDirectory)
			registry := cliproxyauth.NewManager(store, nil, nil)
			cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
			registry.SetConfig(cfg)
			owner := &homeOwnedCredentialExecutor{}
			registry.RegisterExecutor(owner)
			registered, err := registry.Register(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			remote := registered.Clone()
			remote.Metadata["access_token"] = "sk-ant-oat-synthetic-new"
			if variant == "no-local-refresh-token" {
				remote.Metadata["refresh_token"] = "synthetic-home-refresh"
			}
			remote.Label = "remote label"
			index := registered.Index
			switch variant {
			case "remote-source":
				remote.Attributes[cliproxyauth.AttributePath] = "/synthetic-home-store/remote.json"
				remote.Attributes[cliproxyauth.AttributeSource] = "/synthetic-home-store/remote.json"
				remote.Attributes[cliproxyauth.AttributeSourceBackend] = "remote-store"
			case "omitted-source":
				delete(remote.Attributes, cliproxyauth.AttributePath)
				delete(remote.Attributes, cliproxyauth.AttributeSource)
				delete(remote.Attributes, cliproxyauth.AttributeSourceBackend)
			case "foreign-id":
				remote.ID = "foreign"
			case "foreign-index":
				index = "foreign"
			case "foreign-account":
				remote.Metadata["account_uuid"] = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			case "foreign-proxy":
				remote.ProxyURL = "http://127.0.0.1:1"
			case "disabled":
				remote.Disabled = true
			case "missing-binding":
				delete(remote.Metadata, claudedesktop.MetadataEnrollmentKey)
			}
			var response any = homeRefreshAuthEnvelope{Auth: *remote, AuthIndex: index}
			if variant == "bare-auth" {
				response = remote
			}
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			client := &fakeHomeRefreshClient{raw: raw}
			if variant == "acquisition-error" {
				client.err = errors.New("synthetic-private-error")
			}
			previous := currentHomeRefreshClient
			currentHomeRefreshClient = func() homeRefreshClient { return client }
			t.Cleanup(func() { currentHomeRefreshClient = previous })
			credentials := &ClaudeDesktopControlCredentials{Manager: registry, Owner: owner, Acquire: func(ctx context.Context, input *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
				updated, handled, err := RefreshAuthViaHome(ctx, cfg, input)
				if !handled {
					t.Fatal("actual Home acquisition path was not used")
				}
				if variant == "authority-disabled" {
					registry.SetConfig(&config.Config{})
				}
				return updated, err
			}}
			bundle, err := claudeprofile.BuiltinV140609()
			if err != nil {
				t.Fatal(err)
			}
			archives := 0
			control := claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: bundle, Credentials: credentials, DisableLoops: true,
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
					return homeOwnedControlDoer(func(request *http.Request) (*http.Response, error) {
						payload, status := `{}`, http.StatusOK
						switch {
						case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
							payload = `{"session":{"id":"cse_home-owned"}}`
						case strings.HasSuffix(request.URL.Path, "/bridge"):
							payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
						case strings.HasSuffix(request.URL.Path, "/archive"):
							archives++
							if archives == 1 {
								status = http.StatusUnauthorized
							} else if request.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic-new" {
								return nil, fmt.Errorf("teardown did not use the Home-refreshed token")
							}
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
					}), nil
				}})
			t.Cleanup(control.Close)
			if err = control.EnsureSession(t.Context(), registered, "synthetic-sdk-session", "claude-opus-4-6"); err != nil {
				t.Fatal(err)
			}
			control.Close()
			accepted := variant == "same-owner" || variant == "remote-source" || variant == "omitted-source" || variant == "no-local-refresh-token" || variant == "bare-auth"
			if (archives == 2) != accepted || (control.Status().Failed == 0) != accepted || client.calls.Load() != 1 {
				t.Fatal("Home teardown outcome mismatch", archives, client.calls.Load(), control.Status())
			}
			current, _ := registry.GetByID(id)
			if (current.Metadata["access_token"] == "sk-ant-oat-synthetic-new") != accepted || current.FileName != registered.FileName || current.Label != registered.Label || current.Index != registered.Index {
				t.Fatal("Home refresh did not retain the local registered owner")
			}
			for _, key := range []string{cliproxyauth.AttributePath, cliproxyauth.AttributeSource, cliproxyauth.AttributeSourceBackend} {
				expected := registered.Attributes[key]
				if expected == "" && accepted {
					// A memory-only credential gains local annotations on its first
					// complete durable save, never from the Home response's path.
					expected = filepath.Join(storeDirectory, registered.FileName)
					if key == cliproxyauth.AttributeSourceBackend {
						expected = cliproxyauth.AuthSourceFile
					}
				}
				if current.Attributes[key] != expected {
					t.Fatal("Home refresh replaced a local storage annotation")
				}
			}
			if client.authIndex != registered.Index || client.accessTokenHash != authAccessTokenSHA256(registered) {
				t.Fatal("Home acquisition targeted a different credential")
			}
			if accepted {
				if _, err = credentials.Current(current); err != nil {
					t.Fatal("accepted Home credentials cannot be reused", err)
				}
				path := current.Attributes[cliproxyauth.AttributePath]
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "sk-ant-oat-synthetic-new") || strings.Contains(string(encoded), "synthetic-home-refresh") {
					t.Fatal("Home credentials were persisted in plaintext")
				}
				var metadata map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(encoded)))
				decoder.UseNumber()
				if err = decoder.Decode(&metadata); err != nil {
					t.Fatal(err)
				}
				if err = claudedesktop.HydrateMetadata(path, metadata); err != nil || metadata["access_token"] != current.Metadata["access_token"] || metadata["refresh_token"] != current.Metadata["refresh_token"] || metadata["binding_number"] != json.Number("18446744073709551615") {
					t.Fatal("Home refresh did not persist complete exact credentials", err)
				}
			}
		})
	}
}
