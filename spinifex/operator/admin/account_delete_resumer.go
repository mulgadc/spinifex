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

// accountDeletionResumeInterval is how often each gateway looks for deletion
// jobs whose holder died. A dead holder is found within one interval of its
// lease lapsing, so recovery lands between five and six minutes after the crash.
// A scan is one key listing and a read per unfinished job, which is cheap enough
// to leave well below the lease; the jitter keeps gateways from scanning in step.
const accountDeletionResumeInterval = 60 * time.Second

// StartDeletionResumer runs the deletion resumer until ctx is cancelled or the
// returned stop is called, and stop waits for the scan loop to exit. A teardown
// it resumed keeps running on its own context, exactly as one started by a
// request does.
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

// resumeDeletions takes over every deletion job that carries an accepted
// operation, is still RUNNING and has no live lease, and returns how many it
// resumed. Resuming completes an account-level decision already recorded
// (ADR-0003:S7): a record without an operation id and a fence was never accepted
// by the coordinator, and a live lease has a holder that is still working, so
// neither is touched. Failed jobs are not resumed here; they wait for a retry.
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
	engine, err := accountteardown.NewClusterEngine(ctx, d.NATS, accountteardown.ClusterNodes{Expected: d.ExpectedNodes, DaemonIDs: d.NodeIDs, GatewayIDs: d.GatewayNodeIDs}, d.Termination, d.BucketStore)
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
	}
	job, newRevision, _, err := takeOverDeletionJob(ctx, jobs, req, d.holderID(), existing, revision, takeoverReasonLeaseExpired, now)
	if err != nil {
		// Losing the race to another gateway is the expected outcome here.
		return false
	}

	d.runAccountDeletion(engine, jobs, job, newRevision)
	return true
}
