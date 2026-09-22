package helps

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func TestClaudeDesktopSDKSessionStoreProtectedRoundTripAndScope(t *testing.T) {
	root := t.TempDir()
	store := NewClaudeDesktopSDKSessionStore(root, "synthetic-egress")
	scope := strings.Repeat("a", 64)
	payload := []byte(`{"synthetic":"PRIVATE_SESSION_MARKER","version":1}`)
	revision, err := store.Save(scope, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.path(scope)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(file, []byte("PRIVATE_SESSION_MARKER")) || bytes.Contains(file, []byte(scope)) {
		t.Fatal("state leaked outside protected envelope")
	}
	restored, actual, err := NewClaudeDesktopSDKSessionStore(root, "synthetic-egress").Load(scope)
	if err != nil || actual != revision || !bytes.Equal(restored, payload) {
		t.Fatal("restart did not restore exact checkpoint", err)
	}
	for _, variant := range []struct{ scope, egress string }{
		{strings.Repeat("b", 64), "synthetic-egress"}, {scope, "foreign-egress"},
	} {
		other := NewClaudeDesktopSDKSessionStore(root, variant.egress)
		restored, revision, err := other.Load(variant.scope)
		if err != nil || len(restored) != 0 || revision != "" {
			t.Fatal("scope isolation failed", err)
		}
		foreignPath, _, _ := other.path(variant.scope)
		if err := os.WriteFile(foreignPath, file, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := other.Load(variant.scope); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
			t.Fatal("copied protected record bypassed scope check", err)
		}
	}
	for _, scope := range []string{"", "../escape", strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		if _, err := store.Save(scope, "", payload); err == nil {
			t.Fatal("unvalidated path scope accepted")
		}
	}
}

func TestClaudeDesktopFeatureStoreIndependentProtectedNamespace(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("d", 64)
	store := NewClaudeDesktopFeatureStore(root, "synthetic-egress")
	payload := []byte(`{"version":1,"values":{"flag":"PRIVATE_FEATURE_MARKER"}}`)
	revision, err := store.Save(scope, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.path(scope)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(path)) != "sdk-features" {
		t.Fatal("wrong cache namespace")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PRIVATE_FEATURE_MARKER")) || bytes.Contains(raw, []byte(scope)) {
		t.Fatal("feature cache leaked plaintext")
	}
	got, actual, err := NewClaudeDesktopFeatureStore(root, "synthetic-egress").Load(scope)
	if err != nil || actual != revision || !bytes.Equal(got, payload) {
		t.Fatal("feature cache did not restore", err)
	}
	for _, other := range []*ClaudeDesktopSDKSessionStore{NewClaudeDesktopSDKSessionStore(root, "synthetic-egress"), NewClaudeDesktopNativeContentStore(root, "synthetic-egress"), NewClaudeDesktopFeatureStore(root, "other-egress")} {
		if p, r, err := other.Load(scope); err != nil || len(p) != 0 || r != "" {
			t.Fatal("cache crossed ownership namespace", err)
		}
	}
	if _, err := store.Save(scope, "", payload); !errors.Is(err, claudeprompt.ErrSDKSessionStale) {
		t.Fatal("feature cache bypassed revision guard", err)
	}
}

func TestClaudeDesktopSessionAliasStoreProtectedNamespace(t *testing.T) {
	root, scope := t.TempDir(), strings.Repeat("e", 64)
	store := NewClaudeDesktopSessionAliasStore(root, "egress-a")
	payload := []byte(`{"version":1,"session_id":"12345678-1234-4234-8234-123456789abc"}`)
	revision, err := store.Save(scope, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.path(scope)
	if err != nil || filepath.Base(filepath.Dir(path)) != "sdk-session-aliases" {
		t.Fatal("alias is not separately namespaced", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("12345678")) || bytes.Contains(raw, []byte(scope)) {
		t.Fatal("native session leaked outside the protected envelope", err)
	}
	got, actual, err := NewClaudeDesktopSessionAliasStore(root, "egress-a").Load(scope)
	if err != nil || actual != revision || !bytes.Equal(got, payload) {
		t.Fatal("protected alias did not survive store reconstruction", err)
	}
	for _, other := range []*ClaudeDesktopSDKSessionStore{NewClaudeDesktopSDKSessionStore(root, "egress-a"), NewClaudeDesktopNativeContentStore(root, "egress-a"), NewClaudeDesktopFeatureStore(root, "egress-a"), NewClaudeDesktopSessionAliasStore(root, "egress-b")} {
		if p, r, err := other.Load(scope); err != nil || len(p) != 0 || r != "" {
			t.Fatal("alias crossed a persistence namespace", err)
		}
	}
	if _, err := store.Save(scope, "", payload); !errors.Is(err, claudeprompt.ErrSDKSessionStale) {
		t.Fatal("alias persistence bypassed compare-and-swap", err)
	}
}

func TestClaudeDesktopSDKSessionStoreCASAndCorruption(t *testing.T) {
	store := NewClaudeDesktopSDKSessionStore(t.TempDir(), "")
	scope := strings.Repeat("c", 64)
	revision, err := store.Save(scope, "", []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, stale := 0, 0
	for range 8 {
		wg.Go(func() {
			_, err := store.Save(scope, revision, []byte(`{"value":2}`))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if errors.Is(err, claudeprompt.ErrSDKSessionStale) {
				stale++
			} else {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if successes != 1 || stale != 7 {
		t.Fatal("obsolete writers overwrote each other", successes, stale)
	}
	path, _, _ := store.path(scope)
	corrupt := []byte("synthetic-corrupt-record")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(scope); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
		t.Fatal(err)
	}
	if _, err := store.Save(scope, revision, []byte(`{"value":3}`)); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
		t.Fatal("corruption was overwritten", err)
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, corrupt) {
		t.Fatal("original diagnostic record changed")
	}
}

func TestClaudeDesktopSDKSessionStoreLostRecordCannotBeRecreatedByStaleWriter(t *testing.T) {
	store := NewClaudeDesktopSDKSessionStore(t.TempDir(), "")
	scope := strings.Repeat("d", 64)
	revision, err := store.Save(scope, "", []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	path, _, _ := store.path(scope)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(scope, revision, []byte(`{"value":2}`)); !errors.Is(err, claudeprompt.ErrSDKSessionStale) {
		t.Fatal("lost revision silently reset", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale writer recreated missing file")
	}
}

func TestClaudeDesktopSDKSessionStoreWriteFailureIsExplicit(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewClaudeDesktopSDKSessionStore(blocker, "")
	if _, err := store.Save(strings.Repeat("e", 64), "", []byte(`{"value":1}`)); err == nil {
		t.Fatal("unwritable store silently succeeded")
	}
	store = NewClaudeDesktopSDKSessionStore(root, "")
	for _, payload := range [][]byte{nil, []byte("invalid JSON"), bytes.Repeat([]byte(" "), claudeprompt.MaxSDKSessionStateBytes+1)} {
		if _, err := store.Save(strings.Repeat("e", 64), "", payload); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
			t.Fatal("invalid checkpoint accepted", err)
		}
	}
}

func TestClaudeDesktopNativeContentStoreIsSeparateProtectedAndBound(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("f", 64)
	native := NewClaudeDesktopNativeContentStore(root, "synthetic-egress")
	structural := NewClaudeDesktopSDKSessionStore(root, "synthetic-egress")
	payload := []byte(`{"message":{"content":"PRIVATE_NATIVE_CONTENT"}}`)
	revision, err := native.Save(scope, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	nativePath, binding, _ := native.path(scope)
	structuralPath, structuralBinding, _ := structural.path(scope)
	if filepath.Dir(nativePath) == filepath.Dir(structuralPath) || binding == structuralBinding {
		t.Fatal("content and structural state share a storage domain")
	}
	encoded, err := os.ReadFile(nativePath)
	if err != nil || bytes.Contains(encoded, []byte("PRIVATE_NATIVE_CONTENT")) {
		t.Fatal("native content is not protected at rest", err)
	}
	actual, restoredRevision, err := NewClaudeDesktopNativeContentStore(root, "synthetic-egress").Load(scope)
	if err != nil || restoredRevision != revision || !bytes.Equal(actual, payload) {
		t.Fatal("protected native content did not roundtrip", err)
	}
	if actual, _, err := structural.Load(scope); err != nil || len(actual) != 0 {
		t.Fatal("structural store read native content")
	}
	if err := os.MkdirAll(filepath.Dir(structuralPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(structuralPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := structural.Load(scope); !errors.Is(err, claudeprompt.ErrSDKSessionInvalid) {
		t.Fatal("native protected envelope was transplantable into structural state", err)
	}
}
