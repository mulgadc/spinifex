package gateway_ec2_instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// kvBucketClientTokens is the JetStream KV bucket for ClientToken records.
	kvBucketClientTokens = "spinifex-ec2-clienttokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clientTokenTTL must outlast SDK retry windows; short enough that a crashed
	// in-flight record ages out and frees the token for a fresh launch.
	clientTokenTTL = 15 * time.Minute

	tokenStatusInFlight = "in-flight"
	tokenStatusDone     = "done"
)

// clientTokenWaitTimeout caps how long a duplicate caller polls an in-flight winner.
// clientTokenPollStep is the inter-poll sleep. Vars so tests can shrink them.
var (
	clientTokenWaitTimeout = 30 * time.Second
	clientTokenPollStep    = 250 * time.Millisecond
)

// errIdempotentParamMismatch signals the same token was reused with different
// request parameters (AWS IdempotentParameterMismatch).
var errIdempotentParamMismatch = errors.New("clienttoken: idempotent parameter mismatch")

// errClientTokenWaitTimeout signals a duplicate caller polled an in-flight
// winner past clientTokenWaitTimeout without the winner finishing.
var errClientTokenWaitTimeout = errors.New("clienttoken: timed out waiting for in-flight launch")

// tokenRecord is the per-token idempotency record stored in the KV bucket.
type tokenRecord struct {
	Status      string           `json:"status"`
	ParamHash   string           `json:"paramHash"`
	StartedAt   time.Time        `json:"startedAt"`
	Reservation *ec2.Reservation `json:"reservation,omitempty"`
}

// ClientTokenStore implements RunInstances ClientToken idempotency over a TTL KV
// bucket. The first caller owns the launch; duplicates replay (done) or poll (in-flight).
type ClientTokenStore struct {
	kv jetstream.KeyValue
}

var (
	ctStore        *ClientTokenStore
	ctOnce         sync.Once
	errCTStoreInit error
)

// getClientTokenStore lazily initialises the process-wide client-token store via sync.Once.
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
	kv, err := js.KeyValue(ctx, kvBucketClientTokens)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  kvBucketClientTokens,
			History: 1,
			TTL:     clientTokenTTL,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open client-token bucket: %w", err)
	}
	return &ClientTokenStore{kv: kv}, nil
}

func clientTokenKey(accountID, token string) string {
	return accountID + "." + token
}

// clientTokenParamHash hashes the request excluding ClientToken, so the same
// params always produce the same hash. Must run before any input mutation.
func clientTokenParamHash(input *ec2.RunInstancesInput) string {
	clone := *input
	clone.ClientToken = nil
	b, err := json.Marshal(&clone)
	if err != nil {
		// Fallback: idempotency degrades to name-only.
		b = []byte(clientTokenKey("", ""))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Claim attempts to own the launch for this token.
// Returns (nil, true, nil) when the caller owns the launch (must Finalize or Abort),
// (reservation, false, nil) to replay a completed launch, or an error on mismatch/timeout.
func (c *ClientTokenStore) Claim(ctx context.Context, accountID, token, paramHash string) (*ec2.Reservation, bool, error) {
	key := clientTokenKey(accountID, token)
	inflight, err := json.Marshal(tokenRecord{
		Status:    tokenStatusInFlight,
		ParamHash: paramHash,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("clienttoken marshal: %w", err)
	}
	if _, cerr := c.kv.Create(ctx, key, inflight); cerr == nil {
		return nil, true, nil
	} else if !errors.Is(cerr, jetstream.ErrKeyExists) {
		return nil, false, fmt.Errorf("clienttoken create %s: %w", key, cerr)
	}

	deadline := time.Now().Add(clientTokenWaitTimeout)
	for {
		entry, gerr := c.kv.Get(ctx, key)
		if gerr != nil {
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				// Owner aborted or record aged out: race for ownership.
				if _, rcerr := c.kv.Create(ctx, key, inflight); rcerr == nil {
					return nil, true, nil
				} else if !errors.Is(rcerr, jetstream.ErrKeyExists) {
					return nil, false, fmt.Errorf("clienttoken recreate %s: %w", key, rcerr)
				}
				continue
			}
			return nil, false, fmt.Errorf("clienttoken get %s: %w", key, gerr)
		}
		var rec tokenRecord
		if uerr := json.Unmarshal(entry.Value(), &rec); uerr != nil {
			return nil, false, fmt.Errorf("clienttoken unmarshal %s: %w", key, uerr)
		}
		if rec.ParamHash != paramHash {
			return nil, false, errIdempotentParamMismatch
		}
		if rec.Status == tokenStatusDone {
			return rec.Reservation, false, nil
		}
		if time.Now().After(deadline) {
			return nil, false, errClientTokenWaitTimeout
		}
		time.Sleep(clientTokenPollStep)
	}
}

// Finalize marks the token done with the reservation so duplicates can replay it.
func (c *ClientTokenStore) Finalize(ctx context.Context, accountID, token, paramHash string, res *ec2.Reservation) error {
	key := clientTokenKey(accountID, token)
	data, err := json.Marshal(tokenRecord{
		Status:      tokenStatusDone,
		ParamHash:   paramHash,
		StartedAt:   time.Now().UTC(),
		Reservation: res,
	})
	if err != nil {
		return fmt.Errorf("clienttoken finalize marshal: %w", err)
	}
	if _, err := c.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("clienttoken finalize put %s: %w", key, err)
	}
	return nil
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
		if errors.Is(cerr, errIdempotentParamMismatch) {
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
	if ferr := store.Finalize(outcomeCtx, accountID, token, paramHash, &res); ferr != nil {
		// Launch succeeded; finalize failure only weakens future dedup — don't fail.
		slog.Warn("RunInstances: failed to finalize client-token record", "token", token, "err", ferr)
	}
	return res, nil
}

// Abort drops the in-flight token so a retry can re-launch.
func (c *ClientTokenStore) Abort(ctx context.Context, accountID, token string) {
	key := clientTokenKey(accountID, token)
	if err := c.kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.Warn("clienttoken: failed to abort token record", "key", key, "err", err)
	}
}
