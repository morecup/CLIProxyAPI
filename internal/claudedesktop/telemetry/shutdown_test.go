package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func awaitTelemetryShutdown(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal(name + " did not join")
	}
}

func TestQuarantineCancelsGracefulOrExplicitFlushAndPreservesClaims(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "graceful"}[graceful], func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 7, 4, 0, 0, 0, time.UTC)}
			manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
				bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
			})
			started := make(chan *http.Request, 1)
			canceled, release, finished, quarantined := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			closing, quarantining := false, false
			t.Cleanup(func() {
				unblock()
				if closing {
					awaitTelemetryShutdown(t, finished, "flush cleanup")
				}
				if quarantining {
					awaitTelemetryShutdown(t, quarantined, "quarantine cleanup")
				}
			})
			manager.doerFactory = func(string) HTTPDoer {
				return HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
					started <- request
					select {
					case <-request.Context().Done():
						close(canceled)
					case <-release:
					}
					<-release
					if err := request.Context().Err(); err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: 204, Proto: "HTTP/2.0", ProtoMajor: 2, Body: io.NopCloser(strings.NewReader(""))}, nil
				})
			}
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			worker, err := manager.workerFor(auth)
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.enqueueProjected(t.Context(), FactUpdateCheckStarted, "", "", map[string]any{"current_version": "1.40609.0", "update_channel": "production", "is_manual": false}); err != nil {
				t.Fatal(err)
			}
			var flushErr error
			closing = true
			go func() {
				defer close(finished)
				if graceful {
					manager.Close()
				} else {
					flushErr = manager.Flush(context.Background())
				}
			}()
			var request *http.Request
			select {
			case request = <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("flush did not start")
			}
			if _, hasDeadline := request.Context().Deadline(); hasDeadline {
				t.Fatal("shutdown installed a production network deadline")
			}
			quarantining = true
			go func() { defer close(quarantined); manager.Quarantine() }()
			awaitTelemetryShutdown(t, canceled, "quarantine cancellation")
			if !manager.freezeOnShutdown.Load() {
				t.Fatal("quarantine did not freeze the queue")
			}
			unblock()
			awaitTelemetryShutdown(t, finished, "flush")
			awaitTelemetryShutdown(t, quarantined, "quarantine")
			if !graceful && !errors.Is(flushErr, context.Canceled) {
				t.Fatal("explicit flush did not observe quarantine", flushErr)
			}
			files, err := worker.scanQueue()
			if err != nil || len(files) != 1 || files[0].state != "pending" {
				t.Fatal("canceled delivery lost or stranded the durable obligation", files, err)
			}
			// Even with a new management context, quarantine cannot be bypassed.
			if err := manager.Flush(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal("explicit flush bypassed the frozen account", err)
			}
			select {
			case <-started:
				t.Fatal("quarantine started another delivery")
			default:
			}
		})
	}
}
