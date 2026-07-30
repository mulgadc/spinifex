package utils

import (
	"context"

	"github.com/nats-io/nats.go"
)

// IdempotencyKeyHeader carries a caller-stable retry token over the gateway →
// NATS hop. The AWS SDKs repeat one amz-sdk-invocation-id across every retry of
// a single call, which is the only thing that distinguishes a resent mutation
// from a genuinely new one — EC2's AllocateAddress takes no client token.
const IdempotencyKeyHeader = "Spinifex-Idempotency-Key"

// SDKInvocationIDHeader is the HTTP header the AWS SDKs stamp with an id that
// stays fixed across retries of one call (it is the attempt counter in
// amz-sdk-request that changes).
const SDKInvocationIDHeader = "amz-sdk-invocation-id"

type idempotencyKeyCtxKey struct{}

// WithIdempotencyKey attaches key to ctx. An empty key is dropped so callers
// need not branch.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyCtxKey{}, key)
}

// IdempotencyKeyFromContext returns the retry token, or "" when the caller sent
// none. An absent key means "treat this as a new request".
func IdempotencyKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(idempotencyKeyCtxKey{}).(string)
	return key
}

// IdempotencyKeyFromMsg reads the token off an inbound NATS request.
func IdempotencyKeyFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(IdempotencyKeyHeader)
}
