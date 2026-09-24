package helps

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestObserveResponseBodyDecoderExitDoesNotTruncateResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("raw response"), 10000)
	body := ObserveResponseBody(io.NopCloser(bytes.NewReader(payload)), func(source io.Reader) {
		_, _ = io.ReadFull(source, make([]byte, 1))
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("response changed after observer stopped: length=%d error=%v", len(got), err)
		}
		_ = body.Close()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("decoder exit blocked response forwarding")
	}
}

func TestObserveResponseBodyCloseUnblocksReaderAndObserver(t *testing.T) {
	source, writer := io.Pipe()
	defer writer.Close()
	body := ObserveResponseBody(source, func(reader io.Reader) { _, _ = io.Copy(io.Discard, reader) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = body.Read(make([]byte, 1))
	}()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = body.Close()
	}()
	for _, ch := range []<-chan struct{}{done, closed} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("Close did not unblock the raw reader and observer")
		}
	}
}
