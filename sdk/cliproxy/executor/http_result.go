package executor

import (
	"context"
	"sync"
)

type httpResultObserverKey struct{}

// WithHTTPResultObserver lets a raw HTTP executor report protocol completion
// before the caller's transport-only EOF fallback. A request is reported once.
func WithHTTPResultObserver(ctx context.Context, observe func(error)) (context.Context, func(error)) {
	if ctx == nil {
		ctx = context.Background()
	}
	var once sync.Once
	finish := func(err error) { once.Do(func() { observe(err) }) }
	return context.WithValue(ctx, httpResultObserverKey{}, finish), finish
}

// ReportHTTPResult records a raw response's semantic outcome when an observer
// is installed. It does not change the response bytes or transport errors.
func ReportHTTPResult(ctx context.Context, err error) {
	if ctx == nil {
		return
	}
	if finish, ok := ctx.Value(httpResultObserverKey{}).(func(error)); ok {
		finish(err)
	}
}
