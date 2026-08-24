package kvlease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bounded wait for JetStream quorum on cold multi-node start. Vars (not consts) so tests can shrink them.
var (
	bucketRetryFor  = 60 * time.Second
	bucketRetryStep = 1 * time.Second
)

// NATSBucket attaches to the named bucket, creating it only when genuinely absent, and
// retrying while JetStream is still forming quorum. Callers with their own bucket initialiser
// should pass that instead.
func NATSBucket(nc *nats.Conn, bucket string, ttl time.Duration) BucketFunc {
	return func(ctx context.Context) (jetstream.KeyValue, error) {
		js, err := jetstream.New(nc)
		if err != nil {
			return nil, fmt.Errorf("kvlease: JetStream unavailable: %w", err)
		}
		deadline := time.Now().Add(bucketRetryFor)
		for {
			// CreateKeyValue reports "stream name already in use" when the bucket exists, so
			// attach first and create only when genuinely absent.
			kv, err := js.KeyValue(ctx, bucket)
			if errors.Is(err, jetstream.ErrBucketNotFound) {
				kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
					Bucket:  bucket,
					History: 1,
					TTL:     ttl,
				})
			}
			if err == nil {
				return kv, nil
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
