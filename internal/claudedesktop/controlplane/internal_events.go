package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

var errWorkerReadUnavailable = errors.New("Claude Desktop worker read failed")

// WorkerEpochConflict is a terminal query-ownership conflict, not a request
// that may be repeated with a new worker's credentials. No server text or URL
// is retained in the error.
type WorkerEpochConflict struct{ Reason string }

func (e *WorkerEpochConflict) Error() string { return "CCRClient: Epoch mismatch (409)" }

// InternalEvent retains the original envelope and payload privately. Its
// identity belongs to the worker/session reader, never to inbound headers.
type InternalEvent struct {
	EventID        string          `json:"event_id"`
	SessionAgentID string          `json:"session_agent_id"`
	Payload        json.RawMessage `json:"payload"`
	raw            json.RawMessage
}

// EventIdentity preserves the native nullish fallback distinction, including
// an explicitly present empty ID. The envelope itself never escapes.
func (e InternalEvent) EventIdentity() *string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(e.raw, &fields) != nil {
		return nil
	}
	var id *string
	if json.Unmarshal(fields["event_id"], &id) != nil {
		return nil
	}
	return id
}

// EventIdentityPresent is separate from the nullish fallback used for a saved
// tip. Native anchor-return detection falls back to payload.uuid only when the
// envelope property is absent, not when event_id is explicitly null.
func (e InternalEvent) EventIdentityPresent() bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(e.raw, &fields) != nil {
		return false
	}
	_, exists := fields["event_id"]
	return exists
}

type InternalEventStats struct {
	PageCount       int
	BytesReceived   *float64
	ContentEncoding string
}

type InternalEventRead struct {
	events         []InternalEvent
	Stats          InternalEventStats
	AnchorFallback string
}

func (*InternalEventRead) MarshalJSON() ([]byte, error) { return nil, errWorkerReadUnavailable }

func (r *InternalEventRead) Events() []InternalEvent {
	if r == nil {
		return nil
	}
	result := make([]InternalEvent, len(r.events))
	for i, event := range r.events {
		result[i] = event
		result[i].Payload, result[i].raw = bytes.Clone(event.Payload), bytes.Clone(event.raw)
	}
	return result
}

// workerReadClient is an immutable credential, origin and transport snapshot.
// A complete pagination chain uses this snapshot, including all retry attempts.
// It does not reacquire opMu from inside the worker-restoration callback.
type workerReadClient struct {
	request *http.Request
	doer    HTTPDoer
	wait    func(context.Context, time.Duration) error
}

func (s *sessionRuntime) workerReaderLocked(ctx context.Context, endpoint string) (*workerReadClient, error) {
	request, doer, err := s.buildRequest(ctx, endpoint, s.state.RemoteSessionID, nil)
	if err != nil {
		return nil, err
	}
	return &workerReadClient{request: request, doer: doer, wait: s.manager.workerReadWait}, nil
}

func workerReadPause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type workerReadResponse struct {
	body    json.RawMessage
	headers http.Header
	status  int
}

// The pinned R0t.getWithRetry uses ten attempts (three for a single agent),
// exponential delay capped at 30 s plus [0,500) ms jitter, and permanent
// 400/413/422 responses. Its request deadlines are intentionally not ported.
func (c *workerReadClient) get(ctx context.Context, query string, attempts int, inspectAnchorError bool) (workerReadResponse, error) {
	var last workerReadResponse
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		request := c.request.Clone(ctx)
		request.URL.RawQuery = query
		response, err := c.doer.Do(request)
		last = workerReadResponse{}
		if response != nil {
			last.status, last.headers = response.StatusCode, response.Header.Clone()
			if response.Body != nil {
				decoded, errDecode := decodeBody(response.Body, response.Header.Values("Content-Encoding"))
				if errDecode == nil {
					body, errRead := io.ReadAll(decoded)
					errClose := decoded.Close()
					err = errors.Join(err, errRead, errClose)
					last.body = body
				} else {
					err = errors.Join(err, errDecode)
				}
			}
			if response.StatusCode == http.StatusConflict {
				return last, &WorkerEpochConflict{Reason: workerConflictReason(last.headers, last.body)}
			}
			if err == nil && response.StatusCode >= 200 && response.StatusCode < 300 && json.Valid(last.body) {
				return last, nil
			}
			if response.StatusCode == 400 || response.StatusCode == 413 || response.StatusCode == 422 || inspectAnchorError && workerAnchorNotFound(last) {
				return last, errWorkerReadUnavailable
			}
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if attempt+1 < attempts {
			delay := min(500*time.Millisecond*time.Duration(1<<attempt), 30*time.Second) + time.Duration(rand.Float64()*float64(500*time.Millisecond))
			wait := c.wait
			if wait == nil {
				wait = workerReadPause
			}
			if err := wait(ctx, delay); err != nil {
				return last, err
			}
		}
	}
	return last, errWorkerReadUnavailable
}

