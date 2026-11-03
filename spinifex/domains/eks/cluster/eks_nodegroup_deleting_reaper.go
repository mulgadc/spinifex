package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// A DELETING nodegroup becomes DELETE_FAILED once it has been deleting this
// long and this many passes have failed. Vars for tests.
var (
	nodegroupDeleteFailAfter    = 30 * time.Minute
	nodegroupDeleteFailAttempts = 5
)

// A pass waits this long, polling, for ended workers to be proven gone.
const (
	defaultWorkersEndWait = 90 * time.Second
	defaultWorkersEndPoll = 3 * time.Second
)

// errWorkersEnding means workers were told to end but are not yet proven gone.
var errWorkersEnding = errors.New("workers still ending")

// EKSNodegroupDeletingReaper finishes recorded nodegroup deletes: it ends the
// workers, removes the nodegroup's own instance profile and only then the
// record, so a nodegroup is never gone while something it made still is.
type EKSNodegroupDeletingReaper struct {
	svc *EKSServiceImpl
}

var _ vm.Reaper = (*EKSNodegroupDeletingReaper)(nil)

// NewNodegroupDeletingReaper builds the DELETING nodegroup converger.
func (s *EKSServiceImpl) NewNodegroupDeletingReaper() *EKSNodegroupDeletingReaper {
	return &EKSNodegroupDeletingReaper{svc: s}
}

func (r *EKSNodegroupDeletingReaper) Class() string         { return "eks-nodegroup-deleting" }
func (r *EKSNodegroupDeletingReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep hands every due DELETING nodegroup to a background converger and
// returns how many it started. The lease decides which node runs each one.
func (r *EKSNodegroupDeletingReaper) Sweep(ctx context.Context) (int, error) {
	if !r.svc.depsReadyForOrchestration() {
		return 0, nil
	}
	js, err := jetstream.New(r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}
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
			recs, err := ListNodegroupRecords(ctx, acctKV, cluster)
			if err != nil {
				continue
			}
			for _, rec := range recs {
				if r.due(rec) && r.svc.startNodegroupDelete(accountID, acctKV, cluster, rec.Name) {
					started++
				}
			}
		}
	}
	return started, nil
}

// due reports whether rec is DELETING and past its retry backoff.
func (r *EKSNodegroupDeletingReaper) due(rec *NodegroupRecord) bool {
	if rec.Status != eks.NodegroupStatusDeleting {
		return false
	}
	if rec.DeleteReapAttempts == 0 || rec.LastDeleteReapAttempt.IsZero() {
		return true
	}
	return time.Since(rec.LastDeleteReapAttempt) >= deleteReapBackoff(deletingReapMinAge, rec.DeleteReapAttempts)
}

// startNodegroupDelete runs one converger pass in the background and reports
// whether it started; a pass already running here or on another node refuses.
func (s *EKSServiceImpl) startNodegroupDelete(accountID string, kv jetstream.KeyValue, cluster, ng string) bool {
	release, _, ok := s.holdTeardown(accountID, cluster+"/nodegroup/"+ng, false)
	if !ok {
		return false
	}
	s.teardownWG.Go(func() {
		defer release()
		ctx, cancel := context.WithTimeout(s.bgCtx, teardownPassTimeout)
		defer cancel()
		if err := s.convergeNodegroupDeletion(ctx, accountID, kv, cluster, ng); err != nil {
			slog.Warn("eks: nodegroup delete incomplete, the record stays DELETING for retry",
				"cluster", cluster, "nodegroup", ng, "account", accountID, "err", err)
		}
	})
	return true
}

// WaitNodegroupDeletes blocks until every converger this process started has
// returned. Intended for tests.
func (s *EKSServiceImpl) WaitNodegroupDeletes() { s.teardownWG.Wait() }

// mutateNodegroup applies fn to the stored record under compare-and-swap. fn
// returns false to leave the record unchanged.
func (s *EKSServiceImpl) mutateNodegroup(ctx context.Context, kv jetstream.KeyValue, cluster, ng string, fn func(*NodegroupRecord) bool) (*NodegroupRecord, error) {
	for range ngCASMaxRetries {
		rec, rev, err := getNodegroupEntry(ctx, kv, cluster, ng)
		if err != nil {
			return nil, err
		}
		if !fn(rec) {
			return rec, nil
		}
		ok, err := s.casPutNodegroup(ctx, kv, rec, rev)
		if err != nil {
			return nil, err
		}
		if ok {
			return rec, nil
		}
	}
	return nil, fmt.Errorf("nodegroup %s/%s: concurrent writers kept winning", cluster, ng)
}

