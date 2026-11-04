package awsapi

import (
	"context"
	"errors"
	"fmt"

	ec2idemapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/idem"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/idempotency"
	"github.com/nats-io/nats.go/jetstream"
)

// clientTokenBucket lazily binds the EC2 token bucket, once per handler
// rather than once per request.
func (h *Handler) clientTokenBucket(ctx context.Context) (jetstream.KeyValue, error) {
	h.tokenOnce.Do(func() {
		js, err := jetstream.New(h.NATS)
		if err != nil {
			h.tokenErr = fmt.Errorf("clienttoken jetstream: %w", err)
			return
		}
		// The bind happens once, so it must not inherit the first caller's
		// cancellation: a client that disconnects mid-open would poison the
		// bucket for every later request. Deadline-free, so the open falls back
		// to the JetStream API's own timeout.
		h.tokenKV, h.tokenErr = idempotency.OpenBucket(
			context.WithoutCancel(ctx), js, ec2idemapi.KVBucket, ec2idemapi.TTL)
	})
	return h.tokenKV, h.tokenErr
}

// ec2IdempotentHandler is ec2Handler for creates that honour ClientToken: a
// repeat of the same token returns the first call's result instead of creating
// a second resource. Out is pinned so the result can be stored and replayed.
func ec2IdempotentHandler[In, Out any](handler func(ctx context.Context, input *In, c *call, accountID string) (Out, error)) ec2Action {
	return ec2Action{
		idempotent: true,
		parse:      parseEC2Input[In],
		dispatch: func(ctx context.Context, action string, parsed any, c *call, accountID string) ([]byte, error) {
			input, ok := parsed.(*In)
			if !ok {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			// Namespaced by action: one bucket holds every EC2 action's records, and
			// without the prefix one action's payload could be decoded as another's Out.
			output, err := ec2idemapi.Run(ctx, c.clientTokenBucket, action, accountID, input, func() (Out, error) {
				return handler(ctx, input, c, accountID)
			})
			if err != nil {
				return nil, err
			}
			return marshalEC2Response(action, output)
		},
	}
}
