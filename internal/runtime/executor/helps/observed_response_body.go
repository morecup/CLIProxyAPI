package helps

import (
	"io"
	"sync"
)

// ObserveResponseBody feeds a separate decoder without changing the bytes read
// by the caller. EOF and Close wait for observation to finish; the pipe bounds
// buffering and closing either side unblocks the other.
func ObserveResponseBody(body io.ReadCloser, observe func(io.Reader)) io.ReadCloser {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reader.Close()
		observe(reader)
	}()
	return &observedResponseBody{ReadCloser: body, writer: writer, done: done}
}

type observedResponseBody struct {
	io.ReadCloser
	writer *io.PipeWriter
	done   <-chan struct{}
	once   sync.Once
}

func (b *observedResponseBody) finish(err error) {
	b.once.Do(func() { _ = b.writer.CloseWithError(err) })
	<-b.done
}

func (b *observedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// Observation failures must not alter the upstream wire response.
		_, _ = b.writer.Write(p[:n])
	}
	if err == io.EOF {
		b.finish(nil)
	} else if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *observedResponseBody) Close() error {
	err := b.ReadCloser.Close()
	cause := err
	if cause == nil {
		cause = io.ErrUnexpectedEOF
	}
	b.finish(cause)
	return err
}
