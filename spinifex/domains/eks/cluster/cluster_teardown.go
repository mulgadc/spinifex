package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvlease"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	telemetry "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go/jetstream"
)

// teardownConcurrency bounds how many cluster teardowns one node runs at once.
// Request kicks and reaper hand-offs share it; one refused is left to the
// reaper's next sweep rather than queued. Var for tests.
var teardownConcurrency = 4

// maxTeardownPasses bounds how often one converger run re-passes because a
// writer added refs to the record while it was tearing down.
const maxTeardownPasses = 5

// Teardown lease timing. TTL must match the leader bucket's TTL; vars for tests.
var (
	teardownLeaseTTL   = KVBucketEKSLeaderTTL
	teardownLeaseRenew = defaultReconcileLeaseRefresh
)

var (
	errTeardownNotRecorded = errors.New("eks: cluster is not DELETING, teardown refused")
	errTeardownLeaseLost   = errors.New("eks: teardown lease lost")
	errNodegroupsAttached  = errors.New("eks: cluster still has node groups")
)

// teardownLeaderKey is the per-cluster cross-node gate for a DELETING
// teardown. It is distinct from reconcilerLeaderKey so a delete never contends
// with the still-live reconciler, which holds that lease until it reads DELETING.
func teardownLeaderKey(accountID, clusterName string) string {
	return reconcilerLeaderKey(accountID, clusterName) + "/teardown"
}

// teardownLease is a held teardown lease: lost closes if renewal loses the key.
type teardownLease struct {
	release func()
	lost    <-chan struct{}
}

// acquireTeardownLease takes the per-cluster teardown lease, renewing it on
// ctx. The lease re-adopts a key its own holder already has, so it serialises
// nodes, not goroutines; startTeardown's in-process gate covers the latter. A
// nil leaderKV (single-node/test) skips gating.
func (s *EKSServiceImpl) acquireTeardownLease(ctx context.Context, accountID, clusterName string) (teardownLease, bool) {
	if s.leaderKV == nil {
		return teardownLease{release: func() {}}, true
	}
	key := teardownLeaderKey(accountID, clusterName)
	lease, err := kvlease.New(kvlease.Config{
		Name:   "eks/teardown",
		Bucket: kvlease.StaticBucket(s.leaderKV),
		Key:    key,
		Holder: s.deps.HolderID,
		Attrs:  []any{"account", accountID, "cluster", clusterName},
		TTL:    teardownLeaseTTL,
		Renew:  teardownLeaseRenew,
	})
	if err != nil {
		slog.ErrorContext(ctx, "eks: teardown lease config invalid", "key", key, "err", err)
		return teardownLease{}, false
	}
	if !lease.TryAcquire(ctx) {
		return teardownLease{}, false
	}
	return teardownLease{release: func() { lease.Release(ctx) }, lost: lease.Lost()}, true
}

// startTeardown hands one DELETING cluster to a background converger and
// reports whether it did. It refuses when this process already runs that
// cluster's teardown, when the node is at teardownConcurrency, or when another
// node holds the lease; the record stays DELETING for the reaper either way.
func (s *EKSServiceImpl) startTeardown(accountID, name string) bool {
	key := teardownLeaderKey(accountID, name)
	s.teardownMu.Lock()
	if s.teardownRunning == nil {
		s.teardownRunning = map[string]struct{}{}
	}
	_, busy := s.teardownRunning[key]
	if busy || len(s.teardownRunning) >= teardownConcurrency {
		s.teardownMu.Unlock()
		return false
	}
	s.teardownRunning[key] = struct{}{}
	s.teardownMu.Unlock()
	leave := func() {
		s.teardownMu.Lock()
		delete(s.teardownRunning, key)
		s.teardownMu.Unlock()
	}

	// The lease renews on the service's context so it outlives the request and
	// stops with the daemon.
	lease, ok := s.acquireTeardownLease(s.bgCtx, accountID, name)
	if !ok {
		leave()
		return false
	}
	s.teardownWG.Go(func() {
		defer leave()
		defer lease.release()
		started := time.Now()
		if err := s.convergeClusterDeletion(s.bgCtx, accountID, name, lease.lost); err != nil {
			slog.Warn("eks: cluster teardown incomplete, the record stays DELETING for retry",
				"cluster", name, "account", accountID, "err", err)
			return
		}
		slog.Info("eks: cluster teardown complete", "cluster", name, "account", accountID,
			"duration_ms", telemetry.Millis(time.Since(started)))
	})
	return true
}

// WaitTeardowns blocks until every teardown this process started has returned.
// Intended for tests.
func (s *EKSServiceImpl) WaitTeardowns() { s.teardownWG.Wait() }

