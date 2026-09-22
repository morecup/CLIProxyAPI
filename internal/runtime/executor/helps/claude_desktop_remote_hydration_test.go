package helps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestRemoteHydrationStoreHasIndependentProtectedBinding(t *testing.T) {
	root := t.TempDir()
	sum := sha256.Sum256([]byte("owned-remote-transcript"))
	scope := hex.EncodeToString(sum[:])
	remote := NewClaudeDesktopRemoteTranscriptStore(root, "egress-a")
	payload := []byte(`{"private_transcript":"DO_NOT_EXPOSE_REMOTE_HISTORY"}`)
	revision, err := remote.Save(scope, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := remote.path(scope)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("DO_NOT_EXPOSE_REMOTE_HISTORY")) {
		t.Fatal("remote history not protected", err)
	}
	for _, other := range []*ClaudeDesktopSDKSessionStore{
		NewClaudeDesktopSDKSessionStore(root, "egress-a"), NewClaudeDesktopNativeContentStore(root, "egress-a"),
		NewClaudeDesktopRemoteTranscriptStore(root, "egress-b"), NewClaudeDesktopSessionRecordStore(root, "egress-a"),
	} {
		body, otherRevision, err := other.Load(scope)
		if err != nil || len(body) != 0 || otherRevision != "" {
			t.Fatal("remote history crossed namespace or egress")
		}
	}
	got, gotRevision, err := NewClaudeDesktopRemoteTranscriptStore(root, "egress-a").Load(scope)
	if err != nil || gotRevision != revision || !bytes.Equal(got, payload) {
		t.Fatal("protected restoration failed", err)
	}
}
