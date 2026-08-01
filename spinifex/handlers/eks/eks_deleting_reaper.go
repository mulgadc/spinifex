package handlers_eks

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
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
	js, err := jetstream.New(r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}
	// A truncated bucket listing would silently leave a wedged teardown in place
	// for another sweep interval, so an incomplete enumeration fails the sweep.
	names, err := accountBucketNames(ctx, r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("enumerate account buckets: %w", err)
	}

	reaped := 0
	for _, name := range names {
		select {
		case <-ctx.Done():
			return reaped, ctx.Err()
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
			n, err := r.reapCluster(ctx, accountID, acctKV, cluster)
			if err != nil {
				slog.Warn("eks-deleting: re-drive teardown failed", "cluster", cluster, "err", err)
			}
			reaped += n
		}
	}
	return reaped, nil
}

// deleteReapBackoff returns how long the reaper must wait since the last
// re-drive attempt before trying again: minAge on the first attempt, doubling
// after each subsequent failure. priorAttempts is the re-drive count already
// made (meta.DeleteReapAttempts), so the 2nd attempt waits 2×minAge, the 3rd
// waits 4×minAge, and so on — a permanently-failing purge backs off instead of
// re-driving on every GC tick.
func deleteReapBackoff(minAge time.Duration, priorAttempts int) time.Duration {
	return minAge * time.Duration(1<<uint(priorAttempts))
}

// reapCluster re-drives one cluster's teardown if it is wedged in DELETING past
// minAge (and any subsequent backoff) and its leader lease can be acquired.
// Returns 1 when it completed a teardown, 0 otherwise. After maxDeleteReapAttempts
// consecutive failures it gives up permanently (DeleteReapExhausted) rather than
// re-driving a purge that keeps failing the same way forever.
func (r *EKSDeletingReaper) reapCluster(ctx context.Context, accountID string, acctKV jetstream.KeyValue, cluster string) (int, error) {
	meta, err := GetClusterMeta(ctx, acctKV, cluster)
	if err != nil {
		return 0, nil // gone or unreadable: nothing to re-drive
	}
	if meta.Status != ClusterStatusDeleting {
		return 0, nil
	}
	if meta.DeleteReapExhausted {
		return 0, nil // backstop already gave up; needs operator intervention
	}
	if !meta.DeletingSince.IsZero() && time.Since(meta.DeletingSince) < r.minAge {
		return 0, nil // still within the in-flight synchronous-delete window
	}
	// backoffRef is the last re-drive attempt once one has happened, so the
	// wait grows attempt-over-attempt instead of always measuring from
	// DeletingSince (which would let backoff collapse back to minAge forever).
	backoffRef := meta.DeletingSince
	if !meta.LastDeleteReapAttempt.IsZero() {
		backoffRef = meta.LastDeleteReapAttempt
	}
	if time.Since(backoffRef) < deleteReapBackoff(r.minAge, meta.DeleteReapAttempts) {
		return 0, nil // backing off after a recent failed re-drive
	}

	release, ok := r.svc.acquireTeardownLease(ctx, accountID, cluster)
	if !ok {
		return 0, nil // a synchronous delete or another node's reaper owns this teardown
	}
	defer release()

	attempts, err := RecordDeleteReapAttempt(ctx, acctKV, cluster)
	if err != nil {
		return 0, fmt.Errorf("record delete reap attempt: %w", err)
	}

	slog.Warn("eks-deleting: re-driving wedged DELETING teardown",
		"cluster", cluster, "account", accountID, "deletingSince", meta.DeletingSince, "attempt", attempts)
	if purgeErr := r.svc.purgeClusterInfra(context.Background(), accountID, cluster, meta, acctKV, true); purgeErr != nil {
		if attempts >= maxDeleteReapAttempts {
			slog.Error("eks-deleting: giving up on wedged DELETING teardown after repeated failures",
				"cluster", cluster, "account", accountID, "attempts", attempts, "err", purgeErr)
			if markErr := MarkDeleteReapExhausted(context.Background(), acctKV, cluster); markErr != nil {
				return 0, fmt.Errorf("mark delete reap exhausted: %w", markErr)
			}
		}
		return 0, purgeErr
	}
	slog.Info("eks-deleting: teardown completed, meta swept", "cluster", cluster, "account", accountID)
	return 1, nil
}