func workerConflictReason(headers http.Header, body []byte) string {
	recognized := func(value string) bool {
		return value == "superseded_by_worker" || value == "epoch_stale" || value == "session_not_active"
	}
	if value := headers.Get("x-ccr-conflict-reason"); recognized(value) {
		return value
	}
	var value struct {
		Reason *string `json:"reason"`
		Error  struct {
			Reason *string `json:"reason"`
			Type   string  `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &value) == nil {
		reason := value.Error.Reason
		if reason == nil {
			reason = value.Reason
		}
		if reason != nil && recognized(*reason) {
			return *reason
		}
		if recognized(value.Error.Type) {
			return value.Error.Type
		}
	}
	return "epoch_conflict"
}

func workerAnchorNotFound(response workerReadResponse) bool {
	var value struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return response.status == 404 && json.Unmarshal(response.body, &value) == nil && value.Error.Type == "after_event_id_not_found"
}

// readInternal follows insertion-ordered URLSearchParams. Once a cursor is
// present, after_event_id is removed. Failed later pages never publish a prefix.
func (c *workerReadClient) readInternal(ctx context.Context, after, agent string, subagents bool) (*InternalEventRead, error) {
	base, attempts := "limit=1000", 10
	if subagents {
		base = "subagents=true&limit=1000"
	} else if agent != "" {
		base, attempts = "session_agent_id="+workerQueryParameter(agent)+"&limit=1000", 3
	}
	query := base
	if after != "" {
		query += "&after_event_id=" + workerQueryParameter(after)
	}
	result := &InternalEventRead{}
	var cursor string
	bytesReceived := float64(0)
	result.Stats.BytesReceived = &bytesReceived
	encodingSeen := false
	for {
		if cursor != "" {
			query = base + "&cursor=" + workerQueryParameter(cursor)
		}
		page, err := c.get(ctx, query, attempts, true)
		if err != nil {
			if cursor == "" && after != "" && (page.status == 400 || workerAnchorNotFound(page)) {
				full, errFull := c.readInternal(ctx, "", agent, subagents)
				if errFull != nil {
					return nil, errFull
				}
				full.AnchorFallback = "rejected"
				if page.status == 404 {
					full.AnchorFallback = "not-found"
				}
				return full, nil
			}
			return nil, err
		}
		var value struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor string            `json:"next_cursor"`
		}
		if bytes.Equal(bytes.TrimSpace(page.body), []byte("null")) || json.Unmarshal(page.body, &value) != nil {
			return nil, errWorkerReadUnavailable
		}
		result.Stats.PageCount++
		if _, present := page.headers["Content-Length"]; present && result.Stats.BytesReceived != nil {
			length := strings.TrimSpace(page.headers.Get("Content-Length"))
			number, err := float64(0), error(nil)
			if length != "" {
				number, err = strconv.ParseFloat(length, 64)
			}
			if err != nil {
				number = math.NaN()
			}
			bytesReceived += number
		} else {
			result.Stats.BytesReceived = nil
		}
		if encoding, ok := page.headers["Content-Encoding"]; !encodingSeen && ok {
			encodingSeen = true
			if len(encoding) != 0 {
				result.Stats.ContentEncoding = encoding[0]
			}
		}
		for _, raw := range value.Data {
			var event InternalEvent
			if json.Unmarshal(raw, &event) != nil || !json.Valid(event.Payload) {
				return nil, errWorkerReadUnavailable
			}
			var kind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(event.Payload, &kind)
			if kind.Type == "auto-mode-classifier-dump" || kind.Type == "auto-mode-classifier-system" {
				continue
			}
			event.raw = bytes.Clone(raw)
			result.events = append(result.events, event)
		}
		cursor = value.NextCursor
		if cursor == "" {
			if !encodingSeen {
				result.Stats.ContentEncoding = "none"
			}
			return result, nil
		}
	}
}

func workerQueryParameter(value string) string {
	// WHATWG URLSearchParams uses the form percent-encode set, unlike Go's
	// QueryEscape for '*' and '~'. IDs and cursors remain opaque values.
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(value), "~", "%7E"), "%2A", "*")
}

// This reader is lent only during restoration, while its owner holds opMu.
// A retained callback cannot read using a successor's epoch or credentials.
func (r *WorkerRestoration) ReadInternalEvents(ctx context.Context, after string) (*InternalEventRead, error) {
	return r.readInternal(ctx, after, "", false)
}

func (r *WorkerRestoration) ReadSubagentInternalEvents(ctx context.Context) (*InternalEventRead, error) {
	return r.readInternal(ctx, "", "", true)
}

func (r *WorkerRestoration) readInternal(ctx context.Context, after, agent string, subagents bool) (*InternalEventRead, error) {
	if r == nil || r.reader == nil || r.readLifetime == nil || r.readLifetime.Err() != nil {
		return nil, errWorkerRestorationStale
	}
	ctx, cancel := context.WithCancel(ctx)
	unwatch := context.AfterFunc(r.readLifetime, cancel)
	defer func() { unwatch(); cancel() }()
	result, err := r.reader.readInternal(ctx, after, agent, subagents)
	var conflict *WorkerEpochConflict
	if errors.As(err, &conflict) {
		if r.abort != nil {
			r.abort()
		}
		return nil, err
	}
	if r.readLifetime.Err() != nil {
		return nil, errWorkerRestorationStale
	}
	return result, err
}

func (s *sessionRuntime) internalEventReaderLocked(ctx context.Context) (*workerReadClient, error) {
	return s.workerReaderLocked(ctx, claudeprofile.ControlEndpointWorkerInternalEvents)
}
