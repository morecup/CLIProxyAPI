package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeDesktopStartupWiresUpdateTelemetryWithoutInference(t *testing.T) {
	doer := &claudeDesktopTelemetryTestDoer{}
	executor := newClaudeExecutorWithRuntime(&config.Config{}, claudeDesktopRuntimeOptions{
		statePath: t.TempDir(), appSessionID: "synthetic-update-app", enableStartup: true,
		startupDoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
			return claudeAccountStartupDoerFunc(func(*http.Request) (*http.Response, error) {
				body := "{}"
				if role == "startup-update-get" {
					body = `{"currentRelease":"1.40609.0","releases":[]}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		},
		telemetryEndpointDoerFactory: func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
			return claudetelemetry.HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				response, err := doer.Do(request)
				if role == "sdk-event-logging" || role == "datadog-logs" || role == "datadog-logs-browser" {
					response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
				}
				return response, err
			})
		},
	})
	t.Cleanup(executor.Close)
	auth := newClaudeDesktopTelemetryTestAuth(t)
	prepareH73RuntimeAuth(auth)
	if err := executor.Activate(auth); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for executor.StartupStatus().State != "ready" {
		if time.Now().After(deadline) {
			t.Fatalf("startup=%+v", executor.StartupStatus())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := executor.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://claude.ai/") {
			continue
		}
		var batch struct {
			Events []struct {
				Type string `json:"event_type"`
				Data struct {
					Name     string `json:"event_name"`
					Metadata string `json:"metadata"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(request.Body, &batch); err != nil {
			t.Fatal(err)
		}
		for _, event := range batch.Events {
			names = append(names, event.Data.Name)
			if event.Type == "ProductAnalyticsEvent" {
				// Renderer copies ($identify, page_viewed) carry Segment
				// properties, not the application metadata envelope
				// (desktop_renderer.go, desktop_renderer_session.go).
				continue
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(event.Data.Metadata), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["app_session_id"] != "synthetic-update-app" {
				t.Fatal("startup and telemetry application identity diverged")
			}
		}
	}
	// Main-process app-ready fact (Windows token probe; absent on other
	// hosts), the renderer shell load copies, then the update check.
	want := "$identify,page_viewed,desktop_update_check_started,desktop_update_not_available"
	if runtime.GOOS == "windows" {
		want = "desktop_windows_elevation_detected," + want
	}
	if strings.Join(names, ",") != want {
		t.Fatalf("application events=%v, want %s", names, want)
	}
}
