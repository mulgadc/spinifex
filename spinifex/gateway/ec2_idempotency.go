package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_idem "github.com/mulgadc/spinifex/spinifex/gateway/ec2/idem"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/nats-io/nats.go/jetstream"
)

// ec2ClientTokenBucket lazily binds the EC2 token bucket, once per gateway
// rather than once per request.
func (gw *GatewayConfig) ec2ClientTokenBucket(ctx context.Context) (jetstream.KeyValue, error) {
	gw.ec2TokenOnce.Do(func() {
		js, err := jetstream.New(gw.NATSConn)
		if err != nil {
			gw.ec2TokenErr = fmt.Errorf("clienttoken jetstream: %w", err)
			return
		}
		// The bind happens once, so it must not inherit the first caller's
		// cancellation: a client that disconnects mid-open would poison the
		// bucket for every later request. Deadline-free, so the open falls back
		// to the JetStream API's own timeout.
		gw.ec2TokenKV, gw.ec2TokenErr = idempotency.OpenBucket(
			context.WithoutCancel(ctx), js, gateway_ec2_idem.KVBucket, gateway_ec2_idem.TTL)
	})
	return gw.ec2TokenKV, gw.ec2TokenErr
}

// ec2IdempotentHandler is ec2Handler for creates that honour ClientToken: a
// repeat of the same token returns the first call's result instead of creating
// a second resource. Out is pinned so the result can be stored and replayed.
func ec2IdempotentHandler[In, Out any](handler func(ctx context.Context, input *In, gw *GatewayConfig, accountID string) (Out, error)) ec2Action {
	return ec2Action{
		idempotent: true,
		parse:      parseEC2Input[In],
		dispatch: func(action string, parsed any, gw *GatewayConfig, accountID string, r *http.Request) ([]byte, error) {
			input, ok := parsed.(*In)
			if !ok {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			ctx := requestContext(r)
			output, err := runEC2Idempotent(ctx, action, gw, accountID, input, func() (Out, error) {
				return handler(ctx, input, gw, accountID)
			})
			if err != nil {
				return nil, err
			}
			return marshalEC2Response(action, output)
		},
	}
}

// runEC2Idempotent runs work under the input's ClientToken. Requests without one
// run unwrapped, which is what AWS does: the token is how a client opts in.
func runEC2Idempotent[In, Out any](
	ctx context.Context,
	action string,
	gw *GatewayConfig,
	accountID string,
	input *In,
	work func() (Out, error),
) (Out, error) {
	var zero Out
	// Hashed before the handler runs, so a handler that mutates its input cannot
	// change what a retry of the same token hashes to.
	token, paramHash, ok := gateway_ec2_idem.TokenAndParams(input)
	if !ok {
		return work()
	}

	kv, kerr := gw.ec2ClientTokenBucket(ctx)
	if kerr != nil {
		// Running anyway would create the duplicate the token was sent to
		// prevent, so this fails rather than degrading to no idempotency.
		slog.ErrorContext(ctx, "client-token store unavailable", "action", action, "err", kerr)
		return zero, errors.New(awserrors.ErrorServerInternal)
	}

	// Namespaced by action: one bucket holds every EC2 action's records, and
	// without the prefix one action's payload could be decoded as another's Out.
	store := idempotency.NewStore[Out](kv, action)
	output, err := idempotency.Do(ctx, store, accountID, token, paramHash, work)
	switch {
	case errors.Is(err, idempotency.ErrParamMismatch):
		return zero, errors.New(awserrors.ErrorIdempotentParameterMismatch)
	case errors.Is(err, idempotency.ErrUnavailable):
		slog.ErrorContext(ctx, "client-token replay unavailable", "action", action, "token", token, "err", err)
		return zero, errors.New(awserrors.ErrorServerInternal)
	}
	return output, err
}
