package router

import (
	"context"
	"time"
)

func kUnserved(up, ep, op string) string { return "rt:un:" + up + "|" + ep + "|" + op }

func kUnservedSeen(up, ep, op string) string { return "rt:uns:" + up + "|" + ep + "|" + op }

// SeenUnserved counts a route-missing answer from an endpoint for op and
// returns how many were seen within window (the window starts at the
// first one), so callers can require repeated evidence before MarkUnserved.
func (r *Router) SeenUnserved(ctx context.Context, upstreamID, endpointID, op string, window time.Duration) (int64, error) {
	return r.st.Incr(context.WithoutCancel(ctx), kUnservedSeen(upstreamID, endpointID, op), 1, window)
}

// MarkUnserved remembers for ttl that an endpoint has no route for an
// operation (it answered 404/405 for the path itself). It reports whether
// the mark is new, so the caller logs it once.
func (r *Router) MarkUnserved(ctx context.Context, upstreamID, endpointID, op string, ttl time.Duration) (bool, error) {
	return r.st.SetNX(context.WithoutCancel(ctx), kUnserved(upstreamID, endpointID, op), []byte("1"), ttl)
}

// Unserved reports whether an endpoint is marked as not serving op. A state
// read failure reports false.
func (r *Router) Unserved(ctx context.Context, upstreamID, endpointID, op string) bool {
	_, ok, err := r.st.Get(ctx, kUnserved(upstreamID, endpointID, op))
	return err == nil && ok
}

func kRefreshed(up, cred, oldFP string) string { return "rt:rf:" + up + "|" + cred + "|" + oldFP }

// MarkRefreshed records cluster-wide that the credential whose secret had
// fingerprint oldFP was refreshed, so a node still holding the old secret
// reloads it from the store instead of spending the refresh token again.
func (r *Router) MarkRefreshed(ctx context.Context, upstreamID, credID, oldFP string, ttl time.Duration) error {
	return r.st.Set(context.WithoutCancel(ctx), kRefreshed(upstreamID, credID, oldFP), []byte("1"), ttl)
}

// Refreshed reports whether MarkRefreshed was recorded for oldFP. A state
// read failure reports false.
func (r *Router) Refreshed(ctx context.Context, upstreamID, credID, oldFP string) bool {
	_, ok, err := r.st.Get(ctx, kRefreshed(upstreamID, credID, oldFP))
	return err == nil && ok
}