// convergeNodegroupDeletion finishes one recorded delete. It acts only on a
// record that is DELETING when re-read, and keeps the record on any failure.
func (s *EKSServiceImpl) convergeNodegroupDeletion(ctx context.Context, accountID string, kv jetstream.KeyValue, cluster, ng string) error {
	rec, err := s.mutateNodegroup(ctx, kv, cluster, ng, func(r *NodegroupRecord) bool {
		if r.Status != eks.NodegroupStatusDeleting {
			return false
		}
		r.DeleteReapAttempts++
		r.LastDeleteReapAttempt = time.Now().UTC()
		return true
	})
	if err != nil {
		if errors.Is(err, ErrNodegroupNotFound) {
			return nil
		}
		return err
	}
	if rec.Status != eks.NodegroupStatusDeleting {
		return nil
	}

	if err := s.releaseNodegroupResources(ctx, accountID, kv, rec); err != nil {
		if errors.Is(err, errWorkersEnding) {
			s.noteWorkersEnding(ctx, kv, cluster, ng, err)
			return err
		}
		s.noteNodegroupDeleteFailure(ctx, kv, rec, err)
		return err
	}
	if err := DeleteNodegroupRecord(ctx, kv, cluster, ng); err != nil {
		return err
	}
	// A cluster delete waits on its node groups; the last one going lets it run.
	if meta, err := GetClusterMeta(ctx, kv, cluster); err == nil && meta.Status == ClusterStatusDeleting {
		if rest, err := ListNodegroupRecords(ctx, kv, cluster); err == nil && len(rest) == 0 {
			s.startTeardown(accountID, cluster)
		}
	}
	return nil
}

// releaseNodegroupResources ends the workers, then removes the profile. The
// profile and the recorded worker ids go only once every worker is proven
// terminated or absent, since an accepted terminate is only a request.
func (s *EKSServiceImpl) releaseNodegroupResources(ctx context.Context, accountID string, kv jetstream.KeyValue, rec *NodegroupRecord) error {
	if len(rec.InstanceIDs) > 0 {
		if err := s.deps.Worker.TerminateWorkerInstances(ctx, rec.InstanceIDs, accountID); err != nil {
			return fmt.Errorf("terminate workers: %w", err)
		}
		if err := s.awaitWorkersGone(ctx, rec.InstanceIDs, accountID); err != nil {
			return err
		}
		if _, err := s.mutateNodegroup(ctx, kv, rec.ClusterName, rec.Name, func(r *NodegroupRecord) bool {
			r.InstanceIDs = nil
			return true
		}); err != nil {
			return fmt.Errorf("record workers ended: %w", err)
		}
	}
	return s.deleteNodegroupProfile(ctx, accountID, kv, rec)
}

// awaitWorkersGone polls until no worker is live, up to the end wait,
// and returns errWorkersEnding if some still are.
func (s *EKSServiceImpl) awaitWorkersGone(ctx context.Context, ids []string, accountID string) error {
	wait, poll := s.workersEndWait, s.workersEndPoll
	if wait <= 0 {
		wait = defaultWorkersEndWait
	}
	if poll <= 0 {
		poll = defaultWorkersEndPoll
	}
	deadline := time.Now().Add(wait)
	for {
		live, err := s.deps.Worker.LiveWorkerInstances(ctx, ids, accountID)
		if err != nil {
			return fmt.Errorf("confirm workers ended: %w", err)
		}
		if len(live) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: %s", errWorkersEnding, strings.Join(live, ","))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// noteWorkersEnding handles workers that are not yet gone. Early on that is no
// failed attempt: the next sweep checks again without backoff. Once the group
// has been deleting past nodegroupDeleteFailAfter it settles DELETE_FAILED
// naming the instances, keeping the profile and record for a customer retry.
func (s *EKSServiceImpl) noteWorkersEnding(ctx context.Context, kv jetstream.KeyValue, cluster, ng string, cause error) {
	_, err := s.mutateNodegroup(ctx, kv, cluster, ng, func(r *NodegroupRecord) bool {
		if r.Status != eks.NodegroupStatusDeleting {
			return false
		}
		if time.Since(r.DeletingSince) >= nodegroupDeleteFailAfter {
			r.Status = eks.NodegroupStatusDeleteFailed
			r.StatusReason = "delete: " + cause.Error()
			r.ModifiedAt = time.Now().UTC()
			return true
		}
		r.DeleteReapAttempts--
		r.LastDeleteReapAttempt = time.Time{}
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "eks: record workers still ending", "cluster", cluster, "nodegroup", ng, "err", err)
	}
}

// noteNodegroupDeleteFailure records why a pass failed and, once the group has
// been deleting long enough and failed often enough, settles it DELETE_FAILED.
func (s *EKSServiceImpl) noteNodegroupDeleteFailure(ctx context.Context, kv jetstream.KeyValue, rec *NodegroupRecord, cause error) {
	_, err := s.mutateNodegroup(ctx, kv, rec.ClusterName, rec.Name, func(r *NodegroupRecord) bool {
		if r.Status != eks.NodegroupStatusDeleting {
			return false
		}
		r.StatusReason = "delete: " + cause.Error()
		r.ModifiedAt = time.Now().UTC()
		if r.DeleteReapAttempts >= nodegroupDeleteFailAttempts && time.Since(r.DeletingSince) >= nodegroupDeleteFailAfter {
			r.Status = eks.NodegroupStatusDeleteFailed
		}
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "eks: record nodegroup delete failure", "cluster", rec.ClusterName, "nodegroup", rec.Name, "err", err)
	}
}
