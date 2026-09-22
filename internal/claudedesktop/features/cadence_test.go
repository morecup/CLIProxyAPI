package features

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFeatureNativeRefreshCadence(t *testing.T) {
	for _, raw := range []string{"", `null`} {
		d, flagged := RefreshCadence(json.RawMessage(raw), 0)
		if d != 6*time.Hour || flagged {
			t.Fatalf("%s => %v %v", raw, d, flagged)
		}
	}
	for _, raw := range []string{`0`, `-1`, `false`, `true`, `{}`, `[]`, `""`, `"1_0"`, `"Infinity"`, `"0x"`, `"\u00855"`} {
		d, flagged := RefreshCadence(json.RawMessage(raw), 0)
		if d != 19440000*time.Millisecond || !flagged {
			t.Fatalf("%s => %v %v", raw, d, flagged)
		}
	}
	for _, raw := range []string{`5`, `"5"`, `"0x5"`, `"0b101"`, `"0o5"`, `"\ufeff5\u00a0"`, `1`, `0.5`} {
		for _, r := range []float64{0, 0.5} {
			d, flagged := RefreshCadence(json.RawMessage(raw), r)
			want := 270000 * time.Millisecond
			if r == 0.5 {
				want = 5 * time.Minute
			}
			if d != want || !flagged {
				t.Fatalf("%s / %v => %v %v", raw, r, d, flagged)
			}
		}
	}
	if d, _ := RefreshCadence(json.RawMessage(`360`), 1); d != 6*time.Hour {
		t.Fatal(d)
	}
}
