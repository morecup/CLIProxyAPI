package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type queryControlDoer struct {
	recordingControlDoer
	creates        atomic.Int32
	stream         bool
	started        chan *http.Request
	heartbeats     chan *http.Request
	archiveStatus  int
	archiveEntered chan struct{}
	archiveRelease chan struct{}
	blockRead      bool
}

func (d *queryControlDoer) Do(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	response, err := d.recordingControlDoer.Do(request)
	if err != nil {
		return nil, err
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
		response.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(`{"session":{"id":"cse_query-%d"}}`, d.creates.Add(1))))
	case d.stream && strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
		reader, writer := io.Pipe()
		response.Body = &queryPipeBody{PipeReader: reader, writer: writer}
		d.started <- request
	case strings.HasSuffix(request.URL.Path, "/worker/heartbeat"):
		if d.heartbeats != nil {
			d.heartbeats <- request
		}
	case d.blockRead && request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/worker"):
		d.started <- request
		<-request.Context().Done()
		return nil, request.Context().Err()
	case strings.HasSuffix(request.URL.Path, "/archive"):
		if d.archiveEntered != nil {
			close(d.archiveEntered)
			select {
			case <-d.archiveRelease:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		if d.archiveStatus != 0 {
			response.StatusCode = d.archiveStatus
		}
	}
	return response, nil
}

type queryPipeBody struct {
	*io.PipeReader
	writer *io.PipeWriter
	once   sync.Once
}

func (b *queryPipeBody) Close() error {
	b.once.Do(func() { _ = b.PipeReader.Close(); _ = b.writer.Close() })
	return nil
}

func newQueryControlManager(t *testing.T, doer *queryControlDoer) *Manager {
	t.Helper()
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	bundle.ControlPlane.HeartbeatIntervalSeconds = 1
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: !doer.stream,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil },
	})
	t.Cleanup(manager.Close)
	return manager
}

func queryFacts(id string, lifetime context.Context) RequestFacts {
	return RequestFacts{Role: claudeprofile.RoleMain, LocalSessionID: "shared-sdk-transcript", DesktopSessionID: "local_" + id,
		QueryID: id, QueryLifetime: lifetime, RequireQueryOwnership: true, Model: "claude-sonnet-5", Attempt: 1,
		Body: []byte(`{"messages":[{"role":"user","content":"synthetic input"}]}`)}
}

func awaitQueryControl[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("control-plane operation did not complete")
		var zero T
		return zero
	}
}

func TestQueryWorkersDoNotShareTranscriptOwnership(t *testing.T) {
	doer := &queryControlDoer{}
	m := newQueryControlManager(t, doer)
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	first, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("first", firstCtx))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("second", t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if first.session == second.session || first.session.state.RemoteSessionID == second.session.state.RemoteSessionID || first.session.statePath == second.session.statePath {
		t.Fatal("shared transcript merged independent workers")
	}
	if first.session.state.LocalSessionID != second.session.state.LocalSessionID {
		t.Fatal("wire transcript identity changed")
	}
	if err := m.RetireQuery(t.Context(), "local_second", "first"); err == nil {
		t.Fatal("foreign record can retire worker")
	}
	cancelFirst()
	if err := m.RetireQuery(t.Context(), "local_first", "first"); err != nil {
		t.Fatal(err)
	}
	before := len(doer.snapshot())
	first.ObserveHTTPResponse(http.Header{"Request-Id": {"late-response"}})
	first.FinishSuccess(context.Background())
	if err := m.RetireQuery(t.Context(), "local_first", "first"); err != nil {
		t.Fatal(err)
	}
	if len(doer.snapshot()) != before {
		t.Fatal("late completion or duplicate stop sent requests")
	}
	if second.session.ctx.Err() != nil || m.Status() != (Status{Active: 1}) {
		t.Fatal("sibling worker was stopped", m.Status())
	}
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("first", t.Context())); err == nil {
		t.Fatal("retired generation was resurrected")
	}
	next := queryFacts("replacement", t.Context())
	next.DesktopSessionID = "local_first"
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), next); err != nil {
		t.Fatal(err)
	}
	if doer.creates.Load() != 3 {
		t.Fatal("successor reused retired worker")
	}
}

func TestQueryCancellationReapsStreamAndHeartbeat(t *testing.T) {
	doer := &queryControlDoer{stream: true, started: make(chan *http.Request, 8), heartbeats: make(chan *http.Request, 8)}
	m := newQueryControlManager(t, doer)
	ctx, cancel := context.WithCancel(t.Context())
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("loops", ctx))
	if err != nil {
		t.Fatal(err)
	}
	stream := awaitQueryControl(t, doer.started)
	_ = awaitQueryControl(t, doer.heartbeats)
	cancel()
	done := make(chan error, 1)
	go func() { done <- m.RetireQuery(t.Context(), "local_loops", "loops") }()
	if err := awaitQueryControl(t, done); err != nil {
		t.Fatal(err)
	}
	if stream.Context().Err() == nil || !span.session.state.Archived {
		t.Fatal("stream or worker survived retirement")
	}
	count := len(doer.snapshot())
	if err := span.session.consumeStreamOnce(span.session.ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("retired stream may reconnect", err)
	}
	if err := span.session.ensure(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("retired worker may initialize", err)
	}
	if len(doer.snapshot()) != count {
		t.Fatal("retired loops emitted traffic")
	}
}

