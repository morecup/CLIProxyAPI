package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func TestClaudeDesktopExecutorCarriesQueryLineageWithoutTitleInheritance(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleSubagent, claudeprofile.RoleLightHelper, claudeprofile.RoleTitle} {
		t.Run(string(role), func(t *testing.T) {
			bundle, errBundle := claudeprofile.BuiltinV140609()
			if errBundle != nil {
				t.Fatal(errBundle)
			}
			doer := &claudeDesktopTelemetryTestDoer{}
			manager := claudetelemetry.NewManager(claudetelemetry.Options{
				StatePath: t.TempDir(), Bundle: bundle,
				DoerFactory: func(string) claudetelemetry.HTTPDoer { return doer },
			})
			t.Cleanup(manager.Close)
			executor := &ClaudeExecutor{desktopTelemetry: manager}
			facts := claudeDesktopRuntimeFacts{
				SessionID: "44444444-4444-4444-8444-444444444444", PromptID: "55555555-5555-4555-8555-555555555555",
				ClientRequestID: "66666666-6666-4666-8666-666666666666", LogicalModel: "claude-sonnet-5",
			}
			const chain = "77777777-7777-4777-8777-777777777777"
			body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"synthetic"}]}`)
			span := executor.beginClaudeDesktopTelemetry(context.Background(), newClaudeDesktopTelemetryTestAuth(t), role, facts, body, map[string]any{
				helps.ClaudeDesktopQueryChainIDMetadataKey: chain,
				helps.ClaudeDesktopQueryDepthMetadataKey:   2,
			})
			span.ObserveRequest(body, nil)
			span.ObserveResponse("req_synthetic", "end_turn")
			span.FinishSuccess(context.Background())
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			found := 0
			for _, request := range doer.Requests() {
				var batch struct {
					Events []struct {
						Data struct {
							Name     string `json:"event_name"`
							Metadata string `json:"additional_metadata"`
						} `json:"event_data"`
					} `json:"events"`
				}
				if json.Unmarshal(request.Body, &batch) != nil {
					continue
				}
				for _, event := range batch.Events {
					if event.Data.Name != "tengu_api_query" && event.Data.Name != "tengu_api_success" {
						continue
					}
					decoded, errDecode := base64.StdEncoding.DecodeString(event.Data.Metadata)
					if errDecode != nil {
						t.Fatal(errDecode)
					}
					var metadata map[string]any
					if errJSON := json.Unmarshal(decoded, &metadata); errJSON != nil {
						t.Fatal(errJSON)
					}
					if role == claudeprofile.RoleTitle {
						if metadata["queryChainId"] != nil || metadata["queryDepth"] != nil {
							t.Fatal("title inherited main lineage")
						}
					} else if metadata["queryChainId"] != chain || metadata["queryDepth"] != float64(2) {
						t.Fatalf("%s lost caller query position: %v", event.Data.Name, metadata)
					}
					found++
				}
			}
			if found != 2 {
				t.Fatalf("query/success events = %d, want 2", found)
			}
		})
	}
}
