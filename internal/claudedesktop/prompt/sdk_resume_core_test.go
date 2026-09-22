package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func sdkResumeTestRows(t *testing.T, raw json.RawMessage) []*sdkResumeRow {
	t.Helper()
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		t.Fatal("invalid resume test array")
	}
	rows := make([]*sdkResumeRow, 0, len(values))
	for _, value := range values {
		row, err := parseSDKResumeRow(value)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func sdkResumeTestEncoded(t *testing.T, rows []*sdkResumeRow) json.RawMessage {
	t.Helper()
	values := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		raw, err := row.encode()
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, raw)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSDKResumeCoreMatchesNativeSourceTransforms(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-resume-core.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SDKHash string `json:"sdk_sha256"`
		Cases   []struct {
			Operation string          `json:"operation"`
			Input     json.RawMessage `json:"input"`
			Options   struct {
				Pending                           []string `json:"pending"`
				DropSiblingBlocks                 bool     `json:"dropSiblingBlocks"`
				ShutdownUnwindResultsDoNotResolve bool     `json:"shutdownUnwindResultsDoNotResolve"`
				AllowTrailing                     bool     `json:"allowTrailing"`
			} `json:"options"`
			Expected json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.SDKHash != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(fixture.Cases) != 25 {
		t.Fatal("native resume fixture is missing or unreviewed")
	}
	for i, tc := range fixture.Cases {
		t.Run(fmt.Sprintf("%02d-%s", i, tc.Operation), func(t *testing.T) {
			input, expected := tc.Input, tc.Expected
			if tc.Operation == "RNo" {
				input = append(append([]byte{'['}, input...), ']')
				if bytes.Equal(expected, []byte("null")) {
					expected = input
				} else {
					expected = append(append([]byte{'['}, expected...), ']')
				}
			}
			rows := sdkResumeTestRows(t, input)
			var got json.RawMessage
			switch tc.Operation {
			case "Dyt":
				got = sdkResumeTestEncoded(t, sdkResumeDropRetracted(rows))
			case "RNo":
				got = sdkResumeTestEncoded(t, sdkResumeDropInvalidText(rows))
			case "jG":
				var ids []string
				var names map[string]string
				rows, ids, names = sdkResumeReconcileTools(rows, SDKResumeCoreOptions{PendingToolUseIDs: tc.Options.Pending,
					DropSiblingBlocks: tc.Options.DropSiblingBlocks, ShutdownUnwindResultsDoNotResolve: tc.Options.ShutdownUnwindResultsDoNotResolve})
				if ids == nil {
					ids = []string{}
				}
				pairs := make([][]string, 0, len(names))
				for _, id := range ids {
					if name, ok := names[id]; ok {
						pairs = append(pairs, []string{id, name})
					}
				}
				got, err = json.Marshal(map[string]any{"messages": sdkResumeTestEncoded(t, rows), "superseded": ids, "names": pairs})
				if err != nil {
					t.Fatal(err)
				}
			case "QU":
				got = sdkResumeTestEncoded(t, sdkResumeFilterThinking(rows, tc.Options.AllowTrailing))
			case "JU":
				got = sdkResumeTestEncoded(t, sdkResumeFilterWhitespace(rows))
			default:
				t.Fatal("unknown native resume transform")
			}
			if !equalNativeJSON(got, expected) {
				t.Fatalf("native transform differs: got %s; expected %s", got, expected)
			}
		})
	}
}

func TestSDKResumeCoreMatchesNativePipeline(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-resume-core.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SDKHash string `json:"sdk_sha256"`
		Cases   []struct {
			Name    string            `json:"name"`
			Input   []json.RawMessage `json:"input"`
			Options struct {
				Pending                           []string `json:"pending"`
				DropSiblingBlocks                 bool     `json:"dropSiblingBlocks"`
				ShutdownUnwindResultsDoNotResolve bool     `json:"shutdownUnwindResultsDoNotResolve"`
				AllowTrailing                     bool     `json:"allowTrailing"`
				TolerateContextAppends            bool     `json:"tolerateContextAppends"`
			} `json:"options"`
			Expected json.RawMessage `json:"expected"`
		} `json:"core_cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.SDKHash != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" || len(fixture.Cases) != 486 {
		t.Fatal("native resume pipeline fixture is missing or unreviewed")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			result, err := NormalizeSDKResumeCore(tc.Input, SDKResumeCoreOptions{
				PendingToolUseIDs: tc.Options.Pending, DropSiblingBlocks: tc.Options.DropSiblingBlocks,
				ShutdownUnwindResultsDoNotResolve: tc.Options.ShutdownUnwindResultsDoNotResolve,
				AllowTrailingThinking:             tc.Options.AllowTrailing, TolerateContextAppends: tc.Options.TolerateContextAppends,
			})
			if err != nil {
				t.Fatal(err)
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
			got, err := json.Marshal(map[string]any{"messages": result.Messages(), "superseded": ids, "names": pairs})
			if err != nil {
				t.Fatal(err)
			}
			if !equalNativeJSON(got, tc.Expected) {
				t.Fatalf("native pipeline differs: got %s; expected %s", got, tc.Expected)
			}
		})
	}
}

func TestSDKResumeCorePreservesRawContentAndCopies(t *testing.T) {
	input := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"left","isMeta":true,"collapseSources":["older"],"message":{"role":"user","content":"\ud800\u0026"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"removed","message":{"id":"empty","role":"assistant","content":[{"type":"text","text":" "}]}}`),
		json.RawMessage(`{"type":"user","uuid":"right","message":{"role":"user","content":"next"},"ignoredNumber":9007199254740993}`),
	}
	wantInput := append([]json.RawMessage(nil), input...)
	result, err := NormalizeSDKResumeCore(input, SDKResumeCoreOptions{})
	if err != nil || len(result.Messages()) != 1 || !reflect.DeepEqual(input, wantInput) {
		t.Fatal("resume core mutated input or failed native merging", err)
	}
	rows := result.Messages()
	if !bytes.Contains(rows[0], []byte(`\ud800\u0026\n`)) || !bytes.Contains(rows[0], []byte(`"collapseSources":["older","right"]`)) || !bytes.Contains(rows[0], []byte(`"uuid":"right"`)) {
		t.Fatal("raw string, provenance or meta UUID was lost")
	}
	rows[0][0] = 'X'
	if !json.Valid(result.Messages()[0]) {
		t.Fatal("caller changed private restored rows")
	}
	serialized, err := json.Marshal(result)
	if err != nil || string(serialized) != "{}" {
		t.Fatal("private native resume content was exported")
	}
}

