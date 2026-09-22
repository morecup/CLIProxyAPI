package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func internalEventTestReader(t *testing.T, fn func(*http.Request, int) (*http.Response, error)) (*workerReadClient, *[]time.Duration, *int) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "GET", "https://synthetic.invalid/v1/code/sessions/cse_owned/worker/internal-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-epoch-one")
	delays, attempts := &[]time.Duration{}, new(int)
	c := &workerReadClient{request: request,
		doer: credentialBudgetDoerFunc(func(request *http.Request) (*http.Response, error) {
			*attempts++
			if request.Header.Get("Authorization") != "Bearer synthetic-epoch-one" {
				t.Fatal("credential changed within pagination")
			}
			return fn(request, *attempts)
		}), wait: func(ctx context.Context, delay time.Duration) error {
			*delays = append(*delays, delay)
			return ctx.Err()
		}}
	return c, delays, attempts
}

func internalEventTestResponse(status int, body string, headers ...http.Header) *http.Response {
	header := make(http.Header)
	if len(headers) == 1 {
		header = headers[0]
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestInternalEventsPaginationSnapshotAndFilter(t *testing.T) {
	var queries []string
	c, delays, calls := internalEventTestReader(t, func(request *http.Request, call int) (*http.Response, error) {
		queries = append(queries, request.URL.RawQuery)
		if call == 1 {
			request.Header.Set("Authorization", "do-not-reuse-mutated-request")
			return internalEventTestResponse(200, `{"data":[{"event_id":"a","payload":{"uuid":"a","type":"user","message":{"content":"PRIVATE"}}},{"payload":{"type":"auto-mode-classifier-dump"}},{"payload":{"type":"auto-mode-classifier-system"}}],"next_cursor":"page /two"}`,
				http.Header{"Content-Length": {"120"}}), nil
		}
		return internalEventTestResponse(200, `{"data":[{"event_id":"b","payload":{"type":"custom","uuid":"b"}}]}`,
			http.Header{"Content-Length": {"30"}}), nil
	})
	result, err := c.readInternal(t.Context(), "tip /one", "", false)
	if err != nil || *calls != 2 || len(*delays) != 0 || !reflect.DeepEqual(queries, []string{"limit=1000&after_event_id=tip+%2Fone", "limit=1000&cursor=page+%2Ftwo"}) {
		t.Fatal("pagination changed", err, queries, *calls)
	}
	if len(result.Events()) != 2 || result.Stats.PageCount != 2 || result.Stats.BytesReceived == nil || *result.Stats.BytesReceived != 150 || result.Stats.ContentEncoding != "none" {
		t.Fatal("pagination totals or filtering changed")
	}
	copy := result.Events()
	copy[0].Payload[0] = 'x'
	if !json.Valid(result.Events()[0].Payload) {
		t.Fatal("returned payload aliases retained data")
	}
	if _, err := json.Marshal(result); err == nil {
		t.Fatal("private history was serializable")
	}
}

func TestInternalEventsIdentityPresenceDiffersFromNullishFallback(t *testing.T) {
	reader, _, _ := internalEventTestReader(t, func(*http.Request, int) (*http.Response, error) {
		return internalEventTestResponse(200, `{"data":[{"payload":{"uuid":"absent"}},{"event_id":null,"payload":{"uuid":"null"}},{"event_id":"","payload":{"uuid":"empty"}},{"event_id":"tip","payload":{"uuid":"payload-tip"}}]}`), nil
	})
	read, err := reader.readInternal(t.Context(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	events := read.Events()
	for index, event := range events {
		id := event.EventIdentity()
		if event.EventIdentityPresent() != (index != 0) || (id != nil) != (index >= 2) {
			t.Fatal("identity presence collapsed null into absence", index)
		}
		if index == 2 && *id != "" || index == 3 && *id != "tip" {
			t.Fatal("explicit identity was changed", index)
		}
	}
}

func TestInternalEventsNativeRetryDisposition(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 413, 422, 429, 500, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, delays, calls := internalEventTestReader(t, func(*http.Request, int) (*http.Response, error) {
				return internalEventTestResponse(status, `{"error":{"reason":"epoch_stale","message":"PRIVATE"}}`), nil
			})
			result, err := c.readInternal(t.Context(), "", "", false)
			want := 10
			if status == 400 || status == 409 || status == 413 || status == 422 {
				want = 1
			}
			if result != nil || err == nil || strings.Contains(err.Error(), "PRIVATE") || *calls != want || len(*delays) != want-1 {
				t.Fatal("retry disposition changed", *calls, len(*delays), err)
			}
			for i, delay := range *delays {
				minimum := min(500*time.Millisecond*time.Duration(1<<i), 30*time.Second)
				if delay < minimum || delay >= minimum+500*time.Millisecond {
					t.Fatal("native backoff changed", i, delay)
				}
			}
			var conflict *WorkerEpochConflict
			if status == 409 && (!errors.As(err, &conflict) || conflict.Reason != "epoch_stale") {
				t.Fatal("terminal ownership conflict lost")
			}
		})
	}
}

func TestInternalEventsNetworkAndJSONRetry(t *testing.T) {
	c, delays, calls := internalEventTestReader(t, func(_ *http.Request, call int) (*http.Response, error) {
		if call == 1 {
			return nil, errors.New("PRIVATE network URL")
		}
		if call == 2 {
			return internalEventTestResponse(200, "invalid-json"), nil
		}
		return internalEventTestResponse(200, `{"data":[]}`), nil
	})
	result, err := c.readInternal(t.Context(), "", "", false)
	if err != nil || *calls != 3 || len(*delays) != 2 || result.Stats.PageCount != 1 || result.Stats.BytesReceived != nil {
		t.Fatal("network or JSON retry did not reach actual success", err)
	}
}

func TestInternalEventsFormEncodingAndNullPage(t *testing.T) {
	c, _, calls := internalEventTestReader(t, func(request *http.Request, _ int) (*http.Response, error) {
		if request.URL.RawQuery != "limit=1000&after_event_id=%7E*%2B+%2F" {
			t.Fatal("opaque anchor did not use URLSearchParams encoding", request.URL.RawQuery)
		}
		return internalEventTestResponse(200, " null "), nil
	})
	if result, err := c.readInternal(t.Context(), "~*+ /", "", false); result != nil || err == nil || *calls != 1 {
		t.Fatal("null JSON became an empty successful history", err)
	}
}

func TestInternalEventsAnchorFallbackAndNoPartialResult(t *testing.T) {
	for _, kind := range []string{"rejected", "not-found", "late-failure", "full-failure"} {
		t.Run(kind, func(t *testing.T) {
			var queries []string
			c, _, calls := internalEventTestReader(t, func(request *http.Request, call int) (*http.Response, error) {
				queries = append(queries, request.URL.RawQuery)
				if call == 1 {
					if kind == "late-failure" {
						return internalEventTestResponse(200, `{"data":[{"payload":{"uuid":"discard-prefix"}}],"next_cursor":"next"}`), nil
					}
					if kind == "not-found" {
						return internalEventTestResponse(404, `{"error":{"type":"after_event_id_not_found"}}`), nil
					}
					return internalEventTestResponse(400, `{}`), nil
				}
				if kind == "late-failure" || kind == "full-failure" {
					return internalEventTestResponse(413, `{}`), nil
				}
				return internalEventTestResponse(200, `{"data":[{"payload":{"uuid":"full"}}]}`), nil
			})
			result, err := c.readInternal(t.Context(), "anchor", "", false)
			if *calls != 2 {
				t.Fatal("unexpected extra fallback", *calls)
			}
			if strings.HasSuffix(kind, "failure") {
				if result != nil || err == nil {
					t.Fatal("partial read became authoritative")
				}
				return
			}
			if err != nil || result.AnchorFallback != kind || result.Stats.PageCount != 1 || queries[1] != "limit=1000" {
				t.Fatal("anchor fallback changed", err, queries)
			}
		})
	}
}

func TestInternalEventsLaneAndCancellation(t *testing.T) {
	for _, lane := range []string{"subagents", "agent"} {
		t.Run(lane, func(t *testing.T) {
			c, _, calls := internalEventTestReader(t, func(request *http.Request, _ int) (*http.Response, error) {
				want := "subagents=true&limit=1000"
				if lane == "agent" {
					want = "session_agent_id=agent+%2Fone&limit=1000"
				}
				if request.URL.RawQuery != want {
					t.Fatal("agent lane changed")
				}
				return internalEventTestResponse(500, `{}`), nil
			})
			agent, want := "", 10
			if lane == "agent" {
				agent, want = "agent /one", 3
			}
			_, _ = c.readInternal(t.Context(), "", agent, lane == "subagents")
			if *calls != want {
				t.Fatal("wrong lane retry budget", *calls)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := c.readInternal(ctx, "", agent, false); !errors.Is(err, context.Canceled) || *calls != want {
				t.Fatal("cancelled reader still sent")
			}
		})
	}
}

func TestWorkerInternalEventCapabilityExpiresAndConflictsCancelOwner(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconvBool(conflict), func(t *testing.T) {
			c, _, _ := internalEventTestReader(t, func(*http.Request, int) (*http.Response, error) {
				if conflict {
					return internalEventTestResponse(409, `{}`, http.Header{"X-Ccr-Conflict-Reason": {"superseded_by_worker"}}), nil
				}
				return internalEventTestResponse(200, `{"data":[]}`), nil
			})
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			owner, abort := context.WithCancel(t.Context())
			defer abort()
			state := &WorkerRestoration{reader: c, readLifetime: lifetime, abort: abort}
			_, err := state.ReadInternalEvents(t.Context(), "")
			if (err != nil) != conflict || (owner.Err() != nil) != conflict {
				t.Fatal("worker conflict did not retire exact owner", err)
			}
			cancel()
			if _, err := state.ReadInternalEvents(t.Context(), ""); !errors.Is(err, errWorkerRestorationStale) {
				t.Fatal("retained capability remained usable", err)
			}
		})
	}
}

func strconvBool(value bool) string {
	if value {
		return "conflict"
	}
	return "normal"
}
