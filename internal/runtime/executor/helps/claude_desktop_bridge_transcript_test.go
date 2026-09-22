package helps

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func TestClaudeDesktopBridgeTranscriptProtectedMetadataAppend(t *testing.T) {
	store := NewClaudeDesktopTranscriptStore(t.TempDir(), "synthetic-egress")
	scope := strings.Repeat("b", 64)
	row := claudeprompt.SDKBridgeTranscriptRecord{Type: "bridge-session", SessionID: uuid.NewString(), BridgeSessionID: "cse_private", LastSequenceNum: 41,
		OwnerAccountUUID: "owner-account", OwnerOrganizationUUID: "owner-organization"}
	raw, _ := json.Marshal(row)
	raw = append(raw, '\n')
	revision, err := store.AppendTranscript(scope, "", raw)
	if err != nil {
		t.Fatal(err)
	}
	path, _, _ := store.path(scope)
	first, _ := os.ReadFile(path)
	revision, err = store.AppendTranscript(scope, revision, raw)
	if err != nil {
		t.Fatal("identical non-message metadata was incorrectly deduplicated", err)
	}
	all, _ := os.ReadFile(path)
	if !bytes.HasPrefix(all, first) || bytes.Contains(all, []byte("cse_private")) || bytes.Contains(all, []byte("owner-account")) {
		t.Fatal("metadata append rewrote or exposed protected records")
	}
	var read []byte
	index, err := store.ReadTranscript(scope, func(lines []byte) error { read = append(read, lines...); return nil })
	if err != nil || index.Revision != revision || len(index.UUIDs) != 0 || !bytes.Equal(read, append(bytes.Clone(raw), raw...)) {
		t.Fatal("metadata changed the message UUID index or lost exact append bytes", err)
	}
	for _, invalid := range [][]byte{
		bytes.ReplaceAll(raw, []byte(`"lastSequenceNum":41`), []byte(`"lastSequenceNum":null`)),
		bytes.ReplaceAll(raw, []byte(`"lastSequenceNum":41`), []byte(`"lastSequenceNum":-1`)),
		bytes.ReplaceAll(raw, []byte(`"lastSequenceNum":41`), []byte(`"lastSequenceNum":41,"worker_jwt":"invalid"`)),
	} {
		if _, err := store.AppendTranscript(scope, revision, invalid); err == nil {
			t.Fatal("invalid protected metadata accepted")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, all) {
			t.Fatal("invalid append altered protected history")
		}
	}
}
