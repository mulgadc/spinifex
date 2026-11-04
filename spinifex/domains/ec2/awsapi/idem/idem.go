// Package idem holds the client-token plumbing shared by the EC2
// gateway: the KV bucket every EC2 action's token records live in, and the
// extraction of a token and parameter hash from an SDK input struct.
package idem

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/idempotency"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucket is the JetStream KV bucket for EC2 ClientToken records. One
	// bucket for the whole EC2 surface; per-action stores namespace their keys.
	KVBucket = "spinifex-ec2-clienttokens"

	// TTL must outlast SDK retry windows; short enough that a crashed in-flight
	// record ages out and frees the token for a fresh attempt.
	TTL = 15 * time.Minute

	// tokenField is the ClientToken field on every EC2 input struct that
	// supports idempotency.
	tokenField = "ClientToken"
)

// TokenAndParams reads the ClientToken off an SDK input struct and hashes the
// request with the token cleared, so identical requests always hash alike. It
// reports false when the input carries no usable token, which means the caller
// asked for no idempotency and the request runs unwrapped.
func TokenAndParams(input any) (token, paramHash string, ok bool) {
	v := reflect.ValueOf(input)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", "", false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", "", false
	}
	field := v.FieldByName(tokenField)
	if !field.IsValid() || field.Kind() != reflect.Pointer || field.Type().Elem().Kind() != reflect.String {
		return "", "", false
	}
	if field.IsNil() || field.Elem().String() == "" {
		return "", "", false
	}

	// Hash a copy with the token cleared rather than the original: the caller's
	// input is dispatched afterwards and must reach the handler intact.
	clone := reflect.New(v.Type()).Elem()
	clone.Set(v)
	clone.FieldByName(tokenField).Set(reflect.Zero(field.Type()))
	return field.Elem().String(), idempotency.ParamHash(clone.Addr().Interface()), true
}

// Bucket returns the KV bucket that holds the token records. It is called only
// when a request carries a token, so tokenless requests never bind it.
type Bucket func(ctx context.Context) (jetstream.KeyValue, error)

// Run runs work under the input's ClientToken, storing its result under
// namespace. Requests without a token run unwrapped, which is what AWS does:
// the token is how a client opts in.
func Run[Out any](
	ctx context.Context,
	bucket Bucket,
	namespace, accountID string,
	input any,
	work func() (Out, error),
) (Out, error) {
	var zero Out
	// Hashed before work runs, so work that mutates its input cannot change what
	// a retry of the same token hashes to.
	token, paramHash, ok := TokenAndParams(input)
	if !ok {
		return work()
	}

	kv, kerr := bucket(ctx)
	if kerr != nil {
		// Running anyway would create the duplicate the token was sent to
		// prevent, so this fails rather than degrading to no idempotency.
		slog.ErrorContext(ctx, "client-token store unavailable", "namespace", namespace, "err", kerr)
		return zero, errors.New(awserrors.ErrorServerInternal)
	}

	store := idempotency.NewStore[Out](kv, namespace)
	output, err := idempotency.Do(ctx, store, accountID, token, paramHash, work)
	switch {
	case errors.Is(err, idempotency.ErrParamMismatch):
		return zero, errors.New(awserrors.ErrorIdempotentParameterMismatch)
	case errors.Is(err, idempotency.ErrUnavailable):
		slog.ErrorContext(ctx, "client-token replay unavailable", "namespace", namespace, "token", token, "err", err)
		return zero, errors.New(awserrors.ErrorServerInternal)
	}
	return output, err
}
