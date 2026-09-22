package helps

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type sdkAccountingFailingStore struct {
	claudeprompt.SDKSessionStore
	fail bool
}

func (s *sdkAccountingFailingStore) Save(scope, revision string, payload []byte) (string, error) {
	if s.fail {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	return s.SDKSessionStore.Save(scope, revision, payload)
}

var sdkAccountingBody = []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic input"}]}`)
var sdkAccountingReply = []byte(`{"type":"message","role":"assistant","id":"synthetic-response","content":[{"type":"text","text":"<summary>PRIVATE_SUMMARY</summary>"}],"stop_reason":"end_turn"}`)

func accountingFixture(t *testing.T, role claudeprofile.RequestRole) (*ClaudeDesktopSDKAccounting, *claudeprompt.Request, time.Time) {
	t.Helper()
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	var tracker claudeprompt.Tracker
	auth := &cliproxyauth.Auth{ID: "synthetic-account"}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	scope, _ := json.Marshal([]string{auth.ID, bundle.ProfileID, auth.ProxyURL})
	owner := tracker.Begin(claudeprompt.Input{AccountID: string(scope), SessionID: "synthetic-session", Role: "main", ClientRequestID: uuid.NewString(), StartedAt: start, Body: sdkAccountingBody})
	owner.ObserveSDKQuery(sdkAccountingBody)
	var main *claudeprompt.Request
	if role == claudeprofile.RoleMain {
		main = owner
	}
	accounting := BeginClaudeDesktopSDKAccounting(&tracker, auth, bundle, main, role, "synthetic-session", owner.Identity().PromptID, uuid.NewString(), start, start.Add(-2500*time.Millisecond))
	return accounting, owner, start
}

func TestClaudeDesktopSDKAccountingRolesWithoutTelemetry(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleCompaction, claudeprofile.RoleSecurityMonitor, claudeprofile.RoleWebSearchHelper, claudeprofile.RoleSubagent} {
		t.Run(string(role), func(t *testing.T) {
			a, owner, start := accountingFixture(t, role)
			a.ObserveRequest(sdkAccountingBody)
			a.ObserveHTTPResponse(200)
			a.ObservePayload(sdkAccountingReply, false)
			end := start.Add(500*time.Millisecond + 500*time.Microsecond)
			if role == claudeprofile.RoleMain {
				owner.FinishSuccess(end, "end_turn", nil)
			}
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() { a.FinishSuccess(end) })
			}
			group.Wait()
			result := owner.Snapshot().SDK
			want := int64(3001)
			if role == claudeprofile.RoleSecurityMonitor || role == claudeprofile.RoleWebSearchHelper || role == claudeprofile.RoleSubagent {
				want = 0
			}
			if result.APIDurationMS != want {
				t.Fatalf("role ledger=%d want=%d", result.APIDurationMS, want)
			}
			a.helper.RecordSuccess("late-after-completion", "unowned", 999)
			if owner.Snapshot().SDK != result {
				t.Fatal("completed accounting retained a mutable helper lease")
			}
			if result.SawCompact || result.PendingCompactions != map[bool]int{true: 1}[role == claudeprofile.RoleCompaction] {
				t.Fatal("helper success was adopted prematurely or disposition was lost")
			}
			if role == claudeprofile.RoleWebSearchHelper || role == claudeprofile.RoleSubagent {
				if result.IncompleteReason != "unobserved-sdk-helper-boundary" {
					t.Fatal("unsupported helper silently claimed complete accounting")
				}
			}
			encoded, _ := json.Marshal(a)
			if strings.Contains(string(encoded), "PRIVATE_") || string(encoded) != "{}" {
				t.Fatal("private summary serialized")
			}
			if summary := a.compactionResponse.TakeSummary("1.40609.0.0", "2.1.247"); summary != (claudeprompt.SDKCompactionSummary{}) {
				t.Fatal("completed observer retained selected text")
			}
		})
	}
}

