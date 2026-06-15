package handlers_eks

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// deletingReapMinAge is how long a cluster must sit in DELETING before the
// backstop reaper re-drives its teardown. It must exceed a healthy synchronous
// DeleteCluster so the reaper never races an in-flight delete; only a wedged
// teardown (failed first attempt with no client retry) is re-driven. Var for tests.
var deletingReapMinAge = 90 * time.Second

// EKSDeletingReaper is the ADR-0004 tracked-async-teardown backstop. A
// DeleteCluster whose synchronous teardown fails leaves the cluster in DELETING
// with its billable infra (NAT-GW EIP, NLB, CP VPC) still allocated and nothing
// to re-drive it: the per-cluster reconciler exits on DELETING and the billable
// reaper only acts once the cluster meta is GONE. This reaper finds clusters
// stuck in DELETING past deletingReapMinAge and re-runs purgeClusterInfra to
// completion, idempotently, under the per-cluster reconciler leader lease so only
// one node drives each teardown.
type EKSDeletingReaper struct {
	svc    *EKSServiceImpl
	minAge time.Duration
}

var _ vm.Reaper = (*EKSDeletingReaper)(nil)

// NewDeletingReaper builds the EKS wedged-DELETING teardown backstop.
func (s *EKSServiceImpl) NewDeletingReaper() *EKSDeletingReaper {
	return &EKSDeletingReaper{svc: s, minAge: deletingReapMinAge}
}

func (r *EKSDeletingReaper) Class() string         { return "eks-deleting" }
func (r *EKSDeletingReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep re-drives every cluster stuck in DELETING past minAge. Idempotent:
// purgeClusterInfra tolerates already-gone resources, so a clean prior delete is
// a no-op and the meta is swept on success.
func (r *EKSDeletingReaper) Sweep(ctx context.Context) (int, error) {
	if !r.svc.depsReadyForOrchestration() {
		return 0, nil
	}
	js, err := r.svc.deps.NATSConn.JetStream()
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}

	reaped := 0
	for name := range js.KeyValueStoreNames() {
		if !strings.HasPrefix(name, KVBucketEKSAccountPrefix) {
			continue
		}
		select {
		case <-ctx.Done():
			return reaped, ctx.Err()
		default:
		}
		accountID := strings.TrimPrefix(name, KVBucketEKSAccountPrefix)
		acctKV, err := js.KeyValue(name)
		if err != nil {
			continue
		}
		clusters, err := listClusterNames(acctKV)
		if err != nil {
			continue
		}
		for _, cluster := range clusters {
			n, err := r.reapCluster(accountID, acctKV, cluster)
			if err != nil {
				slog.Warn("eks-deleting: re-drive teardown failed", "cluster", cluster, "err", err)
			}
			reaped += n
		}
	}
	return reaped, nil
}

// reapCluster re-drives one cluster's teardown if it is wedged in DELETING past
// minAge and its leader lease can be acquired. Returns 1 when it completed a
// teardown, 0 otherwise.
func (r *EKSDeletingReaper) reapCluster(accountID string, acctKV nats.KeyValue, cluster string) (int, error) {
	meta, err := GetClusterMeta(acctKV, cluster)
	if err != nil {
		return 0, nil // gone or unreadable: nothing to re-drive
	}
	if meta.Status != ClusterStatusDeleting {
		return 0, nil
	}
	if !meta.DeletingSince.IsZero() && time.Since(meta.DeletingSince) < r.minAge {
		return 0, nil // still within the in-flight synchronous-delete window
	}

	release, ok := r.svc.acquireTeardownLease(accountID, cluster)
	if !ok {
		return 0, nil // a synchronous delete or another node's reaper owns this teardown
	}
	defer release()

	slog.Warn("eks-deleting: re-driving wedged DELETING teardown",
		"cluster", cluster, "account", accountID, "deletingSince", meta.DeletingSince)
	if err := r.svc.purgeClusterInfra(accountID, cluster, meta, acctKV, true); err != nil {
		return 0, err
	}
	slog.Info("eks-deleting: teardown completed, meta swept", "cluster", cluster, "account", accountID)
	return 1, nil
}
