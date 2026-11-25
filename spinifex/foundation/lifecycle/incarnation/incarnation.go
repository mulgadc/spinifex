// Package incarnation carries the resource incarnation a policy check
// authorized across a NATS hop, so the handler acts on that incarnation and
// on no same-name replacement.
package incarnation

import (
	"context"

	"github.com/nats-io/nats.go"
)

// Header carries the stored ARN the policy check evaluated, or None.
const Header = "Spinifex-Authorized-Incarnation"

// None is the Header value when the policy check found no stored resource.
const None = "none"

type contextKey struct{}

// With records the stored ARN the policy check evaluated. An empty ARN means
// the check found no stored resource.
func With(ctx context.Context, storedARN string) context.Context {
	if storedARN == "" {
		storedARN = None
	}
	return context.WithValue(ctx, contextKey{}, storedARN)
}

// FromContext returns the recorded value (a stored ARN or None), and false
// when the request carries no authorized incarnation.
func FromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(contextKey{}).(string)
	return v, ok && v != ""
}

// FromMsg copies the Header of an inbound NATS request into ctx.
func FromMsg(ctx context.Context, msg *nats.Msg) context.Context {
	if msg == nil || msg.Header == nil {
		return ctx
	}
	v := msg.Header.Get(Header)
	if v == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, v)
}

// Permits reports whether a handler may act on the stored resource whose ARN
// is storedARN ("" when none is stored). A request with no authorized
// incarnation permits any.
func Permits(ctx context.Context, storedARN string) bool {
	want, ok := FromContext(ctx)
	if !ok {
		return true
	}
	if storedARN == "" {
		return want == None
	}
	return want == storedARN
}
