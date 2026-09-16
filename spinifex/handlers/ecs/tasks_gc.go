package handlers_ecs

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go/jetstream"
)

// sweepStoppedTasks prunes stale STOPPED task records across every ECS account
// bucket. Leader-only (scheduler is the single KV writer). Mirrors reap()'s
// bucket walk. Returns an error when the account enumeration could not be
// completed, so a pass that saw only part of the fleet is reported rather than
// passing for a clean sweep.
//
// The duration is when the soonest surviving record falls due. A record ageing
// out writes nothing, so nothing else would bring the sweep back for it.
func (sc *Scheduler) sweepStoppedTasks(ctx context.Context) (time.Duration, error) {
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return 0, err
	}
	buckets, err := accountBuckets(ctx, sc.nc)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var next time.Duration
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket.name)
		if err != nil {
			slog.Error("ECS sweep: open bucket failed", "bucket", bucket.name, "err", err)
			continue
		}
		pruned, due, serr := sc.svc.sweepStoppedBucket(ctx, kv, bucket.accountID, now, stoppedTaskRetention)
		if serr != nil {
			slog.Error("ECS sweep: bucket failed", "bucket", bucket.name, "err", serr)
			continue
		}
		next = reconciler.Earliest(next, due)
		if pruned > 0 {
			slog.Info("ECS sweep: pruned stale STOPPED tasks", "bucket", bucket.name, "count", pruned)
		}
	}
	return next, nil
}

// sweepStoppedBucket deletes STOPPED task records past retention and retries
// the ENI release for any that still owe one. A missing StoppedAt or an owed
// (unreleased) ENI both block deletion; the latter until the release succeeds.
func (s *Service) sweepStoppedBucket(ctx context.Context, kv jetstream.KeyValue, accountID string, now time.Time, retention time.Duration) (int, time.Duration, error) {
	keys, err := keysWithPrefix(ctx, kv, "clusters/")
	if err != nil {
		return 0, 0, err
	}
	pruned := 0
	retried := 0
	var next time.Duration
	for _, k := range keys {
		if !strings.Contains(k, "/tasks/") {
			continue
		}
		var task TaskRecord
		found, gerr := getJSON(ctx, kv, k, &task)
		if gerr != nil || !found {
			continue
		}
		if task.LastStatus != TaskStatusStopped || task.StoppedAt.IsZero() {
			continue
		}
		if task.ENIID != "" && !task.ENIReleased {
			due := !task.ENIReleaseNextTry.After(now)
			switch {
			case due && retried < eniReleaseRetriesPerPass:
				// Attempt the retry now; a failure recomputes ENIReleaseNextTry
				// itself, so only a leftover success needs no further deadline.
				retried++
				s.retryTaskENIRelease(ctx, kv, k, accountID, &task, now)
				if !task.ENIReleased && task.ENIReleaseNextTry.After(now) {
					next = reconciler.Earliest(next, task.ENIReleaseNextTry.Sub(now))
				}
			case due:
				// Due, but this pass's retry budget is spent: come back soon
				// rather than falling through to whatever else sets next.
				next = reconciler.Earliest(next, sweepInterval)
			default:
				next = reconciler.Earliest(next, task.ENIReleaseNextTry.Sub(now))
			}
			continue
		}
		if now.Sub(task.StoppedAt) <= retention {
			next = reconciler.Earliest(next, task.StoppedAt.Add(retention).Sub(now))
			continue
		}
		if derr := kv.Delete(ctx, k); derr != nil {
			slog.Warn("ECS sweep: delete failed", "key", k, "err", derr)
			continue
		}
		pruned++
	}
	return pruned, next, nil
}

// retryTaskENIRelease makes one more attempt at a STOPPED task's owed ENI
// release, then persists the outcome: success clears the identity and retry
// state, failure bumps the attempt count and backs off the next try.
func (s *Service) retryTaskENIRelease(ctx context.Context, kv jetstream.KeyValue, key, accountID string, task *TaskRecord, now time.Time) {
	s.reclaimTaskENI(ctx, accountID, task)
	if task.ENIReleased {
		task.ENIReleaseAttempts = 0
		task.ENIReleaseNextTry = time.Time{}
	} else {
		task.ENIReleaseAttempts++
		task.ENIReleaseNextTry = now.Add(eniReleaseBackoff(task.ENIReleaseAttempts))
	}
	if perr := putJSON(ctx, kv, key, task); perr != nil {
		slog.Warn("ECS sweep: persist ENI retry state failed", "key", key, "err", perr)
	}
}

// eniReleaseBackoff is the delay before the attempt after the attempts-th
// failure, doubling from eniReleaseBackoffBase and capped at
// eniReleaseBackoffMax.
func eniReleaseBackoff(attempts int) time.Duration {
	shift := min(max(attempts-1, 0), 10)
	return min(eniReleaseBackoffBase<<shift, eniReleaseBackoffMax)
}
