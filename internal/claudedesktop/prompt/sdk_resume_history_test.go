package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

type sdkResumeHistoryTestFixture struct {
	SDKHash string `json:"sdk_sha256"`
	Cases   []struct {
		Name    string            `json:"name"`
		Input   []json.RawMessage `json:"input"`
		Options struct {
			Pending                []string `json:"pending"`
			FirstPartyAPI          *bool    `json:"firstPartyAPI"`
			ResumeInterruptedTurn  bool     `json:"resumeInterruptedTurn"`
			TolerateContextAppends bool     `json:"tolerateContextAppends"`
			ReplyOnResume          bool     `json:"replyOnResume"`
			RewindUUID             string   `json:"rewindUUID"`
			MaxAgeMilliseconds     string   `json:"maxAgeMilliseconds"`
			ResumePrompt           string   `json:"resumePrompt"`
			TerminalMCPTools       string   `json:"terminalMCPTools"`
			Cwd                    string   `json:"cwd"`
			Now                    string   `json:"now"`
		} `json:"options"`
		Expected json.RawMessage `json:"expected"`
	} `json:"cases"`
	Hooks []struct {
		Name     string            `json:"name"`
		Input    []json.RawMessage `json:"input"`
		Hooks    []json.RawMessage `json:"hooks"`
		Expected json.RawMessage   `json:"expected"`
	} `json:"hooks"`
}

func readSDKResumeHistoryFixture(t *testing.T) sdkResumeHistoryTestFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/sdk-resume-history.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture sdkResumeHistoryTestFixture
	if json.Unmarshal(raw, &fixture) != nil || fixture.SDKHash != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(fixture.Cases) != 337 || len(fixture.Hooks) != 11 {
		t.Fatal("native history fixture is missing or unreviewed")
	}
	return fixture
}

func TestSDKResumeHistoryMatchesNativeDeserializer(t *testing.T) {
	fixture := readSDKResumeHistoryFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			options := SDKResumeHistoryOptions{PendingToolUseIDs: tc.Options.Pending, FirstPartyAPI: tc.Options.FirstPartyAPI == nil || *tc.Options.FirstPartyAPI,
				ResumeInterruptedTurn: tc.Options.ResumeInterruptedTurn, TolerateContextAppends: tc.Options.TolerateContextAppends, ReplyOnResume: tc.Options.ReplyOnResume,
				RewindUUID: tc.Options.RewindUUID, MaxAgeMilliseconds: tc.Options.MaxAgeMilliseconds, ResumePrompt: tc.Options.ResumePrompt, TerminalMCPTools: tc.Options.TerminalMCPTools,
				Cwd: tc.Options.Cwd}
			at := time.Date(2026, 9, 7, 6, 0, 0, 0, time.UTC)
			if tc.Options.Now != "" {
				var err error
				at, err = time.Parse(time.RFC3339Nano, tc.Options.Now)
				if err != nil {
					t.Fatal(err)
				}
			}
			serial := 0
			options.Now = func() time.Time { return at }
			options.NewUUID = func() string { serial++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", serial) }
			// The platform path resolver is an explicit owned dependency. These
			// synthetic path mappings match native Windows path.relative, without
			// using the test host's cwd or platform as a Desktop environment.
			options.RelativePath = func(cwd, name string) (string, error) {
				if cwd != `C:\synthetic` {
					return "", errors.New("unexpected cwd")
				}
				switch name {
				case `C:\synthetic\folder\file.txt`:
					return `folder\file.txt`, nil
				case `D:\outside.txt`:
					return name, nil
				}
				return "", errors.New("unexpected path")
			}
			before, _ := json.Marshal(tc.Input)
			result, err := NormalizeSDKResumeHistory(tc.Input, options)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(tc.Input)
			if !bytes.Equal(before, after) {
				t.Fatal("native restoration mutated caller rows")
			}
			ids, names := result.SupersededToolUses()
			if ids == nil {
				ids = []string{}
			}
			pairs := make([][]string, 0, len(names))
			for _, id := range ids {
				if name, ok := names[id]; ok {
					pairs = append(pairs, []string{id, name})
				}
			}
			kind, message := result.Interruption()
			interruption := map[string]any{"kind": kind}
			if len(message) > 0 {
				interruption["message"] = message
			}
			events, diagnostics := result.events, result.diagnostics
			if events == nil {
				events = []sdkResumeEvent{}
			}
			if diagnostics == nil {
				diagnostics = []sdkResumeDiagnostic{}
			}
			got, err := json.Marshal(map[string]any{"messages": result.Messages(), "superseded": ids, "names": pairs, "interruption": interruption,
				"rescueSuppressed": result.RescueSuppressed(), "events": events, "diagnostics": diagnostics})
			if err != nil {
				t.Fatal(err)
			}
			if !equalNativeJSON(got, tc.Expected) {
				t.Fatalf("native deserializer differs: got %s; expected %s", got, tc.Expected)
			}
			encoded, err := json.Marshal(result)
			if err != nil || string(encoded) != "{}" {
				t.Fatal("private history was exported")
			}
		})
	}
}

func TestSDKResumeHistoryMatchesNativeHookDeduplication(t *testing.T) {
	for _, tc := range readSDKResumeHistoryFixture(t).Hooks {
		t.Run(tc.Name, func(t *testing.T) {
			before, _ := json.Marshal([]any{tc.Input, tc.Hooks})
			result, err := DeduplicateSDKResumeHooks(tc.Input, tc.Hooks)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(result)
			if !equalNativeJSON(got, tc.Expected) {
				t.Fatalf("native hook deduplication differs: got %s; expected %s", got, tc.Expected)
			}
			after, _ := json.Marshal([]any{tc.Input, tc.Hooks})
			if !bytes.Equal(before, after) {
				t.Fatal("hook deduplication mutated inputs")
			}
		})
	}
}

func TestSDKResumeHistoryRejectsMissingOwnedDependencyAndDetachesContent(t *testing.T) {
	fixture := readSDKResumeHistoryFixture(t)
	for _, tc := range fixture.Cases {
		if tc.Name == "attachment_11" {
			if _, err := NormalizeSDKResumeHistory(tc.Input, SDKResumeHistoryOptions{}); !errors.Is(err, ErrSDKSessionUnavailable) {
				t.Fatal("guessed an owned path", err)
			}
		}
	}
	input := []json.RawMessage{json.RawMessage(`{"type":"user","uuid":"human","message":{"role":"user","content":"PRIVATE_CONTENT"}}`)}
	result, err := NormalizeSDKResumeHistory(input, SDKResumeHistoryOptions{ReplyOnResume: false})
	if err != nil {
		t.Fatal(err)
	}
	before := result.Messages()
	rows := result.Messages()
	rows[0][0] = 'X'
	_, message := result.Interruption()
	message[0] = 'X'
	if !reflect.DeepEqual(before, result.Messages()) {
		t.Fatal("caller modified restored history")
	}
}
