package gateway_ec2_instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// kvBucketClientTokens is the JetStream KV bucket for ClientToken records.
	kvBucketClientTokens = "spinifex-ec2-clienttokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clientTokenTTL must outlast SDK retry windows; short enough that a crashed
	// in-flight record ages out and frees the token for a fresh launch.
	clientTokenTTL = 15 * time.Minute
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

func newClientTokenStore(ctx context.Context, js jetstream.JetStream) (*ClientTokenStore, error) {
	return idempotency.OpenStore[ec2.Reservation](ctx, js, kvBucketClientTokens, clientTokenTTL)
}

// clientTokenParamHash hashes the request excluding ClientToken, so the same
// params always produce the same hash. Must run before any input mutation.
func clientTokenParamHash(input *ec2.RunInstancesInput) string {
	clone := *input
	clone.ClientToken = nil
	return idempotency.ParamHash(&clone)
}

// runInstancesWithClientToken wraps a launch in ClientToken idempotency:
// claims the token, replays a completed reservation, or (as owner) launches,
// finalizes, and aborts on failure. Extracted for unit-testability.
func runInstancesWithClientToken(
	ctx context.Context,
	store *ClientTokenStore,
	accountID, token, paramHash string,
	launch func() (ec2.Reservation, error),
) (ec2.Reservation, error) {
	var zero ec2.Reservation
	replay, owned, cerr := store.Claim(ctx, accountID, token, paramHash)
	if cerr != nil {
		if errors.Is(cerr, idempotency.ErrParamMismatch) {
			return zero, errors.New(awserrors.ErrorIdempotentParameterMismatch)
		}
		slog.Error("RunInstances: client-token claim failed", "token", token, "err", cerr)
		return zero, errors.New(awserrors.ErrorServerInternal)
	}
	if replay != nil {
		return *replay, nil
	}
	if !owned {
		return zero, errors.New(awserrors.ErrorServerInternal)
	}

	res, rerr := launch()

	// Recording the launch outcome outlives ctx: a caller that went away mid-launch
	// is exactly when the record must be settled, and leaving it in-flight parks
	// every retry of that token behind the poll deadline until the record ages out.
	outcomeCtx := context.WithoutCancel(ctx)
	if rerr != nil {
		store.Abort(outcomeCtx, accountID, token)
		return zero, rerr
	}
	if ferr := store.Finalize(outcomeCtx, accountID, token, paramHash, res); ferr != nil {
		// Launch succeeded; finalize failure only weakens future dedup — don't fail.
		slog.Warn("RunInstances: failed to finalize client-token record", "token", token, "err", ferr)
	}
	return res, nil
}