func TestClaudeDesktopSDKAccountingRejectsUncompletedOrUnownedSuccess(t *testing.T) {
	for _, scenario := range []string{"failure", "no-request", "no-headers", "http-error", "truncated", "no-parent", "failure-then-late-success"} {
		t.Run(scenario, func(t *testing.T) {
			a, owner, start := accountingFixture(t, claudeprofile.RoleCompaction)
			if scenario != "no-request" {
				a.ObserveRequest(sdkAccountingBody)
			}
			if scenario != "no-headers" {
				status := 200
				if scenario == "http-error" {
					status = 502
				}
				a.ObserveHTTPResponse(status)
			}
			if scenario == "truncated" {
				a.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"role":"assistant"}}`))
			} else {
				a.ObservePayload(sdkAccountingReply, false)
			}
			if scenario == "no-parent" {
				a.helper.Close()
				a.helper = nil
			}
			if scenario == "failure" || scenario == "failure-then-late-success" {
				a.FinishFailure()
			}
			a.FinishSuccess(start.Add(time.Second))
			a.ObservePayload(sdkAccountingReply, false)
			a.FinishSuccess(start.Add(2 * time.Second))
			a.helper.RecordSuccess("late-after-failure", "unowned", 999)
			if got := owner.Snapshot().SDK; got.APIDurationMS != 0 || got.PendingCompactions != 0 || got.SawCompact {
				t.Fatalf("uncompleted/unowned callback mutated accounting: %+v", got)
			}
		})
	}
}

func TestClaudeDesktopSDKAccountingKeepsRestoreFailureWithoutHelperOwner(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{true, false} {
		root := t.TempDir()
		if unavailable {
			root = ""
		}
		tracker := claudeprompt.NewTracker(NewClaudeDesktopSDKSessionStore(root, ""))
		auth := &cliproxyauth.Auth{ID: "synthetic-account"}
		for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleCompaction, claudeprofile.RoleLightHelper} {
			a := BeginClaudeDesktopSDKAccounting(&tracker, auth, bundle, nil, role, "synthetic-session", uuid.NewString(), uuid.NewString(), time.Now(), time.Now())
			if a.helper != nil {
				t.Fatal("missing parent acquired an owner")
			}
			if (a.SDKSessionStateError() != nil) != unavailable {
				t.Fatal("missing helper owner swallowed restore failure or invented state loss", role, unavailable)
			}
			a.FinishFailure()
			if (a.SDKSessionStateError() != nil) != unavailable {
				t.Fatal("helper settlement cleared the missing state")
			}
		}
	}
}

func TestClaudeDesktopSDKAccountingKeepsPersistenceFailureAfterLeaseReleaseAndEviction(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	for _, finish := range []string{"success", "failure", "incomplete"} {
		t.Run(finish, func(t *testing.T) {
			store := &sdkAccountingFailingStore{SDKSessionStore: NewClaudeDesktopSDKSessionStore(t.TempDir(), "")}
			tracker := claudeprompt.NewTracker(store)
			t.Cleanup(func() { _ = tracker.Close() })
			auth := &cliproxyauth.Auth{ID: "synthetic-account"}
			scope, _ := json.Marshal([]string{auth.ID, bundle.ProfileID, auth.ProxyURL})
			at := time.Now()
			input := claudeprompt.Input{AccountID: string(scope), SessionID: "synthetic-session", Role: "main", ClientRequestID: uuid.NewString(), StartedAt: at, Body: sdkAccountingBody}
			main := tracker.Begin(input)
			main.ObserveSDKQuery(sdkAccountingBody)
			main.FinishSuccess(at.Add(time.Second), "end_turn", nil)
			main.RecordSDKAPISuccess(17)
			a := BeginClaudeDesktopSDKAccounting(&tracker, auth, bundle, nil, claudeprofile.RoleTitle, input.SessionID, main.Identity().PromptID, uuid.NewString(), at.Add(time.Second), at.Add(time.Second))
			if a.helper == nil || a.SDKSessionStateError() != nil {
				t.Fatal("healthy parent was not bound")
			}
			store.fail = true
			if finish != "success" {
				// Failure/early-return paths must retain an error already observed
				// by the helper, not only one generated by its success callback.
				a.helper.RecordSuccess("synthetic-prior-callback", "prior", 3)
			}
			if finish == "failure" {
				a.FinishFailure()
			} else {
				a.ObserveRequest(sdkAccountingBody)
				a.ObserveHTTPResponse(200)
				if finish == "success" {
					a.ObservePayload(sdkAccountingReply, false)
				}
				a.FinishSuccess(at.Add(2 * time.Second))
			}
			if !errors.Is(a.SDKSessionStateError(), claudeprompt.ErrSDKSessionUnavailable) {
				t.Fatal("completion hid the helper persistence failure")
			}
			store.fail = false
			other := input
			other.SessionID, other.ClientRequestID = "other-session", uuid.NewString()
			other.StartedAt = at.Add(2 * time.Hour)
			tracker.Begin(other)
			if tracker.SDKSessionStateError(input.AccountID, input.SessionID) != nil {
				t.Fatal("completed helper retained the otherwise idle scope")
			}
			a.FinishFailure()
			a.FinishSuccess(at.Add(3 * time.Second))
			if !errors.Is(a.SDKSessionStateError(), claudeprompt.ErrSDKSessionUnavailable) {
				t.Fatal("cache eviction or duplicate completion cleared the captured failure")
			}
		})
	}
}

func TestClaudeDesktopSDKAccountingMainFailureSurvivesEviction(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	store := &sdkAccountingFailingStore{SDKSessionStore: NewClaudeDesktopSDKSessionStore(t.TempDir(), "")}
	tracker := claudeprompt.NewTracker(store)
	t.Cleanup(func() { _ = tracker.Close() })
	auth := &cliproxyauth.Auth{ID: "synthetic-account"}
	scope, _ := json.Marshal([]string{auth.ID, bundle.ProfileID, auth.ProxyURL})
	at := time.Now()
	input := claudeprompt.Input{AccountID: string(scope), SessionID: "synthetic-session", Role: "main", ClientRequestID: uuid.NewString(), StartedAt: at, Body: sdkAccountingBody}
	main := tracker.Begin(input)
	main.ObserveSDKQuery(sdkAccountingBody)
	a := BeginClaudeDesktopSDKAccounting(&tracker, auth, bundle, main, claudeprofile.RoleMain, input.SessionID, "", input.ClientRequestID, at, at)
	a.ObserveRequest(sdkAccountingBody)
	a.ObserveHTTPResponse(200)
	a.ObservePayload(sdkAccountingReply, false)
	store.fail = true
	main.FinishSuccess(at.Add(time.Second), "end_turn", nil)
	a.FinishSuccess(at.Add(time.Second))
	if !errors.Is(a.SDKSessionStateError(), claudeprompt.ErrSDKSessionUnavailable) {
		t.Fatal("main success hid its persistence failure")
	}
	store.fail = false
	other := input
	other.SessionID, other.ClientRequestID, other.StartedAt = "other-session", uuid.NewString(), at.Add(2*time.Hour)
	tracker.Begin(other)
	if tracker.SDKSessionStateError(input.AccountID, input.SessionID) != nil {
		t.Fatal("completed main callback still retained its idle cache scope")
	}
	if !errors.Is(a.SDKSessionStateError(), claudeprompt.ErrSDKSessionUnavailable) {
		t.Fatal("main accounting became healthy solely because its cache scope was evicted")
	}
}
