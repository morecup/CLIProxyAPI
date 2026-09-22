package tasks

import "context"

type callerKey struct{}
type invocationKey struct{}

func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

func CallerFromContext(ctx context.Context) Caller {
	if ctx == nil {
		return Caller{}
	}
	value, _ := ctx.Value(callerKey{}).(Caller)
	return value
}

// IsOwnedRequest reports whether ctx carries a request issued by this runtime
// (a main turn or a child generation) rather than by a downstream client.
func IsOwnedRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(callerKey{}).(Caller)
	return ok
}

func WithInvocation(ctx context.Context, invocation Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, invocation)
}

func InvocationFromContext(ctx context.Context) (Invocation, bool) {
	if ctx == nil {
		return Invocation{}, false
	}
	value, ok := ctx.Value(invocationKey{}).(Invocation)
	return value, ok && value.AgentID != ""
}
