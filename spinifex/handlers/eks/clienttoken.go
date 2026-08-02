package handlers_eks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// kvBucketClusterTokens is the JetStream KV bucket for CreateCluster
	// ClientRequestToken idempotency records.
	kvBucketClusterTokens = "spinifex-eks-clustertokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clusterTokenTTL only needs to outlast SDK retry windows around the fast
	// (synchronous) phase of CreateCluster, not the whole async cluster launch:
	// a duplicate replays the CREATING/ACTIVE/FAILED meta by name, which the
	// cluster's own KV record — not this one — keeps current.
	clusterTokenTTL = 15 * time.Minute

	clusterTokenStatusInFlight = "in-flight"
	clusterTokenStatusDone     = "done"
)

// clusterTokenWaitTimeout caps how long a duplicate caller polls an in-flight
// winner. clusterTokenPollStep is the inter-poll sleep. Vars so tests can shrink them.
var (
	clusterTokenWaitTimeout = 30 * time.Second
	clusterTokenPollStep    = 250 * time.Millisecond
)

// errClusterTokenParamMismatch signals the same ClientRequestToken was reused
// with different CreateClusterInput parameters (AWS IdempotentParameterMismatch).
var errClusterTokenParamMismatch = errors.New("clienttoken: idempotent parameter mismatch")

// errClusterTokenWaitTimeout signals a duplicate caller polled an in-flight
// winner past clusterTokenWaitTimeout without the winner finishing.
var errClusterTokenWaitTimeout = errors.New("clienttoken: timed out waiting for in-flight create")

// clusterTokenRecord is the per-token idempotency record stored in the KV bucket.
type clusterTokenRecord struct {
	Status      string    `json:"status"`
	ParamHash   string    `json:"paramHash"`
	StartedAt   time.Time `json:"startedAt"`
	ClusterName string    `json:"clusterName,omitempty"`
}

// ClusterTokenStore implements CreateCluster ClientRequestToken idempotency over
// a TTL KV bucket, mirroring the EC2 RunInstances ClientToken pattern (see
// gateway/ec2/instance/clienttoken.go): the first caller owns the create;
// duplicates replay (done) or poll (in-flight).
type ClusterTokenStore struct {
	kv jetstream.KeyValue
}

// getClusterTokenStore lazily initialises s's cluster-token store via sync.Once,
// scoped to the service instance (not the process) so each EKSServiceImpl binds
// its own NATSConn — tests constructing independent instances never bleed a
// stale connection into one another.
func (s *EKSServiceImpl) getClusterTokenStore(ctx context.Context) (*ClusterTokenStore, error) {
	s.clusterTokenOnce.Do(func() {
		js, err := jetstream.New(s.deps.NATSConn)
		if err != nil {
			s.clusterTokenErr = fmt.Errorf("clustertoken jetstream: %w", err)
			return
		}
		// The bind happens once per service instance, so it must not inherit the
		// first caller's cancellation: a client that disconnects mid-open would
		// poison the store for every later create. Deadline-free, so the open
		// falls back to the JetStream API's own timeout.
		s.clusterTokenStore, s.clusterTokenErr = newClusterTokenStore(context.WithoutCancel(ctx), js)
	})
	return s.clusterTokenStore, s.clusterTokenErr
}

func newClusterTokenStore(ctx context.Context, js jetstream.JetStream) (*ClusterTokenStore, error) {
	kv, err := js.KeyValue(ctx, kvBucketClusterTokens)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  kvBucketClusterTokens,
			History: 1,
			TTL:     clusterTokenTTL,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open cluster-token bucket: %w", err)
	}
	return &ClusterTokenStore{kv: kv}, nil
}

func clusterTokenKey(accountID, token string) string {
	return accountID + "." + token
}

// clusterTokenParamHash hashes the request excluding ClientRequestToken, so
// identical params always produce the same hash. Must run before any input mutation.
func clusterTokenParamHash(input *eks.CreateClusterInput) string {
	clone := *input
	clone.ClientRequestToken = nil
	b, err := json.Marshal(&clone)
	if err != nil {
		// Fallback: idempotency degrades to name-only.
		b = []byte(clusterTokenKey("", ""))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Claim attempts to own the create for this token.
// Returns ("", true, nil) when the caller owns the create (must Finalize or Abort),
// (clusterName, false, nil) to replay a completed create, or an error on mismatch/timeout.
func (c *ClusterTokenStore) Claim(ctx context.Context, accountID, token, paramHash string) (string, bool, error) {
	key := clusterTokenKey(accountID, token)
	inflight, err := json.Marshal(clusterTokenRecord{
		Status:    clusterTokenStatusInFlight,
		ParamHash: paramHash,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", false, fmt.Errorf("clustertoken marshal: %w", err)
	}
	if _, cerr := c.kv.Create(ctx, key, inflight); cerr == nil {
		return "", true, nil
	} else if !errors.Is(cerr, jetstream.ErrKeyExists) {
		return "", false, fmt.Errorf("clustertoken create %s: %w", key, cerr)
	}

	deadline := time.Now().Add(clusterTokenWaitTimeout)
	for {
		entry, gerr := c.kv.Get(ctx, key)
		if gerr != nil {
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				// Owner aborted or record aged out: race for ownership.
				if _, rcerr := c.kv.Create(ctx, key, inflight); rcerr == nil {
					return "", true, nil
				} else if !errors.Is(rcerr, jetstream.ErrKeyExists) {
					return "", false, fmt.Errorf("clustertoken recreate %s: %w", key, rcerr)
				}
				continue
			}
			return "", false, fmt.Errorf("clustertoken get %s: %w", key, gerr)
		}
		var rec clusterTokenRecord
		if uerr := json.Unmarshal(entry.Value(), &rec); uerr != nil {
			return "", false, fmt.Errorf("clustertoken unmarshal %s: %w", key, uerr)
		}
		if rec.ParamHash != paramHash {
			return "", false, errClusterTokenParamMismatch
		}
		if rec.Status == clusterTokenStatusDone {
			return rec.ClusterName, false, nil
		}
		if time.Now().After(deadline) {
			return "", false, errClusterTokenWaitTimeout
		}
		time.Sleep(clusterTokenPollStep)
	}
}

// Finalize marks the token done with the created cluster's name, so duplicates
// replay its current state via GetClusterMeta rather than a frozen snapshot.
func (c *ClusterTokenStore) Finalize(ctx context.Context, accountID, token, paramHash, clusterName string) error {
	key := clusterTokenKey(accountID, token)
	data, err := json.Marshal(clusterTokenRecord{
		Status:      clusterTokenStatusDone,
		ParamHash:   paramHash,
		StartedAt:   time.Now().UTC(),
		ClusterName: clusterName,
	})
	if err != nil {
		return fmt.Errorf("clustertoken finalize marshal: %w", err)
	}
	if _, err := c.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("clustertoken finalize put %s: %w", key, err)
	}
	return nil
}

// Abort drops the in-flight token so a retry can re-attempt the create.
func (c *ClusterTokenStore) Abort(ctx context.Context, accountID, token string) {
	key := clusterTokenKey(accountID, token)
	if err := c.kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.Warn("clienttoken: failed to abort token record", "key", key, "err", err)
	}
}
