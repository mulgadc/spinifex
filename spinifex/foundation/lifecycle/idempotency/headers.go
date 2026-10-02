package idempotency

import (
	"context"

	"github.com/nats-io/nats.go"
)

// KeyHeader carries a caller-stable retry token over a NATS hop. The AWS SDKs
// repeat one amz-sdk-invocation-id across every retry of a single call, which
// is the only thing that distinguishes a resent mutation from a new one.
const KeyHeader = "Spinifex-Idempotency-Key"

// SDKInvocationIDHeader is the HTTP header AWS SDKs stamp with an identifier
// that stays fixed across retries of one call. The attempt counter in
// amz-sdk-request changes instead.
const SDKInvocationIDHeader = "amz-sdk-invocation-id"

type keyContextKey struct{}

// WithKey attaches key to ctx. An empty key is dropped so callers need not
// branch before propagating an optional retry token.
func WithKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, keyContextKey{}, key)
}

// KeyFromContext returns the retry token, or "" when the caller sent none.
// An absent key means "treat this as a new request".
func KeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(keyContextKey{}).(string)
	return key
}

// KeyFromMsg reads the token off an inbound NATS request.
func KeyFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(KeyHeader)
}
