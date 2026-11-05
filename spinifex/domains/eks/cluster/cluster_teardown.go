package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
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

var (
	errTeardownNotRecorded = errors.New("eks: cluster is not DELETING, teardown refused")
	errTeardownLeaseLost   = errors.New("eks: teardown lease lost")
	errNodegroupsAttached  = errors.New("eks: cluster still has node groups")
	errLaunchInFlight      = errors.New("eks: cluster create launch still running")
)

// teardownLease is a held teardown lease: lost closes if renewal loses the key.
type teardownLease struct {
	release func()
	lost    <-chan struct{}
}

// acquireTeardownLease takes the per-cluster teardown lease, renewing it until
// released. It refuses when another node holds it or another goroutine here does.
func (s *EKSServiceImpl) acquireTeardownLease(ctx context.Context, accountID, clusterName string) (teardownLease, bool) {
	lease := s.leases.Get(teardownLeaseName(accountID, clusterName))
	tok, err := lease.TryLock(ctx)
	if err != nil {
		if _, held := errors.AsType[*kvsync.HeldError](err); !held {
			slog.WarnContext(ctx, "eks: teardown lease attempt failed", "key", lease.Key(), "err", err)
		}
		return teardownLease{}, false
	}
	return teardownLease{release: func() { lease.Unlock(ctx, tok) }, lost: lease.Lost()}, true
}

// teardownPassTimeout bounds one converger run, so a step that hangs cannot
// hold the gate and the lease for ever; the reaper retries it. Var for tests.
var teardownPassTimeout = 15 * time.Minute

// holdTeardown claims the right to tear a cluster down on this node: the
// in-process gate, which also bounds this node's teardowns, then the cross-node
// lease. It refuses, without waiting, when either is held, or, if capped, when
// the node is at teardownConcurrency.
func (s *EKSServiceImpl) holdTeardown(accountID, name string, capped bool) (release func(retire bool), lost <-chan struct{}, ok bool) {
	key := teardownLeaseName(accountID, name)
	s.teardownMu.Lock()
	if s.teardownRunning == nil {
		s.teardownRunning = map[string]struct{}{}
	}
	_, busy := s.teardownRunning[key]
	if busy || (capped && len(s.teardownRunning) >= teardownConcurrency) {
		s.teardownMu.Unlock()
		return nil, nil, false
	}
	s.teardownRunning[key] = struct{}{}
	s.teardownMu.Unlock()
	leave := func() {
		s.teardownMu.Lock()
		delete(s.teardownRunning, key)
		s.teardownMu.Unlock()
	}

	// The lease renews on the service's context so it outlives any request and
	// stops with the daemon.
	lease, held := s.acquireTeardownLease(s.bgCtx, accountID, name)
	if !held {
		leave()
		return nil, nil, false
	}
	// Retiring drops the teardown lease before the gate opens: every Get of it
	// is under the gate, so no later teardown of a recreated name can hold it.
	return func(retire bool) {
		lease.release()
		if retire {
			s.leases.Forget(key)
		}
		leave()
	}, lease.lost, true
}

