package reconcile

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// Single CAS-elected leader key; TTL bounds crash-recovery.
const (
	reconcileLeaderBucket = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
	reconcileLeaderTTL    = 60 * time.Second
)

// Bounded wait for JetStream quorum on cold multi-node start. Vars (not
// consts) so tests can shrink them.
var (
	leaderRetryFor  = 60 * time.Second
	leaderRetryStep = 1 * time.Second
)

// AcquireLeader returns (release, true) exactly once across all vpcds, or
// (nil, false) for losers and JetStream-unreachable cases.
func AcquireLeader(nc *nats.Conn, holder string) (func(), bool) {
	js, _ := nc.JetStream()

	var (
		kv  nats.KeyValue
		err error
	)
	deadline := time.Now().Add(leaderRetryFor)
	for {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:  reconcileLeaderBucket,
			History: 1,
			TTL:     reconcileLeaderTTL,
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			slog.Error("reconcile/lock: JetStream KV unreachable after retry, skipping reconcile",
				"holder", holder, "waited", leaderRetryFor, "err", err)
			return nil, false
		}
		slog.Debug("reconcile/lock: JetStream KV not ready, retrying", "holder", holder, "err", err)
		time.Sleep(leaderRetryStep)
	}

	if _, err := kv.Create(reconcileLeaderKey, []byte(holder)); err != nil {
		slog.Info("reconcile/lock: another vpcd is leader, skipping reconcile", "holder", holder, "err", err)
		return nil, false
	}

	slog.Info("reconcile/lock: elected", "holder", holder)
	return func() {
		if err := kv.Delete(reconcileLeaderKey); err != nil {
			slog.Warn("reconcile/lock: failed to release lock (TTL will reap)", "holder", holder, "err", err)
		}
	}, true
}
