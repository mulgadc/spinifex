package reconcile

import (
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// Single CAS-elected leader key; TTL bounds crash-recovery.
const (
	KVBucketVPCDReconcile = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
	reconcileLeaderTTL    = 60 * time.Second
)

// Bounded wait for JetStream quorum on cold multi-node start. Vars (not
// consts) so tests can shrink them.
var (
	leaderRetryFor  = 60 * time.Second
	leaderRetryStep = 1 * time.Second
)

// AcquireLeader elects one leader on the named lock bucket. Independent
// reconcile loops pass distinct buckets so they never share a single mutex: the
// gateway quota reconcile must not block vpcd's network reconcile, and vice
// versa.
func AcquireLeader(nc *nats.Conn, bucket, holder string) (func(), bool) {
	js, err := nc.JetStream()
	if err != nil {
		slog.Error("reconcile/lock: JetStream unavailable, skipping reconcile",
			"holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}

	var kv nats.KeyValue
	deadline := time.Now().Add(leaderRetryFor)
	for {
		// Get-or-create: CreateKeyValue returns "stream name already in use" if
		// the bucket exists; attach first, create only when genuinely absent.
		kv, err = js.KeyValue(bucket)
		if errors.Is(err, nats.ErrBucketNotFound) {
			kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
				Bucket:  bucket,
				History: 1,
				TTL:     reconcileLeaderTTL,
			})
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			slog.Error("reconcile/lock: JetStream KV unreachable after retry, skipping reconcile",
				"holder", holder, "bucket", bucket, "waited", leaderRetryFor, "err", err)
			return nil, false
		}
		slog.Debug("reconcile/lock: JetStream KV not ready, retrying", "holder", holder, "bucket", bucket, "err", err)
		time.Sleep(leaderRetryStep)
	}

	if _, err := kv.Create(reconcileLeaderKey, []byte(holder)); err != nil {
		slog.Info("reconcile/lock: another holder is leader, skipping reconcile", "holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}

	slog.Info("reconcile/lock: elected", "holder", holder, "bucket", bucket)
	return func() {
		if err := kv.Delete(reconcileLeaderKey); err != nil {
			slog.Warn("reconcile/lock: failed to release lock (TTL will reap)", "holder", holder, "bucket", bucket, "err", err)
		}
	}, true
}
