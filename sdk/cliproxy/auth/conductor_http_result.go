package auth

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ExecuteHTTPRequest accounts for a selected model request sent through the raw
// HTTP path. HttpRequest remains available for management and non-model calls.
func (m *Manager) ExecuteHTTPRequest(ctx context.Context, auth *Auth, req *http.Request, model string, opts cliproxyexecutor.Options) (*http.Response, error) {
	if m == nil || auth == nil || req == nil {
		return m.HttpRequest(ctx, auth, req)
	}
	ctx = newUpstreamAttemptContext(ctx)
	ephemeral := m.HomeEnabled()
	ctx, finish := cliproxyexecutor.WithHTTPResultObserver(ctx, func(err error) {
		result := Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Options: opts, Success: err == nil}
		if err != nil {
			result.Error = resultErrorFromError(err)
			result.RetryAfter = retryAfterFromError(err)
			result.CredentialScope = isCredentialScopedError(err)
			action, matched := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot())
			applyRequestScopedActionToResult(action, matched, &result)
		}
		m.recordExecutionResult(ctx, result, auth, ephemeral)
	})
	response, err := m.HttpRequest(ctx, auth, req.Clone(ctx))
	if err != nil {
		finish(err)
		return response, err
	}
	if response == nil || response.Body == nil {
		err = &Error{Code: "empty_response", Message: "upstream returned an empty response", Retryable: true}
		finish(err)
		return response, err
	}
	logging.SetResponseHeaders(ctx, response.Header)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		finish(&Error{HTTPStatus: response.StatusCode, Message: http.StatusText(response.StatusCode)})
	}
	response.Body = &httpResultBody{ReadCloser: response.Body, finish: finish, ctx: ctx}
	return response, nil
}

type httpResultBody struct {
	io.ReadCloser
	finish func(error)
	ctx    context.Context
}

func (b *httpResultBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.finish(nil)
	} else if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *httpResultBody) Close() error {
	err := b.ReadCloser.Close()
	cause := err
	if cause == nil {
		cause = b.ctx.Err()
	}
	if cause == nil {
		cause = io.ErrUnexpectedEOF
	}
	b.finish(cause)
	return err
}
