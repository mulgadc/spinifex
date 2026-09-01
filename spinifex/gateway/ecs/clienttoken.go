package gateway_ecs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// kvBucketClientTokens is the JetStream KV bucket for ECS ClientToken
	// records. ECS keeps its own rather than borrowing EC2's, whose name and TTL
	// belong to the EC2 gateway.
	kvBucketClientTokens = "spinifex-ecs-clienttokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clientTokenTTL must outlast SDK retry windows; short enough that a crashed
	// in-flight record ages out and frees the token for a fresh attempt.
	clientTokenTTL = 15 * time.Minute
)

// runTaskStore replays a whole RunTaskOutput: a placement can partly succeed, so
// the failures are as much a part of the result as the tasks.
type runTaskStore = idempotency.Store[ecs.RunTaskOutput]

var (
	ctStore        *runTaskStore
	ctOnce         sync.Once
	errCTStoreInit error
)

// getRunTaskStore lazily binds the process-wide store. Process-wide because the
// ECS handlers are free functions taking a *nats.Conn, with no instance to hang
// it on.
func getRunTaskStore(ctx context.Context, nc *nats.Conn) (*runTaskStore, error) {
	ctOnce.Do(func() {
		js, err := jetstream.New(nc)
		if err != nil {
			errCTStoreInit = fmt.Errorf("clienttoken jetstream: %w", err)
			return
		}
		// The bind happens once per process, so it must not inherit the first
		// caller's cancellation: a client that disconnects mid-open would poison
		// the store for every later launch.
		ctStore, errCTStoreInit = idempotency.OpenStore[ecs.RunTaskOutput](
			context.WithoutCancel(ctx), js, kvBucketClientTokens, clientTokenTTL)
	})
	return ctStore, errCTStoreInit
}

// runTaskParamHash hashes the request excluding ClientToken, so the same params
// always produce the same hash. Must run before any input mutation.
func runTaskParamHash(input *ecs.RunTaskInput) string {
	clone := *input
	clone.ClientToken = nil
	return idempotency.ParamHash(&clone)
}

// runTaskWithClientToken wraps a launch in ClientToken idempotency and maps the
// token errors onto the AWS ones RunTask returns. Extracted for unit-testability.
//
// A partial placement finalizes rather than aborts: some tasks are already
// running, and re-running would add more on top of them.
func runTaskWithClientToken(
	ctx context.Context,
	store *runTaskStore,
	accountID, token, paramHash string,
	launch func() (ecs.RunTaskOutput, error),
) (ecs.RunTaskOutput, error) {
	out, err := idempotency.Do(ctx, store, accountID, token, paramHash, launch)
	switch {
	case errors.Is(err, idempotency.ErrParamMismatch):
		return ecs.RunTaskOutput{}, errors.New(awserrors.ErrorIdempotentParameterMismatch)
	case errors.Is(err, idempotency.ErrUnavailable):
		slog.ErrorContext(ctx, "ECS RunTask: client-token claim failed", "token", token, "err", err)
		return ecs.RunTaskOutput{}, errors.New(awserrors.ErrorServerInternal)
	}
	return out, err
}
