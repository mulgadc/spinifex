package idempotency

import (
	"context"
	"fmt"
	"log/slog"
)

// ErrNoReplay signals a finalized record that carries no payload, so there is
// nothing to return and re-running would defeat the token.
var ErrNoReplay = fmt.Errorf("%w: completed token has no payload to replay", ErrUnavailable)

// Do runs work under a token: the first caller runs it, duplicates replay its
// result. Errors from work are returned as-is; the token errors are this
// package's sentinels, which callers map onto their own API errors.
//
// EKS CreateCluster does not use this: it interleaves aborts through a long
// create and replays by re-reading the cluster rather than from the payload.
func Do[T any](
	ctx context.Context,
	store *Store[T],
	accountID, token, paramHash string,
	work func() (T, error),
) (T, error) {
	var zero T
	replay, owned, err := store.Claim(ctx, accountID, token, paramHash)
	if err != nil {
		return zero, err
	}
	if replay != nil {
		return *replay, nil
	}
	if !owned {
		return zero, ErrNoReplay
	}

	result, werr := work()

	// Settling the record outlives ctx: a caller that went away mid-request is
	// exactly when it must be settled, and leaving it in-flight parks every retry
	// of that token behind the poll deadline until the record ages out.
	outcomeCtx := context.WithoutCancel(ctx)
	if werr != nil {
		store.Abort(outcomeCtx, accountID, token)
		return zero, werr
	}
	if ferr := store.Finalize(outcomeCtx, accountID, token, paramHash, result); ferr != nil {
		// The work succeeded; a finalize failure only weakens later dedup, and
		// failing here would report a completed create as an error.
		slog.WarnContext(ctx, "idempotency: failed to finalize token record",
			"namespace", store.namespace, "token", token, "err", ferr)
	}
	return result, nil
}
