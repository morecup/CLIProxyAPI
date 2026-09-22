package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func contextTestBody(rows ...string) []byte {
	return []byte(`{"model":"synthetic-model","system":"caller system","messages":[` + strings.Join(rows, ",") + `],"other":{"exact":9007199254740993}}`)
}

const (
	contextOldUser  = `{"role":"user","content":"synthetic private input"}`
	contextOldReply = `{"role":"assistant","content":"synthetic old reply"}`
	contextCurrent  = `{"role":"user","content":"synthetic current"}`
	contextSummary  = `{"role":"user","content":[{"type":"text","text":"synthetic private summary","cache_control":{"type":"ephemeral"}}]}`
)

func TestClaudeDesktopContextDurableContinuationAndChainedCompaction(t *testing.T) {
	root := t.TempDir()
	store := NewClaudeDesktopContextStore(root, "")
	auth := &cliproxyauth.Auth{ID: "synthetic-account", ProxyURL: "synthetic-egress"}
	source := contextTestBody(contextOldUser, contextOldReply, contextCurrent)
	continued := contextTestBody(contextSummary, contextCurrent)
	lease, untouched, err := store.Resume(t.Context(), auth, "profile", "session", source)
	if err != nil || !bytes.Equal(source, untouched) {
		t.Fatal("fresh context changed input", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatal("lookup created state before an actual adoption")
	}
	if err := lease.SaveAdopted(t.Context(), source, continued); err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveAdopted(t.Context(), source, continued); !errors.Is(err, ErrClaudeDesktopContextStale) {
		t.Fatal("the same revision lease committed more than once")
	}
	encoded, err := os.ReadFile(lease.path)
	if err != nil || bytes.Contains(encoded, []byte("synthetic")) || bytes.Contains(encoded, []byte("session")) {
		t.Fatal("owned content or identity escaped encryption", err)
	}
	serialized, _ := json.Marshal(lease)
	if string(serialized) != "{}" {
		t.Fatal("lease exposes request-local facts")
	}
	const reply = `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"number":9007199254740993}}]}`
	const next = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"synthetic read"},{"type":"image","source":{"type":"base64","data":"synthetic-image","media_type":"image/png"}}]}`
	followup := contextTestBody(contextOldUser, contextOldReply, contextCurrent, reply, next)
	want := contextTestBody(contextSummary, contextCurrent, reply, next)
	restarted := NewClaudeDesktopContextStore(root, "")
	second, got, err := restarted.Resume(t.Context(), auth, "profile", "session", followup)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("restart did not preserve exact appended content: error=%v", err)
	}
	if gjson.GetBytes(got, "other.exact").Raw != "9007199254740993" {
		t.Fatal("large integer or non-message fields changed")
	}
	const newerSummary = `{"role":"user","content":"synthetic second summary"}`
	latest := contextTestBody(newerSummary, next)
	if err := second.SaveAdopted(t.Context(), got, latest); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{followup, want, latest} {
		_, got, err := NewClaudeDesktopContextStore(root, "").Resume(t.Context(), auth, "profile", "session", input)
		if err != nil || !bytes.Equal(got, latest) {
			t.Fatal("original/intermediate/latest histories diverged", err)
		}
	}
}

func TestClaudeDesktopContextExactScopeAndHistory(t *testing.T) {
	for _, mode := range []string{"account", "profile", "session", "egress", "global-egress", "runtime-root", "changed-prefix", "reordered-prefix", "text-blocks", "prefix-only", "extra-row-field", "extra-text-field", "large-number", "duplicate-root", "duplicate-row", "duplicate-content", "trailing-json", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			store := NewClaudeDesktopContextStore(root, "global-egress")
			auth := &cliproxyauth.Auth{ID: "account"}
			source := contextTestBody(contextOldUser, contextOldReply, contextCurrent)
			continued := contextTestBody(contextSummary, contextCurrent)
			lease, _, err := store.Resume(t.Context(), auth, "profile", "session", source)
			if err != nil || lease.SaveAdopted(t.Context(), source, continued) != nil {
				t.Fatal("fixture could not persist", err)
			}
			profile, session := "profile", "session"
			ctx := t.Context()
			input := source
			wantError := false
			switch mode {
			case "account":
				auth.ID = "other"
			case "profile":
				profile = "other"
			case "session":
				session = "../../other"
			case "egress":
				auth.ProxyURL = "other"
			case "global-egress":
				store = NewClaudeDesktopContextStore(root, "other")
			case "runtime-root":
				store = NewClaudeDesktopContextStore(t.TempDir(), "global-egress")
			case "changed-prefix":
				input = bytes.Replace(source, []byte("private input"), []byte("changed input"), 1)
			case "reordered-prefix":
				input = contextTestBody(contextCurrent, contextOldReply, contextOldUser)
			case "prefix-only":
				input = contextTestBody(contextOldUser, contextOldReply)
			case "text-blocks":
				input = contextTestBody(`{"role":"user","content":[{"type":"text","text":"synthetic "},{"type":"text","text":"private input"}]}`, contextOldReply, contextCurrent)
			case "extra-row-field":
				input = contextTestBody(`{"role":"user","content":"synthetic private input","extra":true}`, contextOldReply, contextCurrent)
			case "extra-text-field":
				input = contextTestBody(`{"role":"user","content":[{"type":"text","text":"synthetic private input","extra":true}]}`, contextOldReply, contextCurrent)
			case "large-number":
				input = contextTestBody(`{"role":"user","content":"synthetic private input","extra":9007199254740993}`, contextOldReply, contextCurrent)
			case "duplicate-root":
				input = []byte(`{"messages":[],"messages":[` + contextOldUser + `]}`)
				wantError = true
			case "duplicate-row":
				input = contextTestBody(`{"role":"assistant","role":"user","content":"synthetic private input"}`, contextOldReply, contextCurrent)
				wantError = true
			case "duplicate-content":
				input = contextTestBody(`{"role":"user","content":[{"type":"text","text":"other","text":"synthetic private input"}]}`, contextOldReply, contextCurrent)
				wantError = true
			case "trailing-json":
				input = append(bytes.Clone(source), []byte(` {}`)...)
				wantError = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantError = true
			}
			_, got, err := store.Resume(ctx, auth, profile, session, input)
			if (err != nil) != wantError {
				t.Fatalf("error=%v expected error=%t", err, wantError)
			}
			want := input
			if mode == "text-blocks" {
				want = continued
			}
			if !bytes.Equal(got, want) {
				t.Fatal("unmatched/ambiguous history was rewritten or exact text did not match")
			}
		})
	}
}

func TestClaudeDesktopContextStaleAndInvalidState(t *testing.T) {
	for _, mode := range []string{"stale", "changed-source", "cancelled", "corrupt", "foreign-scope", "write-failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			store := NewClaudeDesktopContextStore(root, "")
			auth := &cliproxyauth.Auth{ID: "account"}
			source := contextTestBody(contextOldUser, contextOldReply, contextCurrent)
			continued := contextTestBody(contextSummary, contextCurrent)
			lease, _, err := store.Resume(t.Context(), auth, "profile", "session", source)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			var before []byte
			switch mode {
			case "stale":
				other, _, _ := NewClaudeDesktopContextStore(root, "").Resume(ctx, auth, "profile", "session", source)
				if err := other.SaveAdopted(ctx, source, continued); err != nil {
					t.Fatal(err)
				}
				before, _ = os.ReadFile(lease.path)
			case "changed-source":
				source = bytes.Replace(source, []byte("private input"), []byte("changed"), 1)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "corrupt":
				if os.MkdirAll(filepath.Dir(lease.path), 0700) != nil {
					t.Fatal("mkdir")
				}
				before = []byte("synthetic corruption")
				if os.WriteFile(lease.path, before, 0600) != nil {
					t.Fatal("write fixture")
				}
			case "foreign-scope":
				other, _, _ := store.Resume(ctx, &cliproxyauth.Auth{ID: "other"}, "profile", "session", source)
				if err := other.SaveAdopted(ctx, source, continued); err != nil {
					t.Fatal(err)
				}
				before, _ = os.ReadFile(other.path)
				if os.WriteFile(lease.path, before, 0600) != nil {
					t.Fatal("copy fixture")
				}
			case "write-failure":
				if os.WriteFile(filepath.Join(root, "owned-context"), []byte("synthetic blocking file"), 0600) != nil {
					t.Fatal("write fixture")
				}
			}
			if err := lease.SaveAdopted(ctx, source, continued); err == nil {
				t.Fatal("invalid commit succeeded")
			}
			if len(before) != 0 {
				after, _ := os.ReadFile(lease.path)
				if !bytes.Equal(before, after) {
					t.Fatal("unverified or newer state overwritten")
				}
			}
			if mode == "corrupt" || mode == "foreign-scope" {
				_, got, err := store.Resume(t.Context(), auth, "profile", "session", source)
				if !errors.Is(err, ErrClaudeDesktopContextInvalid) || !bytes.Equal(got, source) {
					t.Fatal("corruption hidden", err)
				}
			}
		})
	}
}

func TestClaudeDesktopContextConcurrentAdoption(t *testing.T) {
	root := t.TempDir()
	auth := &cliproxyauth.Auth{ID: "account"}
	source := contextTestBody(contextOldUser, contextOldReply, contextCurrent)
	continued := contextTestBody(contextSummary, contextCurrent)
	var leases []*ClaudeDesktopContextLease
	for range 8 {
		lease, _, err := NewClaudeDesktopContextStore(root, "").Resume(t.Context(), auth, "profile", "session", source)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	var wait sync.WaitGroup
	results := make(chan error, len(leases))
	for _, lease := range leases {
		wait.Go(func() { results <- lease.SaveAdopted(t.Context(), source, continued) })
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrClaudeDesktopContextStale) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers=%d", successes)
	}
}

func TestClaudeDesktopContextProtectedEnvelopeValidation(t *testing.T) {
	store := NewClaudeDesktopContextStore(t.TempDir(), "")
	lease, _, err := store.Resume(t.Context(), &cliproxyauth.Auth{ID: "account"}, "profile", "session", contextTestBody(contextCurrent))
	if err != nil {
		t.Fatal(err)
	}
	if os.MkdirAll(filepath.Dir(lease.path), 0700) != nil {
		t.Fatal("mkdir")
	}
	for _, record := range []desktopContextRecord{
		{Version: 2, Scope: lease.scope},
		{Version: 1, Scope: lease.scope, Revision: "invalid", Transitions: []desktopContextTransition{{Source: []string{"not-a-hash"}, Replacement: []json.RawMessage{json.RawMessage(contextCurrent)}}}},
	} {
		plain, _ := json.Marshal(record)
		protector, ciphertext, err := claudedesktop.ProtectRuntimePayload(lease.path, plain)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(desktopContextEnvelope{Version: 1, Protector: protector, Ciphertext: ciphertext})
		if os.WriteFile(lease.path, encoded, 0600) != nil {
			t.Fatal("write")
		}
		if _, err := readDesktopContext(lease.path, lease.scope); !errors.Is(err, ErrClaudeDesktopContextInvalid) {
			t.Fatal("invalid decrypted record accepted")
		}
	}
}

func TestClaudeDesktopContextLosslessMessageKeys(t *testing.T) {
	for _, item := range []struct {
		name, content string
		valid         bool
	}{
		{"utf8", `"synthetic 😀"`, true},
		{"surrogate-pair", `"synthetic \ud83d\ude00"`, true},
		{"literal-escape", `"synthetic \\ud800"`, true},
		{"high-surrogate", `"synthetic \ud800"`, false},
		{"low-surrogate", `"synthetic \udc00"`, false},
		{"unpaired-high", `"synthetic \ud800x"`, false},
		{"invalid-second", `"synthetic \ud800\u0000"`, false},
		{"invalid-utf8", "\"synthetic \xff\"", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			row := json.RawMessage(`{"role":"user","content":` + item.content + `}`)
			_, err := desktopContextKeys([]json.RawMessage{row})
			if (err == nil) != item.valid {
				t.Fatalf("valid=%t error=%v", item.valid, err)
			}
		})
	}
	keys, err := desktopContextKeys([]json.RawMessage{
		json.RawMessage(`{"role":"user","content":"synthetic 😀"}`),
		json.RawMessage(`{"role":"user","content":"synthetic \ud83d\ude00"}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"synthetic","number":9007199254740992}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"synthetic","number":9007199254740993}]}`),
	})
	if err != nil || keys[0] != keys[1] || keys[2] == keys[3] {
		t.Fatal("Unicode normalization or number fidelity is lossy", err)
	}
}
