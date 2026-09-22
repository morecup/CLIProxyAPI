package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

func TestBridgeRecordCheckpointSurvivesQueryReplacement(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	host, first, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	g := r.BridgeForHost(host)
	if err := g.MatchQuery(first.ID, first.QueryID, first.SDKSessionID); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		identity := []string{first.ID, first.QueryID, first.SDKSessionID}
		identity[index] = "foreign"
		if err := g.MatchQuery(identity[0], identity[1], identity[2]); !errors.Is(err, ErrStaleQuery) {
			t.Fatal("bridge grant accepted foreign request facts", err)
		}
	}
	value := BridgeState{SessionID: "cse_owned", LastSequenceNum: 41, OwnerAccountUUID: "account", OwnerOrganizationUUID: "org",
		DeclaredDialogKinds: []string{"permission"}, SessionGroupingID: "group", NoHistoryBackfill: true}
	if err := g.Save(value); err != nil {
		t.Fatal(err)
	}
	value.DeclaredDialogKinds[0] = "mutated"
	loaded, err := g.Load("account", "org")
	if err != nil || loaded.DeclaredDialogKinds[0] != "permission" {
		t.Fatal("checkpoint aliases its producer", err)
	}
	loaded.DeclaredDialogKinds[0] = "also-mutated"
	if _, err := g.Load("foreign", "org"); !errors.Is(err, ErrBridgeOwner) {
		t.Fatal("foreign login read saved bridge", err)
	}
	if _, err := g.Load("account", "foreign"); !errors.Is(err, ErrBridgeOwner) {
		t.Fatal("foreign organization read saved bridge", err)
	}
	before := bytes.Clone(store.payload)
	value.OwnerAccountUUID = "foreign"
	if err := g.Save(value); !errors.Is(err, ErrBridgeOwner) || !bytes.Equal(before, store.payload) {
		t.Fatal("changed login replaced saved record", err)
	}
	host.Close()
	value.OwnerAccountUUID, value.LastSequenceNum = "account", 53
	value.DeclaredDialogKinds[0] = "permission"
	if err := g.Save(value); err != nil {
		t.Fatal("final cleanup could not checkpoint cancelled generation", err)
	}
	restored := NewRegistry(store)
	next, second, err := restored.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if first.QueryID == second.QueryID || first.ID != second.ID || first.SDKSessionID != second.SDKSessionID {
		t.Fatal("durable identity and query generation were conflated")
	}
	checkpoint, err := restored.BridgeForHost(next).Load("account", "org")
	if err != nil || checkpoint.LastSequenceNum != 53 || checkpoint.SessionID != "cse_owned" || !checkpoint.NoHistoryBackfill || checkpoint.SessionGroupingID != "group" || checkpoint.DeclaredDialogKinds[0] != "permission" {
		t.Fatal("saved bridge was not reconstructed independently of query state", checkpoint, err)
	}
	public, _ := json.Marshal(second)
	for _, secret := range []string{"cse_owned", "account", "org", "permission", "group", "lastSequenceNum"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatal("private bridge metadata reached management snapshot")
		}
	}
}

func TestBridgeRecordFailedWritePreservesCheckpointAndBinding(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner := strings.Repeat("a", 64)
	host, _, _ := r.Resolve(t.Context(), owner, strings.Repeat("b", 64), startTestHost(uuid.NewString()))
	defer host.Close()
	g := r.BridgeForHost(host)
	value := BridgeState{SessionID: "cse_owned", LastSequenceNum: 41, OwnerAccountUUID: "account", OwnerOrganizationUUID: "org"}
	if err := g.Save(value); err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(store.payload)
	store.err = errors.New("synthetic write failure")
	value.SessionID, value.LastSequenceNum = "cse_replacement", 0
	if err := g.Save(value); err == nil {
		t.Fatal("missing checkpoint write error")
	}
	if err := r.BindBridge(host, "cse_replacement"); err == nil {
		t.Fatal("missing binding write error")
	}
	loaded, err := g.Load("account", "org")
	if err != nil || loaded.SessionID != "cse_owned" || loaded.LastSequenceNum != 41 || !bytes.Equal(before, store.payload) {
		t.Fatal("failed save changed the published checkpoint", err)
	}
	store.err = nil
	if err := g.Save(value); err != nil {
		t.Fatal(err)
	}
	loaded, _ = g.Load("account", "org")
	if loaded.SessionID != "cse_replacement" || loaded.LastSequenceNum != 0 {
		t.Fatal("replacement inherited old cursor")
	}
	sibling, _, _ := r.Resolve(t.Context(), owner, strings.Repeat("c", 64), startTestHost(uuid.NewString()))
	defer sibling.Close()
	if err := r.BridgeForHost(sibling).Save(value); !errors.Is(err, ErrRemoteBound) {
		t.Fatal("sibling stole bridge binding", err)
	}
}

func TestBridgeRecordAdmissionJoinsCleanupOutsideRecordLock(t *testing.T) {
	r := NewRegistry(&testStore{})
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	host, _, _ := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	defer host.Close()
	g := r.BridgeForHost(host)
	done := make(chan struct{})
	if err := g.BindDone(done); err != nil {
		t.Fatal(err)
	}
	host.Close()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	start := func(*string) (*features.Host, error) { return nil, errors.New("successor admitted before cleanup") }
	go func() { _, _, err := r.Resolve(ctx, owner, scope, start); result <- err }()
	// A checkpoint needs this same lock while Resolve waits. This write must
	// finish and remain visible to the successor, rather than deadlocking Stop.
	checkpointed := make(chan error, 1)
	go func() {
		checkpointed <- g.Save(BridgeState{SessionID: "cse_owned", LastSequenceNum: 61, OwnerAccountUUID: "account", OwnerOrganizationUUID: "org"})
	}()
	select {
	case err := <-checkpointed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission held record lock while waiting for cleanup")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission ignored caller cancellation")
	}
	close(done)
	next, _, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	checkpoint, err := r.BridgeForHost(next).Load("account", "org")
	if err != nil || checkpoint.LastSequenceNum != 61 {
		t.Fatal("successor missed final checkpoint", err)
	}
	if err := g.Save(*checkpoint); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("old query overwrote successor", err)
	}
}

func TestBridgeRecordUnverifiedTranscriptCannotAdoptSavedBridge(t *testing.T) {
	r := NewRegistry(&testStore{})
	host, _, err := r.Resolve(t.Context(), strings.Repeat("a", 64), strings.Repeat("b", 64), func(*string) (*features.Host, error) {
		return features.NewHost(nil, uuid.NewString()), errors.New("synthetic unresolved transcript alias")
	})
	if !errors.Is(err, ErrResumeUnverified) || host == nil {
		t.Fatal(err)
	}
	defer host.Close()
	g := r.BridgeForHost(host)
	if _, err := g.Load("account", "org"); !errors.Is(err, ErrResumeUnverified) {
		t.Fatal("unverified SDK session adopted a bridge", err)
	}
	if err := g.Prepare("cse_new"); !errors.Is(err, ErrResumeUnverified) {
		t.Fatal("unverified SDK session reserved a replacement", err)
	}
}
