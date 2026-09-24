package helps

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestClaudeDesktopCompactionRequestKind(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers http.Header
		want    string
		invalid bool
	}{
		{name: "unmarked explicit summary", want: "manual"},
		{name: "manual", headers: http.Header{"X-CC-Compaction-Request": {"manual"}, "X-Claude-Code-Compaction": {"manual"}}, want: "manual"},
		{name: "threshold", headers: http.Header{"x-cc-compaction-request": {"auto"}, "x-claude-code-compaction": {"auto"}}, want: "auto"},
		{name: "PTL", headers: http.Header{"X-CC-Compaction-Request": {"reactive"}}, want: "reactive"},
		{name: "legacy alias", headers: http.Header{"x-claude-code-compaction": {" AUTO "}}, want: "auto"},
		{name: "matching duplicates", headers: http.Header{"X-CC-Compaction-Request": {"auto", "auto"}, "x-cc-compaction-request": {"auto"}}, want: "auto"},
		{name: "conflict", headers: http.Header{"X-CC-Compaction-Request": {"manual"}, "X-Claude-Code-Compaction": {"auto"}}, invalid: true},
		{name: "duplicate conflict", headers: http.Header{"X-CC-Compaction-Request": {"manual", "reactive"}}, invalid: true},
		{name: "case alias conflict", headers: http.Header{"X-CC-Compaction-Request": {"manual"}, "x-cc-compaction-request": {"auto"}}, invalid: true},
		{name: "combined values", headers: http.Header{"X-CC-Compaction-Request": {"manual, auto"}}, invalid: true},
		{name: "unknown", headers: http.Header{"X-CC-Compaction-Request": {"automatic"}}, invalid: true},
		{name: "empty", headers: http.Header{"X-CC-Compaction-Request": {""}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := test.headers.Clone()
			got, err := ClaudeDesktopCompactionRequestKind(nil, test.headers)
			if got != test.want || (err != nil) != test.invalid {
				t.Fatalf("kind=%q err=%v, want=%q invalid=%v", got, err, test.want, test.invalid)
			}
			if !reflect.DeepEqual(before, test.headers) {
				t.Fatal("origin resolution mutated caller headers")
			}
			child := context.WithValue(t.Context(), claudeDesktopReactiveCompactionContextKey{}, true)
			if got, err := ClaudeDesktopCompactionRequestKind(child, test.headers); got != "reactive" || err != nil {
				t.Fatalf("owned PTL helper inherited client origin: kind=%q err=%v", got, err)
			}
		})
	}
}
