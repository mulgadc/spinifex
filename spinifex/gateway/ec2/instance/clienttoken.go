package gateway_ec2_instance

import (
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
)

const (
	// kvBucketClientTokens holds RunInstances ClientToken idempotency records.
	kvBucketClientTokens = "spinifex-ec2-clienttokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clientTokenTTL bounds how long a token replays. It must outlast normal
	// SDK retry windows yet be short enough that a crashed in-flight record
	// (winner died mid-launch) ages out and frees the token for a fresh launch.
	clientTokenTTL = 15 * time.Minute

	tokenStatusInFlight = "in-flight"
	tokenStatusDone     = "done"
)

// clientTokenWaitTimeout caps how long a duplicate caller polls an in-flight
// winner before giving up (the SDK retries the whole call); clientTokenPollStep
// is the inter-poll sleep. Vars (not consts) so tests can shrink them.
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

// tokenRecord is the per-token idempotency record. ParamHash binds the token to
// the original request so a reuse with different params is rejected; Reservation
// is populated only once Status is done so a duplicate caller can replay it.
type tokenRecord struct {
	Status      string           `json:"status"`
	ParamHash   string           `json:"paramHash"`
	StartedAt   time.Time        `json:"startedAt"`
	Reservation *ec2.Reservation `json:"reservation,omitempty"`
}

// ClientTokenStore implements RunInstances ClientToken idempotency over a
// dedicated TTL KV bucket. The first caller to Create a token record owns the
// launch; duplicates either replay the stored reservation (done) or poll until
// the owner finishes (in-flight).
type ClientTokenStore struct {
	kv nats.KeyValue
}

var (
	ctStore        *ClientTokenStore
	ctOnce         sync.Once
	errCTStoreInit error
)

// getClientTokenStore lazily opens (or creates) the process-wide client-token
// store. The gateway holds a single long-lived NATS connection, so a sync.Once
// bind is sufficient; the bucket is get-or-created with a TTL.
func getClientTokenStore(nc *nats.Conn) (*ClientTokenStore, error) {
	ctOnce.Do(func() {
		js, err := nc.JetStream()
		if err != nil {
			errCTStoreInit = fmt.Errorf("clienttoken jetstream: %w", err)
			return
		}
		ctStore, errCTStoreInit = newClientTokenStore(js)
	})
	return ctStore, errCTStoreInit
}

func newClientTokenStore(js nats.JetStreamContext) (*ClientTokenStore, error) {
	kv, err := js.KeyValue(kvBucketClientTokens)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
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

// clientTokenParamHash hashes the caller's request, ignoring ClientToken, so the
// same token with identical params matches and with different params mismatches.
// It MUST run before any mutation of input (e.g. instance-profile ARN rewrite).
func clientTokenParamHash(input *ec2.RunInstancesInput) string {
	clone := *input
	clone.ClientToken = nil
	b, err := json.Marshal(&clone)
	if err != nil {
		// Marshal of an SDK struct does not fail in practice; fall back to a
		// constant so a token still claims (idempotency degrades to name-only).
		b = []byte(clientTokenKey("", ""))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Claim runs the two-phase token claim. It returns:
//   - (nil, true, nil)        ⇒ caller owns the launch; must Finalize or Abort.
//   - (reservation, false, nil) ⇒ replay a completed launch; return it directly.
//   - (nil, false, err)       ⇒ errIdempotentParamMismatch (token reused with
//     different params) or errClientTokenWaitTimeout (in-flight winner too slow).
func (c *ClientTokenStore) Claim(accountID, token, paramHash string) (*ec2.Reservation, bool, error) {
	key := clientTokenKey(accountID, token)
	inflight, err := json.Marshal(tokenRecord{
		Status:    tokenStatusInFlight,
		ParamHash: paramHash,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("clienttoken marshal: %w", err)
	}
	if _, cerr := c.kv.Create(key, inflight); cerr == nil {
		return nil, true, nil
	} else if !errors.Is(cerr, nats.ErrKeyExists) {
		return nil, false, fmt.Errorf("clienttoken create %s: %w", key, cerr)
	}

	deadline := time.Now().Add(clientTokenWaitTimeout)
	for {
		entry, gerr := c.kv.Get(key)
		if gerr != nil {
			if errors.Is(gerr, nats.ErrKeyNotFound) {
				// Owner aborted (transient launch failure) or the record aged
				// out: race for ownership again.
				if _, rcerr := c.kv.Create(key, inflight); rcerr == nil {
					return nil, true, nil
				} else if !errors.Is(rcerr, nats.ErrKeyExists) {
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

// Finalize stamps the token record done with the launched reservation so
// duplicate callers replay it. The owner is the sole writer, so a plain Put is
// safe and refreshes the TTL.
func (c *ClientTokenStore) Finalize(accountID, token, paramHash string, res *ec2.Reservation) error {
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
	if _, err := c.kv.Put(key, data); err != nil {
		return fmt.Errorf("clienttoken finalize put %s: %w", key, err)
	}
	return nil
}

// runInstancesWithClientToken wraps a launch in ClientToken idempotency: it
// claims the token, replays a completed reservation, or (as the owner) runs
// launch and finalizes the result — aborting the token on a launch failure so a
// retry can re-launch. AWS error codes are mapped here; the launch closure
// returns the raw reservation/error. Extracted from RunInstances so the
// idempotency flow is unit-testable without a live gateway.
func runInstancesWithClientToken(
	store *ClientTokenStore,
	accountID, token, paramHash string,
	launch func() (ec2.Reservation, error),
) (ec2.Reservation, error) {
	var zero ec2.Reservation
	replay, owned, cerr := store.Claim(accountID, token, paramHash)
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
	if rerr != nil {
		store.Abort(accountID, token)
		return zero, rerr
	}
	if ferr := store.Finalize(accountID, token, paramHash, &res); ferr != nil {
		// Launch succeeded; failing to persist the replay record only weakens a
		// future retry's dedup, so do not fail the response.
		slog.Warn("RunInstances: failed to finalize client-token record", "token", token, "err", ferr)
	}
	return res, nil
}

// Abort drops an in-flight token after a launch failure so a retry with the
// same token can re-launch instead of replaying a non-existent reservation.
func (c *ClientTokenStore) Abort(accountID, token string) {
	key := clientTokenKey(accountID, token)
	if err := c.kv.Delete(key); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		slog.Warn("clienttoken: failed to abort token record", "key", key, "err", err)
	}
}
