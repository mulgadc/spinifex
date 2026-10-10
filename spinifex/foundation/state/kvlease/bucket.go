package kvlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	telemetry "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bounded wait for JetStream quorum on cold multi-node start. Vars (not consts) so tests can shrink them.
var (
	bucketRetryFor  = 60 * time.Second
	bucketRetryStep = 1 * time.Second
)

// BucketConfig describes a lease bucket. History is not a field: a lease key has
// no history worth keeping, so every lease bucket is created with one.
type BucketConfig struct {
	Name string

	// TTL expires a lease its holder died without releasing, so a crashed leader
	// does not park the key forever. It must match the lease's own Config.TTL.
	TTL time.Duration

	// Version runs the registered schema migrations for the bucket after it is
	// opened. Zero skips them, for a bucket holding nothing but lease keys.
	Version int
}

// OpenBucket attaches to a lease bucket, creating it only when genuinely absent,
// and runs any schema migration registered for it.
//
// Attach before create, because callers re-open this on every reconcile tick:
// CreateKeyValue against a bucket that already exists is a STREAM.CREATE the
// meta leader answers with an error, so creating first bills one per tick.
//
// A leader lease is the worst thing here to lose to a single node. Nobody can
// acquire a lease whose bucket has no quorum, so every reconciler that shares
// it stops at once, cluster-wide, at exactly the moment a node has failed.
func OpenBucket(ctx context.Context, js jetstream.JetStream, cfg BucketConfig) (jetstream.KeyValue, error) {
	replicas, err := clustersize.Replicas()
	if err != nil {
		return nil, fmt.Errorf("kvlease: open or create lease bucket %s: %w", cfg.Name, err)
	}

	kv, err := js.KeyValue(ctx, cfg.Name)
	switch {
	case errors.Is(err, jetstream.ErrBucketNotFound):
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:   cfg.Name,
			History:  1,
			TTL:      cfg.TTL,
			Replicas: replicas,
		})
	case err == nil:
		// A failed raise must not fail the open. This runs on every reconcile
		// tick, so refusing here would stop every reconciler sharing the bucket
		// — the outage this replica count exists to prevent — over a bucket that
		// is present and quorate at the count it already has.
		kvutil.TryRaiseBucketReplicas(ctx, js, cfg.Name, replicas)
	}
	if err != nil {
		return nil, fmt.Errorf("kvlease: open or create lease bucket %s: %w", cfg.Name, err)
	}
	if cfg.Version > 0 {
		if err := migrate.DefaultRegistry.RunKV(ctx, cfg.Name, kv, cfg.Version); err != nil {
			return nil, fmt.Errorf("migrate %s: %w", cfg.Name, err)
		}
	}
	slog.DebugContext(ctx, "kvlease: lease bucket ready", "bucket", cfg.Name, "ttl_ms", telemetry.Millis(cfg.TTL))
	return kv, nil
}

// NATSBucket is OpenBucket as a BucketFunc, retrying while JetStream is still
// forming quorum on a cold multi-node start. Callers with their own bucket
// initialiser should pass that instead.
func NATSBucket(nc *nats.Conn, bucket string, ttl time.Duration) BucketFunc {
	return func(ctx context.Context) (jetstream.KeyValue, error) {
		js, err := jetstream.New(nc)
		if err != nil {
			return nil, fmt.Errorf("kvlease: JetStream unavailable: %w", err)
		}
		deadline := time.Now().Add(bucketRetryFor)
		for {
			kv, err := OpenBucket(ctx, js, BucketConfig{Name: bucket, TTL: ttl})
			if err == nil {
				return kv, nil
			}
			// An undeclared cluster size does not become declared by waiting, so
			// it surfaces now rather than after the whole retry window.
			if clustersize.Permanent(err) {
				return nil, err
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("kvlease: KV bucket %q unreachable after %s: %w", bucket, bucketRetryFor, err)
			}
			// A shutdown mid-wait must not sit out the remaining retry window.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(bucketRetryStep):
			}
		}
	}
}

// StaticBucket adapts an already-open bucket to a BucketFunc, for callers handed
// a KeyValue rather than opening one themselves.
func StaticBucket(kv jetstream.KeyValue) BucketFunc {
	return func(context.Context) (jetstream.KeyValue, error) { return kv, nil }
}
