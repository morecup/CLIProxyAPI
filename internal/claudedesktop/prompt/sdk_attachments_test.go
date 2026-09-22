package prompt

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func attachmentTestOptions() SDKAttachmentNormalizeOptions {
	return SDKAttachmentNormalizeOptions{ReadTextDisplay: func(json.RawMessage) (SDKReadTextDisplay, error) {
		return SDKReadTextDisplay{}, nil
	}}
}

func attachmentRowsEqual(t *testing.T, got, want []json.RawMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count: got %d, want %d", len(got), len(want))
	}
	for index := range got {
		var actual, expected any
		if json.Unmarshal(got[index], &actual) != nil || json.Unmarshal(want[index], &expected) != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("normalized row %d differs: got %s, want %s", index, got[index], want[index])
		}
	}
}

func TestSDKAttachmentNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-attachments-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Name       string
			Attachment json.RawMessage
			Expected   []json.RawMessage
		}
		Wrapper []struct {
			Payload  json.RawMessage
			Expected json.RawMessage
			Effects  []string
		}
		TabAware struct {
			Attachment json.RawMessage
			Expected   []json.RawMessage
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil || len(fixture.Vectors) != 44 || len(fixture.Wrapper) != 21 {
		t.Fatal("invalid native attachment fixture", err)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			rows, errRows := NormalizeSDKAttachment(vector.Attachment, attachmentTestOptions())
			if errRows != nil {
				t.Fatal(errRows)
			}
			attachmentRowsEqual(t, rows, vector.Expected)
			if vector.Name == "empty_file" || vector.Name == "past_offset" {
				withoutSideTable, errEmpty := NormalizeSDKAttachment(vector.Attachment, SDKAttachmentNormalizeOptions{})
				if errEmpty != nil {
					t.Fatal("unused read-display state incorrectly required", errEmpty)
				}
				attachmentRowsEqual(t, withoutSideTable, vector.Expected)
			}
		})
	}
	for _, vector := range fixture.Wrapper {
		fields, _ := sdkAttachmentObject(vector.Expected)
		var effects []string
		wrapped, errWrap := WrapSDKAttachment(vector.Payload, func() time.Time {
			effects = append(effects, "now")
			return time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
		}, func() string {
			effects = append(effects, "uuid")
			return sdkRestorationString(fields, "uuid")
		})
		if errWrap != nil || !reflect.DeepEqual(effects, vector.Effects) {
			t.Fatal("native identity/clock evaluation order differs", effects, vector.Effects, errWrap)
		}
		attachmentRowsEqual(t, []json.RawMessage{wrapped}, []json.RawMessage{vector.Expected})
	}
	rows, err := NormalizeSDKAttachment(fixture.TabAware.Attachment, SDKAttachmentNormalizeOptions{ReadTextDisplay: func(json.RawMessage) (SDKReadTextDisplay, error) {
		return SDKReadTextDisplay{TabAwareSeparator: true, Prefix: "prefix\n"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	attachmentRowsEqual(t, rows, fixture.TabAware.Expected)
}

func TestSDKAttachmentUnknownIsNotAnEmptyOrFailedRead(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"Type":"already_read_file"}`, `{"type":"unknown"}`, `{"type":"plan_mode"}`,
		`{"type":"file","filename":"private"}`, `{"type":"invoked_skills","skills":null}`,
		`{"type":"plan_file_reference","planFilePath":"private"}`, `{"type":"task_status","status":"running"}`,
		`{"type":"hook_additional_context","hookName":"test","content":[null]}`,
		`{"type":"file","filename":"private","content":{"type":"text","file":{"content":"private","startLine":1,"numLines":1,"totalLines":1}}}`,
		`{"type":"file","filename":"private","content":{"type":"notebook","file":{"cells":[]}}}`,
	} {
		if rows, err := NormalizeSDKAttachment([]byte(raw), SDKAttachmentNormalizeOptions{}); err == nil || len(rows) != 0 {
			t.Fatalf("unknown content silently became a normalized result: %s", raw)
		}
	}
	read := []byte(`{"type":"file","filename":"private","content":{"type":"text","file":{"content":"private","startLine":1,"numLines":1,"totalLines":1}}}`)
	want := errors.New("synthetic unavailable read side table")
	rows, err := NormalizeSDKAttachment(read, SDKAttachmentNormalizeOptions{ReadTextDisplay: func(json.RawMessage) (SDKReadTextDisplay, error) {
		return SDKReadTextDisplay{}, want
	}})
	if !errors.Is(err, want) || len(rows) != 0 {
		t.Fatal("missing read facts became an oTe Error result")
	}
}
