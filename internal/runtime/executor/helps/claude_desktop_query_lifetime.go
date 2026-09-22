package helps

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// BindClaudeDesktopQueryLifetime keeps request cancellation independent from
// query retirement. Completing one request never retires its query or siblings.
// The caller retains all values and its existing deadline; no deadline is added.
func BindClaudeDesktopQueryLifetime(ctx, query context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if query == nil {
		return ctx, func() {}
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(query, cancel)
	if query.Err() != nil {
		cancel()
	}
	var once sync.Once
	return bound, func() {
		once.Do(func() {
			stop()
			cancel()
		})
	}
}

// RetainClaudeDesktopResponseLifetime transfers cleanup to the raw caller's
// response body. Header delivery is not completion, including for error bodies.
// Wrap outside response observation so final telemetry and helpers run first.
func RetainClaudeDesktopResponseLifetime(response *http.Response, release func()) *http.Response {
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		release()
		return response
	}
	response.Body = &claudeDesktopQueryResponseBody{ReadCloser: response.Body, release: release}
	return response
}

type claudeDesktopQueryResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *claudeDesktopQueryResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *claudeDesktopQueryResponseBody) Close() error {
	defer b.once.Do(b.release)
	return b.ReadCloser.Close()
}