// convergeClusterDeletion finishes a recorded delete: it removes every
// dependant the record names and then the record itself, last. It acts only
// on a record that is DELETING when re-read under the lease, keeps the record
// with its refs on any failure, and stops at a step boundary if lost closes.
func (s *EKSServiceImpl) convergeClusterDeletion(ctx context.Context, accountID, name string, lost <-chan struct{}) error {
	js, err := jetstream.New(s.deps.NATSConn)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(ctx, js, accountID)
	if err != nil {
		return fmt.Errorf("get account bucket: %w", err)
	}
	checkpoint := func() error {
		select {
		case <-lost:
			return errTeardownLeaseLost
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	for range maxTeardownPasses {
		if err := checkpoint(); err != nil {
			return err
		}
		if _, err := RecordDeleteReapAttempt(ctx, acctKV, name); err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return nil
			}
			return fmt.Errorf("record teardown attempt: %w", err)
		}
		meta, err := GetClusterMeta(ctx, acctKV, name)
		if err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return nil
			}
			return err
		}
		if meta.Status != ClusterStatusDeleting {
			return errTeardownNotRecorded
		}
		if err := s.refuseWhileNodegroups(ctx, acctKV, name); err != nil {
			return err
		}

		steps, terr := s.teardownClusterInfra(ctx, accountID, name, meta, acctKV, checkpoint)
		if terr != nil {
			if errors.Is(terr, errTeardownLeaseLost) || ctx.Err() != nil {
				return terr
			}
			s.recordTeardownFailure(ctx, acctKV, name, steps, terr)
			return terr
		}

		done, err := s.removeClusterRecord(ctx, acctKV, name, meta)
		if err != nil {
			if !errors.Is(err, errNodegroupsAttached) && !errors.Is(err, errTeardownNotRecorded) {
				s.recordTeardownFailure(ctx, acctKV, name, []string{stepRecord}, err)
			}
			return err
		}
		if done {
			return nil
		}
		slog.Info("eks: cluster record gained refs during teardown, running another pass", "cluster", name)
	}
	return fmt.Errorf("eks: cluster %s record kept changing across %d teardown passes", name, maxTeardownPasses)
}

// refuseWhileNodegroups keeps the record when a node group is still recorded
// under it. Removing the record would orphan the group's running workers, and
// DeleteNodegroup still works on a DELETING cluster to clear them.
func (s *EKSServiceImpl) refuseWhileNodegroups(ctx context.Context, acctKV jetstream.KeyValue, name string) error {
	groups, err := ListNodegroupRecords(ctx, acctKV, name)
	if err != nil {
		return fmt.Errorf("list node groups: %w", err)
	}
	if len(groups) == 0 {
		return nil
	}
	s.recordTeardownFailure(ctx, acctKV, name, []string{stepNodegroups}, errNodegroupsAttached)
	return errNodegroupsAttached
}

// removeClusterRecord deletes the cluster's KV state once a pass has torn down
// everything tornDown recorded. It reports false, with nothing deleted from the
// meta, when the record now names a ref tornDown did not, so the caller runs
// another pass; the meta is deleted last and only at the revision it checked.
func (s *EKSServiceImpl) removeClusterRecord(ctx context.Context, acctKV jetstream.KeyValue, name string, tornDown *ClusterMeta) (bool, error) {
	cur, err := GetClusterMeta(ctx, acctKV, name)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return true, nil
		}
		return false, err
	}
	if cur.Status != ClusterStatusDeleting {
		return false, errTeardownNotRecorded
	}
	if refsAddedSince(tornDown, cur) {
		return false, nil
	}
	if err := s.refuseWhileNodegroups(ctx, acctKV, name); err != nil {
		return false, err
	}
	if err := DeleteClusterChildren(ctx, acctKV, name); err != nil {
		return false, err
	}

	errRefsAdded := errors.New("refs added")
	_, err = kvutil.DeleteIf(ctx, acctKV, ClusterMetaKey(name), kvutil.CASConfig{
		Attempts: maxClusterStateCASRetries,
		NotFound: ErrClusterNotFound,
	}, func(m *ClusterMeta) error {
		if m.Status != ClusterStatusDeleting {
			return errTeardownNotRecorded
		}
		if refsAddedSince(tornDown, m) {
			return errRefsAdded
		}
		return nil
	})
	switch {
	case err == nil, errors.Is(err, ErrClusterNotFound):
		return true, nil
	case errors.Is(err, errRefsAdded):
		return false, nil
	default:
		return false, err
	}
}

func (s *EKSServiceImpl) recordTeardownFailure(ctx context.Context, acctKV jetstream.KeyValue, name string, steps []string, terr error) {
	if err := RecordDeleteReapFailure(ctx, acctKV, name, steps, terr); err != nil && !errors.Is(err, ErrClusterNotFound) {
		slog.WarnContext(ctx, "eks: record teardown failure", "cluster", name, "err", err)
	}
}
