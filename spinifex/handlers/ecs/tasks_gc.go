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
		pruned, due, serr := sc.svc.sweepStoppedBucket(ctx, kv, now, stoppedTaskRetention)
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

// sweepStoppedBucket deletes task records that have been STOPPED longer than
// retention. A task missing its StoppedAt timestamp is never pruned (defensive:
// it would otherwise look infinitely old). Returns the number deleted and when
// the soonest record it kept falls due.
func (s *Service) sweepStoppedBucket(ctx context.Context, kv jetstream.KeyValue, now time.Time, retention time.Duration) (int, time.Duration, error) {
	keys, err := keysWithPrefix(ctx, kv, "clusters/")
	if err != nil {
		return 0, 0, err
	}
	pruned := 0
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
