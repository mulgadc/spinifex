package handlers_ecs

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// sweepStoppedTasks prunes stale STOPPED task records across every ECS account
// bucket. Leader-only (scheduler is the single KV writer); runs on the sweep
// tick. Mirrors reap()'s bucket walk. Returns an error when the account
// enumeration could not be completed, so a pass that saw only part of the fleet
// is reported rather than passing for a clean sweep.
func (sc *Scheduler) sweepStoppedTasks(ctx context.Context) error {
	js, err := sc.nc.JetStream()
	if err != nil {
		return err
	}
	buckets, err := accountBuckets(ctx, sc.nc)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, bucket := range buckets {
		kv, err := js.KeyValue(bucket.name)
		if err != nil {
			slog.Error("ECS sweep: open bucket failed", "bucket", bucket.name, "err", err)
			continue
		}
		pruned, serr := sc.svc.sweepStoppedBucket(kv, now, stoppedTaskRetention)
		if serr != nil {
			slog.Error("ECS sweep: bucket failed", "bucket", bucket.name, "err", serr)
			continue
		}
		if pruned > 0 {
			slog.Info("ECS sweep: pruned stale STOPPED tasks", "bucket", bucket.name, "count", pruned)
		}
	}
	return nil
}

// sweepStoppedBucket deletes task records that have been STOPPED longer than
// retention. A task missing its StoppedAt timestamp is never pruned (defensive:
// it would otherwise look infinitely old). Returns the number deleted.
func (s *Service) sweepStoppedBucket(kv nats.KeyValue, now time.Time, retention time.Duration) (int, error) {
	keys, err := keysWithPrefix(kv, "clusters/")
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, k := range keys {
		if !strings.Contains(k, "/tasks/") {
			continue
		}
		var task TaskRecord
		found, gerr := getJSON(kv, k, &task)
		if gerr != nil || !found {
			continue
		}
		if task.LastStatus != TaskStatusStopped || task.StoppedAt.IsZero() {
			continue
		}
		if now.Sub(task.StoppedAt) <= retention {
			continue
		}
		if derr := kv.Delete(k); derr != nil {
			slog.Warn("ECS sweep: delete failed", "key", k, "err", derr)
			continue
		}
		pruned++
	}
	return pruned, nil
}