func TestSDKResumeCoreUsesNativeStageOrder(t *testing.T) {
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u","message":{"role":"user","content":"input"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"thinking","message":{"id":"reply","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signed"}]}}`),
		json.RawMessage(`{"type":"assistant","uuid":"tool","message":{"id":"reply","role":"assistant","content":[{"type":"tool_use","id":"pending","name":"SyntheticTool","input":{}}]}}`),
		json.RawMessage(`{"type":"assistant","uuid":"artifact","message":{"id":"reply","role":"assistant","content":[{"type":"text","text":null}]}}`),
	}
	result, err := NormalizeSDKResumeCore(rows, SDKResumeCoreOptions{DropSiblingBlocks: true})
	if err != nil || len(result.Messages()) != 1 {
		t.Fatal("text artifact or removed tool incorrectly kept orphan thinking", err)
	}
	ids, names := result.SupersededToolUses()
	if !reflect.DeepEqual(ids, []string{"pending"}) || names["pending"] != "SyntheticTool" {
		t.Fatal("superseded tool ownership lost")
	}
	names["pending"] = "changed"
	ids[0] = "changed"
	nextIDs, nextNames := result.SupersededToolUses()
	if nextIDs[0] != "pending" || nextNames["pending"] != "SyntheticTool" {
		t.Fatal("caller changed superseded ownership")
	}
}

func TestSDKResumeCoreDecodesOwnedContextSource(t *testing.T) {
	row, err := parseSDKResumeRow(json.RawMessage(`{"type":"user","uuid":"context","promptSource":"s\u0064k","message":{"role":"user","content":"<system-reminder>context</system-reminder>"}}`))
	if err != nil || !sdkResumeContextAppend(row, true) {
		t.Fatal("context source compared encoded spelling instead of value", err)
	}
	if sdkResumeContextAppend(row, false) {
		t.Fatal("context feature flag was ignored")
	}
}
