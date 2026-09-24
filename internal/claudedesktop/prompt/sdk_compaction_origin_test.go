package prompt

import "testing"

func TestSDKCompactionOriginSeparatesNativeTriggerAndWireKind(t *testing.T) {
	for _, kind := range []string{"manual", "auto", "reactive", "unknown"} {
		for _, source := range []string{"", "model-default", "settings", "private-value"} {
			origin := SDKCompactionOrigin{Kind: kind, ThresholdSource: source}
			want := (kind == "manual" || kind == "reactive") && source == "" || kind == "auto" && (source == "model-default" || source == "settings")
			if origin.Valid() != want {
				t.Fatalf("origin validation differs: %+v", origin)
			}
		}
	}
}

func TestSDKCompactionTranscriptCountRequiresExactPreservedSelection(t *testing.T) {
	owner, body := compactionViewFixture(t, true)
	view, err := owner.CompactionView(body)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Discard()
	history := view.History()
	preserve := history.Groups[len(history.Groups)-1]
	count, known := view.PreservedTranscriptUUIDCount(preserve)
	if !known || count != len(preserve) {
		t.Fatal("simple preserved projection not known", count, known)
	}
	count, known = view.PreservedTranscriptUUIDCount(nil)
	if !known || count != 0 {
		t.Fatal("empty summarize_all tail not known")
	}
	foreign := append([]SDKHistoryMessage(nil), preserve...)
	foreign[0].Type = "attachment"
	if _, known := view.PreservedTranscriptUUIDCount(foreign); known {
		t.Fatal("unowned native attachment projection guessed")
	}
}
