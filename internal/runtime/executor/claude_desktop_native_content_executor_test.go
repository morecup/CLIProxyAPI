package executor

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// Observe production output, without asking the runtime to pre-create the
// owner that the default entrypoint itself is responsible for establishing.
func observeDefaultSDKSession(t *testing.T, request *http.Request, session *string) {
	t.Helper()
	owner, _ := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	if owner.manager == nil || owner.host == nil || owner.accountID == "" || owner.profileID == "" || owner.host.Context().Err() != nil || owner.session != owner.host.SessionID() {
		t.Fatal("default outgoing main request lost its scoped live query owner")
	}
	if request.GetBody == nil {
		t.Fatal("default wire request has no replayable body")
	}
	reader, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	errClose := reader.Close()
	if err != nil || errClose != nil {
		t.Fatal(err, errClose)
	}
	native := gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()
	id, err := uuid.Parse(native)
	if err != nil || id == uuid.Nil || helps.HeaderValueCaseInsensitive(request.Header, "X-Claude-Code-Session-Id") != native {
		t.Fatal("wire metadata and session header do not share a native UUID")
	}
	if *session == "" {
		*session = native
	} else if *session != native {
		t.Fatal("another turn, helper or resumed runtime changed the SDK session")
	}
}

func assertDefaultNativeContent(t *testing.T, content claudeprompt.SDKNativeContentSnapshot, history claudeprompt.SDKHistorySnapshot, session string) {
	t.Helper()
	if content.PersistenceError || content.IncompleteReason != "" || len(content.ActiveUUIDs) != len(history.Messages) {
		t.Fatalf("default native content ownership incomplete: rows=%d active=%d history=%d issue=%q write_error=%v", len(content.Messages), len(content.ActiveUUIDs), len(history.Messages), content.IncompleteReason, content.PersistenceError)
	}
	byID := make(map[string]claudeprompt.SDKNativeMessage)
	for _, row := range content.Messages {
		if row.SessionID != session || row.Version != "2.1.247" || row.Entrypoint != "claude-desktop" || row.Cwd == "" || row.IsSidechain || row.UserType != "external" || row.SessionKind != "" {
			t.Fatal("default native content has a foreign session or missing runtime decoration")
		}
		if at, err := time.Parse("2006-01-02T15:04:05.000Z", row.Timestamp); err != nil || at.IsZero() {
			t.Fatal("native yield timestamp is not owned")
		}
		if _, duplicate := byID[row.UUID]; duplicate {
			t.Fatal("native message was appended twice")
		}
		byID[row.UUID] = row
		if row.Type == "system" {
			if row.Subtype != "compact_boundary" || row.ParentUUID != nil || row.LogicalParentUUID == nil {
				t.Fatal("adopted compact boundary lost physical/logical parent distinction")
			}
		} else if !json.Valid(row.Message) && !json.Valid(row.Attachment) {
			t.Fatal("native content is absent")
		}
	}
	for index, message := range history.Messages {
		row, exists := byID[message.UUID]
		if !exists || content.ActiveUUIDs[index] != message.UUID || row.Type != message.Type {
			t.Fatal("active native content differs from canonical history")
		}
	}
	if len(history.Messages) > 1 {
		last := byID[history.Messages[len(history.Messages)-1].UUID]
		if last.Type == "assistant" && (last.ParentUUID == nil || *last.ParentUUID != history.Messages[len(history.Messages)-2].UUID) {
			t.Fatal("latest assistant parent does not follow the active post-compaction history")
		}
	}
}
