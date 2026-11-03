package admin

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/nats-io/nats.go/jetstream"
)

// accountDeletionResumeInterval is the scan period. Recovery lands within one
// interval of a dead holder's lease lapsing; a scan is one key listing, so it
// stays well below the lease.
const accountDeletionResumeInterval = 60 * time.Second

// StartDeletionResumer scans until ctx is cancelled or stop is called, and stop
// waits for the loop to exit. A resumed teardown runs on its own context, like
// one started by a request.
func (d Deps) StartDeletionResumer(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			timer := time.NewTimer(jittered(accountDeletionResumeInterval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			d.resumeDeletions(ctx, time.Now().UTC())
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func jittered(d time.Duration) time.Duration {
	return d - d/5 + time.Duration(rand.Int64N(int64(d/5)*2+1)) //nolint:gosec // jitter, not cryptographic
}

// resumeDeletions takes over RUNNING jobs that carry an accepted operation and
// have no live lease, which completes a decision already recorded (ADR-0003:S7).
// Records without an operation id and fence, and failed jobs, are left alone.
func (d Deps) resumeDeletions(ctx context.Context, now time.Time) int {
	if d.Termination == nil || d.NATS == nil {
		return 0
	}
	jobs, err := d.accountDeletionStore(ctx)
	if err != nil {
		return 0
	}
	keys, err := jobs.Keys(ctx)
	if err != nil {
		if !errors.Is(err, jetstream.ErrNoKeysFound) {
			slog.Error("DeletionResumer: failed to list deletion jobs", "err", err)
		}
		return 0
	}

	resumed := 0
	for _, accountID := range keys {
		if ctx.Err() != nil {
			break
		}
		existing, revision, err := readDeletionEntry(ctx, jobs, accountID)
		if err != nil || existing == nil || !resumable(existing, now) {
			continue
		}
		if d.resumeDeletion(ctx, jobs, existing, revision, now) {
			resumed++
		}
	}
	return resumed
}

// resumable is the whole of what lets the resumer act on a record.
func resumable(job *accountDeletionJob, now time.Time) bool {
	return job.State == operatorv1.DeletionStateRunning &&
		job.OperationID != "" &&
		job.Fence >= 1 &&
		deletionLeaseExpired(job, now)
}

// resumeDeletion takes the job over with the same fenced write a retried request
// uses, conditional on the revision it was judged resumable at, then runs the
// remainder on a fresh timeout budget.
func (d Deps) resumeDeletion(
	ctx context.Context,
	jobs jetstream.KeyValue,
	existing *accountDeletionJob,
	revision uint64,
	now time.Time,
) bool {
	engine, err := accountteardown.NewClusterEngine(ctx, d.NATS, accountteardown.ClusterNodes{Expected: d.ExpectedNodes, DaemonIDs: d.NodeIDs, GatewayIDs: d.GatewayNodeIDs}, d.Termination, d.BucketStore, d.PlatformCleaner)
	if err != nil {
		// Nothing was written, so a later scan retries.
		slog.Error("DeletionResumer: failed to build teardown engine", "account_id", existing.AccountID, "operation_id", existing.OperationID, "err", err)
		return false
	}
	req := &operatorv1.DeleteAccountRequest{
		AccountID:   existing.AccountID,
		AccountName: existing.AccountName,
		ClientToken: existing.ClientToken,
		Force:       existing.Force,

		NonFinalizing: existing.NonFinalizing,
	}
	job, newRevision, _, err := takeOverDeletionJob(ctx, jobs, req, d.holderID(), existing, revision, takeoverReasonLeaseExpired, now)
	if err != nil {
		// Losing the race to another gateway is the expected outcome here.
		return false
	}

	d.runAccountDeletion(engine, jobs, job, newRevision)
	return true
}
