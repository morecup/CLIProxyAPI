package helps

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRemoteWorkerRestorationDrivesNextInput(t *testing.T) {
	for _, tc := range []struct{ name, metadata, want string }{
		{"model", `{"model":"restored"}`, "restored"},
		{"default", `{"model":" DeFaUlT "}`, "owned-default"},
		{"unknown", `{"model":"unknown"}`, "saved"},
		{"empty", `{"model":""}`, "saved"},
		{"numeric", `{"model":42}`, "saved"},
		{"absent", `{}`, "saved"},
		{"null", `null`, "saved"},
		{"not-object", `false`, "saved"},
		{"unowned-system", `{"model":"restored","system_prompt":"unowned","headers":{"X-Test":"unowned"}}`, "restored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, outcomes := make(chan []byte, 1), make(chan error, 1)
			var saves int
			a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "saved", DefaultModel: "owned-default", System: "owned-system",
				Persist: func(_ context.Context, model, system string) error {
					saves++
					if model != tc.want || system != "owned-system" {
						t.Error("unowned configuration persisted")
					}
					return nil
				}}, func(_ context.Context, model string, body []byte) ([]byte, error) {
				if model != tc.want {
					t.Error("executor saw stale model")
				}
				requests <- body
				return []byte(`{"role":"assistant","content":"reply"}`), nil
			}, nil, func(model string) (string, error) {
				if model != "restored" && model != "owned-default" {
					return "", errors.New("not a recognized model")
				}
				return model, nil
			}, func(err error) { outcomes <- err })
			t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
			if err := a.Enqueue(t.Context(), json.RawMessage(`{"type":"user","uuid":"queued","message":{"role":"user","content":"input"}}`)); err != nil {
				t.Fatal(err)
			}
			if err := a.RestoreWorker(t.Context(), json.RawMessage(tc.metadata), 7); err != nil {
				t.Fatal(err)
			}
			if err := a.RestoreWorker(t.Context(), json.RawMessage(`{"model":"unknown"}`), 7); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 0 {
				t.Fatal("restoration submitted inference")
			}
			if err := a.Start(); err != nil {
				t.Fatal(err)
			}
			body := remoteInputAwait(t, requests)
			if gjson.GetBytes(body, "model").String() != tc.want || gjson.GetBytes(body, "system").String() != "owned-system" || gjson.GetBytes(body, "headers").Exists() {
				t.Fatal("restored wire configuration differs")
			}
			if err := remoteInputAwait(t, outcomes); err != nil {
				t.Fatal(err)
			}
			wantSaves := 0
			if tc.want != "saved" {
				wantSaves = 1
			}
			if saves != wantSaves {
				t.Fatal("restoration was not committed exactly once", saves)
			}
			if err := a.RestoreWorker(t.Context(), json.RawMessage(`{"model":"restored"}`), 8); err == nil {
				t.Fatal("running actor adopted another worker")
			}
		})
	}
}

func TestRemoteWorkerRestorationFailureRetainsPausedActor(t *testing.T) {
	failed := true
	a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "saved", System: "owned",
		Persist: func(context.Context, string, string) error {
			if failed {
				return errors.New("synthetic persistence failure")
			}
			return nil
		}}, nil, nil,
		func(model string) (string, error) { return model, nil }, nil)
	t.Cleanup(func() { a.Stop(); remoteInputAwait(t, a.Done()) })
	if err := a.RestoreWorker(t.Context(), json.RawMessage(`{"model":"restored"}`), 2); err == nil {
		t.Fatal("failed persistence accepted")
	}
	if a.model != "saved" || a.workerRestored || a.started {
		t.Fatal("failed restoration published state")
	}
	failed = false
	if err := a.RestoreWorker(t.Context(), json.RawMessage(`{"model":"restored"}`), 2); err != nil {
		t.Fatal(err)
	}
	if a.model != "restored" || !a.workerRestored {
		t.Fatal("retry did not apply state")
	}
	if err := a.RestoreWorker(t.Context(), nil, 3); err == nil {
		t.Fatal("paused actor changed epoch")
	}
}

func TestRemoteWorkerRestorationCannotAdoptWithoutResolverOrPersistence(t *testing.T) {
	for _, resolver := range []bool{false, true} {
		var resolve func(string) (string, error)
		if resolver {
			resolve = func(model string) (string, error) { return model, nil }
		}
		a := NewClaudeDesktopRemoteInput(t.Context(), ClaudeDesktopRemoteInputConfiguration{Model: "saved"}, nil, nil, resolve, nil)
		err := a.RestoreWorker(t.Context(), json.RawMessage(`{"model":"unowned"}`), 0)
		if resolver != (err != nil) || a.model != "saved" {
			t.Fatal("unowned restoration admitted", err)
		}
		a.Stop()
		remoteInputAwait(t, a.Done())
	}
}
