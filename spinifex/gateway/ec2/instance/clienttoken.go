package gateway_ec2_instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_idem "github.com/mulgadc/spinifex/spinifex/gateway/ec2/idem"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ClientTokenStore is the RunInstances idempotency store: the first caller owns
// the launch, duplicates replay its reservation or poll for it.
type ClientTokenStore = idempotency.Store[ec2.Reservation]

var (
	ctStore        *ClientTokenStore
	ctOnce         sync.Once
	errCTStoreInit error
)

// getClientTokenStore lazily initialises the process-wide client-token store via
// sync.Once. Process-wide because RunInstances is a free function with no
// instance to hang it on.
func getClientTokenStore(ctx context.Context, nc *nats.Conn) (*ClientTokenStore, error) {
	ctOnce.Do(func() {
		js, err := jetstream.New(nc)
		if err != nil {
			errCTStoreInit = fmt.Errorf("clienttoken jetstream: %w", err)
			return
		}
		// The bind happens once per process, so it must not inherit the first
		// caller's cancellation: a client that disconnects mid-open would poison
		// the store for every later launch. Deadline-free, so the open falls back
		// to the JetStream API's own timeout.
		ctStore, errCTStoreInit = newClientTokenStore(context.WithoutCancel(ctx), js)
	})
	return ctStore, errCTStoreInit
}

// newClientTokenStore takes the bucket unnamespaced. Every other EC2 action
// namespaces its keys by action, but RunInstances predates that: re-keying live
// records would make a retry re-launch instead of replay.
func newClientTokenStore(ctx context.Context, js jetstream.JetStream) (*ClientTokenStore, error) {
	return idempotency.OpenStore[ec2.Reservation](ctx, js, gateway_ec2_idem.KVBucket, gateway_ec2_idem.TTL)
}

// clientTokenParamHash hashes the request excluding ClientToken, so the same
// params always produce the same hash. Must run before any input mutation.
func clientTokenParamHash(input *ec2.RunInstancesInput) string {
	clone := *input
	clone.ClientToken = nil
	return idempotency.ParamHash(&clone)
}

// runInstancesWithClientToken wraps a launch in ClientToken idempotency and maps
// the token errors onto the AWS ones RunInstances returns. Extracted for
// unit-testability.
func runInstancesWithClientToken(
	ctx context.Context,
	store *ClientTokenStore,
	accountID, token, paramHash string,
	launch func() (ec2.Reservation, error),
) (ec2.Reservation, error) {
	res, err := idempotency.Do(ctx, store, accountID, token, paramHash, launch)
	switch {
	case errors.Is(err, idempotency.ErrParamMismatch):
		return ec2.Reservation{}, errors.New(awserrors.ErrorIdempotentParameterMismatch)
	case errors.Is(err, idempotency.ErrUnavailable):
		slog.ErrorContext(ctx, "RunInstances: client-token claim failed", "token", token, "err", err)
		return ec2.Reservation{}, errors.New(awserrors.ErrorServerInternal)
	}
	return res, err
}
