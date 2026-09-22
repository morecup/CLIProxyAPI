package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeDesktopQueryLifetimeIsolation(t *testing.T) {
	query, closeQuery := context.WithCancel(t.Context())
	defer closeQuery()
	caller, cancelCaller := context.WithCancel(t.Context())
	a, releaseA := BindClaudeDesktopQueryLifetime(caller, query)
	defer releaseA()
	b, releaseB := BindClaudeDesktopQueryLifetime(t.Context(), query)
	defer releaseB()
	other, releaseOther := BindClaudeDesktopQueryLifetime(t.Context(), t.Context())
	defer releaseOther()
	cancelCaller()
	if a.Err() != context.Canceled || b.Err() != nil || query.Err() != nil {
		t.Fatal("request cancellation crossed the query boundary")
	}
	releaseA()
	if b.Err() != nil || query.Err() != nil {
		t.Fatal("request cleanup retired the query")
	}
	closeQuery()
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("query retirement did not cancel its request")
	}
	if other.Err() != nil {
		t.Fatal("query retirement cancelled a different query")
	}
	closed, releaseClosed := BindClaudeDesktopQueryLifetime(t.Context(), query)
	defer releaseClosed()
	if closed.Err() != context.Canceled {
		t.Fatal("already retired query allowed an uncancelled request")
	}
}

func TestClaudeDesktopQueryLifetimePreservesCallerContext(t *testing.T) {
	type key struct{}
	deadline := time.Now().Add(time.Hour)
	caller, cancel := context.WithDeadline(context.WithValue(t.Context(), key{}, "owned"), deadline)
	defer cancel()
	ctx, release := BindClaudeDesktopQueryLifetime(caller, t.Context())
	defer release()
	actual, ok := ctx.Deadline()
	if !ok || !actual.Equal(deadline) || ctx.Value(key{}) != "owned" {
		t.Fatal("query binding changed caller values or deadline")
	}
	without, releaseWithout := BindClaudeDesktopQueryLifetime(context.Background(), t.Context())
	defer releaseWithout()
	if _, ok := without.Deadline(); ok {
		t.Fatal("query binding invented a network deadline")
	}
}

func TestClaudeDesktopRawResponseRetainsQueryLifetime(t *testing.T) {
	for _, mode := range []string{"eof", "close", "empty", "nil", "error-status"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("response"))}
			if mode == "nil" {
				response = nil
			} else if mode == "empty" {
				response.Body = http.NoBody
			} else if mode == "error-status" {
				response.StatusCode = 502
			}
			response = RetainClaudeDesktopResponseLifetime(response, func() { calls++ })
			if mode == "nil" || mode == "empty" {
				if calls != 1 {
					t.Fatal("bodyless response retained a finished request")
				}
				return
			}
			if calls != 0 {
				t.Fatal("headers released a live response")
			}
			if mode != "close" {
				if _, err := io.ReadAll(response.Body); err != nil {
					t.Fatal(err)
				}
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("response did not release exactly once", calls)
			}
		})
	}
}