// startTeardown hands one DELETING cluster to a background converger and
// reports whether it did. When holdTeardown refuses, the record stays DELETING
// for the reaper.
func (s *EKSServiceImpl) startTeardown(accountID, name string) bool {
	release, lost, ok := s.holdTeardown(accountID, name, true)
	if !ok {
		return false
	}
	s.teardownWG.Go(func() {
		deleted := false
		defer func() { release(deleted) }()
		ctx, cancel := context.WithTimeout(s.bgCtx, teardownPassTimeout)
		defer cancel()
		started := time.Now()
		if err := s.convergeClusterDeletion(ctx, accountID, name, lost); err != nil {
			slog.Warn("eks: cluster teardown incomplete, the record stays DELETING for retry",
				"cluster", name, "account", accountID, "err", err)
			return
		}
		deleted = true
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
	acctKV, err := s.teardownBucket(ctx, accountID)
	if err != nil {
		return err
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
		// Waiting on node groups or on a running launch is not a failed pass, so
		// both are checked before the attempt is counted.
		pre, err := GetClusterMeta(ctx, acctKV, name)
		if err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return nil
			}
			return err
		}
		if pre.Status != ClusterStatusDeleting {
			return errTeardownNotRecorded
		}
		if err := s.refuseWhileNodegroups(ctx, acctKV, name); err != nil {
			return err
		}
		if err := s.waitForLaunch(ctx, acctKV, pre); err != nil {
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

		steps, terr := s.teardownClusterInfra(ctx, accountID, name, meta, acctKV, checkpoint)
		if terr != nil {
			if errors.Is(terr, errTeardownLeaseLost) || errors.Is(ctx.Err(), context.Canceled) {
				return terr
			}
			// A pass that ran out of time is a failed pass: record it so the
			// reaper backs off, on a context the deadline has not ended.
			s.recordTeardownFailure(context.WithoutCancel(ctx), acctKV, name, steps, terr)
			return terr
		}

		// Forgotten while the record still blocks a same-name create, and after
		// teardownClusterInfra stopped this node's reconciler.
		s.leases.Forget(clusterLeaseName(accountID, name))
		done, err := s.removeClusterRecord(ctx, acctKV, name, meta)
		if err != nil {
			if !errors.Is(err, errNodegroupsAttached) && !errors.Is(err, errTeardownNotRecorded) {
				s.recordTeardownFailure(context.WithoutCancel(ctx), acctKV, name, []string{stepRecord}, err)
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

// waitForLaunch keeps the record while a create launch for it is still running:
// removing it would let the launch find it gone and recreate it, so a caller
// would read the cluster as missing and then DELETING again. The launch hands
// its refs over at its next write and starts the teardown when it returns.
func (s *EKSServiceImpl) waitForLaunch(ctx context.Context, acctKV jetstream.KeyValue, meta *ClusterMeta) error {
	if !launchInFlight(meta, s.launchStaleAfter) {
		return nil
	}
	if meta.DeleteFailedStep != stepLaunch {
		s.recordTeardownFailure(ctx, acctKV, meta.Name, []string{stepLaunch}, errLaunchInFlight)
	}
	return errLaunchInFlight
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
	if meta, err := GetClusterMeta(ctx, acctKV, name); err == nil && meta.DeleteFailedStep != stepNodegroups {
		s.recordTeardownFailure(ctx, acctKV, name, []string{stepNodegroups}, errNodegroupsAttached)
	}
	return errNodegroupsAttached
}

// teardownBucket opens the account bucket the converger works in;
// s.teardownKV replaces it in tests that inject KV faults.
func (s *EKSServiceImpl) teardownBucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	if s.teardownKV != nil {
		return s.teardownKV(ctx, accountID)
	}
	return s.accountBucket(ctx, accountID)
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
	_, err = kvstore.DeleteIf(ctx, acctKV, ClusterMetaKey(name), kvstore.CASConfig{
		Attempts: maxClusterStateCASRetries,
		NotFound: ErrClusterNotFound,
	}, func(m *ClusterMeta) error {
		if m.Status != ClusterStatusDeleting {
			return errTeardownNotRecorded
		}
		if refsAddedSince(tornDown, m) || launchInFlight(m, s.launchStaleAfter) {
			return errRefsAdded
		}
		return nil
	})
	switch {
	case err == nil, errors.Is(err, ErrClusterNotFound):
		s.sweepLateChildren(ctx, acctKV, name)
		return true, nil
	case errors.Is(err, errRefsAdded):
		return false, nil
	default:
		return false, err
	}
}

// sweepLateChildren removes child keys written between the children sweep and
// the meta delete, by a launch or bootstrap still running for the cluster. A
// writer that lands after this finds the record gone and removes its own.
func (s *EKSServiceImpl) sweepLateChildren(ctx context.Context, acctKV jetstream.KeyValue, name string) {
	if _, err := GetClusterMeta(ctx, acctKV, name); !errors.Is(err, ErrClusterNotFound) {
		return
	}
	if err := DeleteClusterChildren(ctx, acctKV, name); err != nil {
		slog.WarnContext(ctx, "eks: remove late child records", "cluster", name, "err", err)
	}
}

func (s *EKSServiceImpl) recordTeardownFailure(ctx context.Context, acctKV jetstream.KeyValue, name string, steps []string, terr error) {
	if err := RecordDeleteReapFailure(ctx, acctKV, name, steps, terr); err != nil && !errors.Is(err, ErrClusterNotFound) {
		slog.WarnContext(ctx, "eks: record teardown failure", "cluster", name, "err", err)
	}
}