func TestQueryStopCancelsInFlightInitialization(t *testing.T) {
	doer := &queryControlDoer{blockRead: true, started: make(chan *http.Request, 1)}
	m := newQueryControlManager(t, doer)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("starting", ctx))
		done <- err
	}()
	_ = awaitQueryControl(t, doer.started)
	cancel()
	if err := awaitQueryControl(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.RetireQuery(t.Context(), "local_starting", "starting"); err != nil {
		t.Fatal(err)
	}
	if m.Status() != (Status{}) {
		t.Fatal("initializing worker survived cancellation", m.Status())
	}
}

func TestQueryRetirementFailureIsVisibleAndInert(t *testing.T) {
	doer := &queryControlDoer{archiveStatus: http.StatusBadGateway}
	m := newQueryControlManager(t, doer)
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("failure", t.Context())); err != nil {
		t.Fatal(err)
	}
	err := m.RetireQuery(t.Context(), "local_failure", "failure")
	if err == nil || m.Status() != (Status{Failed: 1}) {
		t.Fatal("archive failure was hidden", err, m.Status())
	}
	count := len(doer.snapshot())
	if err := m.RetireQuery(t.Context(), "local_failure", "failure"); err == nil {
		t.Fatal("failure was cleared by repeated stop")
	}
	m.Close()
	if len(doer.snapshot()) != count {
		t.Fatal("account close repeated query shutdown")
	}
}

func TestRetirementWaitDoesNotBlockOtherWorkerAdmission(t *testing.T) {
	doer := &queryControlDoer{archiveEntered: make(chan struct{}), archiveRelease: make(chan struct{})}
	m := newQueryControlManager(t, doer)
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("blocked", t.Context())); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.RetireQuery(waitCtx, "local_blocked", "blocked") }()
	awaitQueryControl(t, doer.archiveEntered)
	var release sync.Once
	unblock := func() { release.Do(func() { close(doer.archiveRelease) }) }
	t.Cleanup(unblock)
	cancelWait()
	if err := awaitQueryControl(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m.Status().Retiring != 1 {
		t.Fatal("canceled caller lost retirement status")
	}
	admitted := make(chan error, 1)
	go func() {
		_, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("sibling", t.Context()))
		admitted <- err
	}()
	if err := awaitQueryControl(t, admitted); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := m.RetireQuery(t.Context(), "local_blocked", "blocked"); err != nil {
		t.Fatal(err)
	}
	// The sibling will close at cleanup; do not reuse the one-shot archive gate.
	doer.archiveEntered = nil
}

func TestQueryTitleCannotCrossGenerations(t *testing.T) {
	doer := &queryControlDoer{}
	m := newQueryControlManager(t, doer)
	ctx, cancel := context.WithCancel(t.Context())
	facts := queryFacts("title", ctx)
	facts.Role = claudeprofile.RoleTitle
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts)
	if err != nil {
		t.Fatal(err)
	}
	span.ObserveResponsePayload([]byte(`{"content":[{"type":"text","text":"{\"title\":\"stale title\"}"}]}`), false)
	cancel()
	span.FinishSuccess(context.Background())
	if len(m.pendingTitles) != 0 {
		t.Fatal("late title survived its query")
	}
	facts.QueryLifetime = nil
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts); err == nil {
		t.Fatal("partial query ownership was accepted")
	}
}

func TestQueryCancellationDoesNotInventGracefulReason(t *testing.T) {
	doer := &queryControlDoer{}
	m := newQueryControlManager(t, doer)
	ctx, cancel := context.WithCancel(t.Context())
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("unclassified-close", ctx))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitQueryControl(t, span.session.done)
	for _, request := range doer.snapshot() {
		if strings.Contains(string(request.body), "worker_shutting_down") {
			t.Fatal("cancellation invented a shutdown reason")
		}
	}
}

func TestQuarantineReapsQueryWithoutShutdownTraffic(t *testing.T) {
	doer := &queryControlDoer{stream: true, started: make(chan *http.Request, 2)}
	m := newQueryControlManager(t, doer)
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("quarantined", t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	stream := awaitQueryControl(t, doer.started)
	count := len(doer.snapshot())
	done := make(chan struct{})
	go func() { m.Quarantine(); close(done) }()
	awaitQueryControl(t, done)
	if stream.Context().Err() == nil || span.session.state.Archived || len(doer.snapshot()) != count {
		t.Fatal("quarantine emitted normal shutdown or kept a stream alive")
	}
	m.Close()
	if len(doer.snapshot()) != count {
		t.Fatal("close after quarantine sent shutdown")
	}
}

func TestQuarantineAbortsAlreadyRunningGracefulClose(t *testing.T) {
	doer := &queryControlDoer{archiveEntered: make(chan struct{}), archiveRelease: make(chan struct{})}
	m := newQueryControlManager(t, doer)
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), queryFacts("quarantine-during-close", t.Context())); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	awaitQueryControl(t, doer.archiveEntered)
	quarantined := make(chan struct{})
	go func() { m.Quarantine(); close(quarantined) }()
	awaitQueryControl(t, quarantined)
	awaitQueryControl(t, closed)
}

func TestQueryInitializationFailureRemainsVisible(t *testing.T) {
	doer := &queryControlDoer{}
	m := newQueryControlManager(t, doer)
	auth := testDesktopAuth(t)
	auth.Metadata["access_token"] = ""
	if _, err := m.BeginRequest(t.Context(), auth, queryFacts("missing-auth", t.Context())); err == nil {
		t.Fatal("missing token was accepted")
	}
	if got := m.Status(); got.Active != 1 || got.InitializationFailed != 1 {
		t.Fatal("initialization failure hidden", got)
	}
	if len(doer.snapshot()) != 0 {
		t.Fatal("missing credential sent requests")
	}
}
