package reconcile

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// reconcileLeaderBucket holds a single CAS-elected leader key. MaxAge bounds
// recovery time when an elected vpcd crashes before releasing the lock.
const (
	reconcileLeaderBucket = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
	reconcileLeaderTTL    = 60 * time.Second
)

// Bounded wait for JetStream to come up at vpcd startup. On a cold multi-node
// start NATS clustering takes a few seconds and js.CreateKeyValue returns
// "context deadline exceeded" until quorum forms. Retrying makes the election
// deterministic; without it every vpcd falls through and races the same OVN
// NB transactions (mulga-js-72). Vars (not consts) so tests can shrink them.
var (
	leaderRetryFor  = 60 * time.Second
	leaderRetryStep = 1 * time.Second
)

// AcquireLeader returns release+true exactly once across all vpcds in a
// cluster, gating the startup Reconcile pass. Other vpcds get release=nil,
// elected=false and skip reconcile. Bounded retry on JetStream-not-ready
// keeps cold-multi-node-boot deterministic; on exhaustion we return
// elected=false so a follow-up retry (systemd Restart=on-failure) can run.
//
// Exported with the same contract as the legacy
// services/vpcd.AcquireReconcileLeader — call sites in services/vpcd/vpcd.go
// switch to this package by changing the import path only.
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
