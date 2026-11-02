package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// deletingReapMinAge is the base of the reaper's retry backoff after a failed
// teardown pass. It is not a race guard: the teardown lease and the in-process
// gate already keep a second converger off a cluster. Var for tests.
var deletingReapMinAge = 90 * time.Second

// deleteReapBackoffCap bounds the retry interval, so a cluster whose teardown
// keeps failing is still retried every half hour for as long as it takes.
var deleteReapBackoffCap = 30 * time.Minute

// EKSDeletingReaper is the backstop for recorded cluster deletes (ADR-0003:S2,
// S8). DeleteCluster records DELETING and starts the teardown itself; this
// reaper finds every DELETING record whose teardown is not running, because the
// kick died with its daemon or a pass failed, and hands it to the same
// converger. Node-local: the lease decides which node runs each cluster.
type EKSDeletingReaper struct {
	svc    *EKSServiceImpl
	minAge time.Duration
}

var _ vm.Reaper = (*EKSDeletingReaper)(nil)

// NewDeletingReaper builds the EKS DELETING teardown backstop.
func (s *EKSServiceImpl) NewDeletingReaper() *EKSDeletingReaper {
	return &EKSDeletingReaper{svc: s, minAge: deletingReapMinAge}
}

func (r *EKSDeletingReaper) Class() string         { return "eks-deleting" }
func (r *EKSDeletingReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep hands every due DELETING cluster to a background converger and
// returns how many it started. It never runs a teardown inline, since the GC
// runs every reaper in sequence and one teardown can take minutes.
func (r *EKSDeletingReaper) Sweep(ctx context.Context) (int, error) {
	if !r.svc.depsReadyForOrchestration() {
		return 0, nil
	}
	js, err := jetstream.New(r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}
	// A truncated bucket listing would silently leave a recorded delete in place
	// for another sweep interval, so an incomplete enumeration fails the sweep.
	names, err := accountBucketNames(ctx, r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("enumerate account buckets: %w", err)
	}

	started := 0
	for _, name := range names {
		select {
		case <-ctx.Done():
			return started, ctx.Err()
		default:
		}
		accountID := strings.TrimPrefix(name, KVBucketEKSAccountPrefix)
		acctKV, err := js.KeyValue(ctx, name)
		if err != nil {
			continue
		}
		clusters, err := listClusterNames(ctx, acctKV)
		if err != nil {
			continue
		}
		for _, cluster := range clusters {
			meta, err := GetClusterMeta(ctx, acctKV, cluster)
			if err != nil || !r.due(meta) {
				continue
			}
			if r.svc.startTeardown(accountID, cluster) {
				slog.Info("eks-deleting: resuming recorded cluster delete",
					"cluster", cluster, "account", accountID, "attempts", meta.DeleteReapAttempts,
					"exhausted", meta.DeleteReapExhausted)
				started++
			}
		}
	}
	return started, nil
}

// due reports whether a record is DELETING and past its retry backoff. A
// record no pass has touched is due at once; the gate and lease turn the
// sweep away if its kick is still running.
func (r *EKSDeletingReaper) due(meta *ClusterMeta) bool {
	if meta.Status != ClusterStatusDeleting {
		return false
	}
	if meta.DeleteReapAttempts == 0 || meta.LastDeleteReapAttempt.IsZero() {
		return true
	}
	return time.Since(meta.LastDeleteReapAttempt) >= deleteReapBackoff(r.minAge, meta.DeleteReapAttempts)
}

// deleteReapBackoff is the wait after the attempts-th teardown pass before the
// next: minAge after the first, doubling after each, capped at
// deleteReapBackoffCap.
func deleteReapBackoff(minAge time.Duration, attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	d := minAge
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= deleteReapBackoffCap {
			return deleteReapBackoffCap
		}
	}
	return min(d, deleteReapBackoffCap)
}
