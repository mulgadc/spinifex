package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	loop "github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DriftInterval is the resync backstop between drift passes when nothing
// changes. Var (not const) so integration tests can shrink it.
var DriftInterval = 5 * time.Minute

// driftBackoffBase is the requeue gap after the first incomplete pass, and the
// floor on how often a change may drive a pass. Var so tests can shrink it.
var driftBackoffBase = 5 * time.Second

// driftBackoffFactor grows the requeue gap per consecutive incomplete pass, up
// to DriftInterval. Without a requeue the repair latency for any transient apply
// failure is a full DriftInterval, so a 57-second DHCP stall costs five minutes
// of a VPC having no external gateway.
const driftBackoffFactor = 3

// intentBuckets names every KV bucket LoadIntentFromKV reads. None of their
// writers is periodic, so watching them cannot self-trigger a loop. The one
// write a pass makes is confirming an IGW attachment, which fires only on the
// pending transition and so costs one extra pass per attach, not a cycle.
var intentBuckets = []string{
	handlers_ec2_vpc.KVBucketVPCs,
	handlers_ec2_vpc.KVBucketSubnets,
	handlers_ec2_vpc.KVBucketSecurityGroups,
	handlers_ec2_vpc.KVBucketENIs,
	handlers_ec2_igw.KVBucketIGW,
	handlers_ec2_eip.KVBucketEIPs,
	handlers_ec2_routetable.KVBucketRouteTables,
	handlers_ec2_natgw.KVBucketNatGateways,
}

// intentSource watches the intent buckets. Enumerated rather than fixed so a
// bucket that does not exist yet is left alone: the EC2 handlers own these and
// set their history depth, and an opening watch would create one at the wrong
// depth. A bucket that appears later is picked up on the next resync.
func intentSource(js jetstream.JetStream) loop.Source {
	return loop.Dynamic(func(ctx context.Context) ([]*kvstore.Bucket, error) {
		buckets := make([]*kvstore.Bucket, 0, len(intentBuckets))
		for _, name := range intentBuckets {
			kv, err := js.KeyValue(ctx, name)
			switch {
			case errors.Is(err, jetstream.ErrBucketNotFound):
				// Nothing of this kind has ever been created here.
				slog.DebugContext(ctx, "reconcile/drift: intent bucket absent, not watched",
					"bucket", name)
				continue
			case err != nil:
				// Reported rather than skipped: a blip is not evidence the bucket
				// went away, and a partial list would have the caller tear down
				// the watchers it left out.
				return nil, fmt.Errorf("resolve intent bucket %s: %w", name, err)
			}
			// js is passed so a watcher whose stream was lost can reconnect;
			// RecreateIfMissing stays false so that reconnect never creates.
			buckets = append(buckets, kvstore.NewOpenBucket(js, kv, kvstore.Config{Name: name}))
		}
		return buckets, nil
	}, ">")
}

// DriftLoop reconciles on every intent change and every DriftInterval, gated on
// AcquireLeader so only one vpcd scans at a time. An incomplete pass asks to be
// revisited on a short backoff instead. Returns when ctx is cancelled.
//
// startup is the outcome of the bootstrap ReconcileApplyOnly pass, or nil when
// this node did not run one. It seeds the first deadline rather than triggering
// a pass: that bootstrap pass is the startup scan, and running a second one here
// would duplicate it and pull a scan forward on every clean boot.
func DriftLoop(ctx context.Context, rec Reconciler, nc *nats.Conn, localAZ, holder string, startup error) {
	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("reconcile/drift: JetStream context unavailable, drift loop disabled", "err", err)
		return
	}

	loop.Run(ctx, loop.Config{
		Name:      "vpcd/drift",
		Sources:   []loop.Source{intentSource(js)},
		Reconcile: driftPass(rec, nc, js, localAZ, holder, startup),
		Resync:    DriftInterval,
	})
}

// driftPass returns the reconcile pass, closing over the backoff ladder and the
// last pass time. Run never calls it concurrently with itself, which is why
// neither needs a lock.
func driftPass(rec Reconciler, nc *nats.Conn, js jetstream.JetStream, localAZ, holder string, startup error) func(context.Context) (time.Duration, error) {
	var backoff time.Duration
	var lastPass time.Time
	seeded := false

	return func(ctx context.Context) (time.Duration, error) {
		if !seeded {
			seeded = true
			backoff = nextDriftBackoff(0, startup)
			return backoff, nil
		}
		// A change-driven pass is a repair check, not the apply path: the event
		// subscriber has already applied the change, so coalescing a burst behind
		// the floor the ladder starts at costs a scan nothing it would have
		// found. Without it a launch storm scans continuously.
		if wait := time.Until(lastPass.Add(driftBackoffBase)); wait > 0 {
			return wait, nil
		}
		previous := backoff
		backoff = nextDriftBackoff(backoff, runDriftCycle(ctx, rec, nc, js, localAZ, holder))
		// Stamped after the scan, not before: the floor is a gap between passes,
		// and a pass slower than the floor would otherwise leave none at all.
		lastPass = time.Now()
		if backoff != previous {
			slog.Info("reconcile/drift: requeue interval changed", "backoff_ms", otelsetup.Millis(backoff))
		}
		// The outcome is logged by the cycle, and an unconverged pass is a change
		// of schedule rather than a failure of the loop.
		return backoff, nil
	}
}

// nextDriftBackoff returns the requeue backoff after a pass: zero once a pass
// converges, otherwise the previous backoff tripled from driftBackoffBase and
// held at DriftInterval. Zero means no deadline, leaving a converged loop to the
// resync rather than asking for a revisit it does not need.
func nextDriftBackoff(current time.Duration, err error) time.Duration {
	if !errors.Is(err, ErrPassIncomplete) {
		return 0
	}
	next := driftBackoffBase
	if current > 0 {
		next = current * driftBackoffFactor
	}
	return min(next, DriftInterval)
}

// runDriftCycle is one pass body, split out so tests can drive it directly.
// Returns the reconcile outcome so the caller can size the next wait; a cycle
// this node did not win the election for returns nil.
func runDriftCycle(ctx context.Context, rec Reconciler, nc *nats.Conn, js jetstream.JetStream, localAZ, holder string) error {
	release, elected := AcquireLeader(ctx, nc, KVBucketVPCDReconcile, holder)
	if !elected {
		return nil
	}
	defer release()

	intent, err := LoadIntentFromKV(ctx, js, localAZ)
	if err != nil {
		slog.Warn("reconcile/drift: load intent failed", "err", err)
		return err
	}
	if err := rec.Reconcile(ctx, intent); err != nil {
		slog.Warn("reconcile/drift: reconcile failed", "err", err)
		return err
	}
	return nil
}
