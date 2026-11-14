package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	instancecache "github.com/mulgadc/spinifex/spinifex/runtime/compute/cache"
	"github.com/nats-io/nats.go/jetstream"
)

// FeatureLevelStore returns the cluster feature-level record over the
// cluster-config bucket, which has no TTL. Reads go to the stream leader.
func (m *JetStreamManager) FeatureLevelStore(ctx context.Context) (*featurelevel.Store, error) {
	if m.clusterConfig == nil {
		return nil, errors.New("cluster config KV not initialized")
	}
	kv, err := m.clusterConfig.KV(ctx)
	if err != nil {
		return nil, err
	}
	return featurelevel.NewStore(featurelevel.LeaderKV(m.js, kv)), nil
}

// AttachFeatureLevel opens the buckets an operator command reads that a daemon
// already created, without creating, repairing or migrating either. The
// cluster-state bucket is optional: a missing one is reported as unavailable
// heartbeats rather than created.
func (m *JetStreamManager) AttachFeatureLevel(ctx context.Context) (*featurelevel.Store, error) {
	kv, err := m.js.KeyValue(ctx, ClusterConfigBucket)
	if err != nil {
		return nil, fmt.Errorf("cluster config bucket %s is unavailable: %w", ClusterConfigBucket, err)
	}
	if state, serr := m.js.KeyValue(ctx, ClusterStateBucket); serr == nil {
		m.clusterKV = state
	}
	return featurelevel.NewStore(featurelevel.LeaderKV(m.js, kv)), nil
}

// featureLevelMembers reads every configured node's heartbeat. A member is
// current when its heartbeat is within NodeStaleAfter. A node with no heartbeat
// (never written, or expired from the bucket) or an unreadable timestamp is not
// current; any other read failure is returned so the tick advances nothing.
func featureLevelMembers(ctx context.Context, read func(ctx context.Context, node string) (*Heartbeat, error), nodes []string, now time.Time) ([]featurelevel.Member, error) {
	members := make([]featurelevel.Member, 0, len(nodes))
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read heartbeat of %s: %w", node, err)
		}
		h, err := read(ctx, node)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			members = append(members, featurelevel.Member{Node: node})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read heartbeat of %s: %w", node, err)
		}
		written, perr := time.Parse(time.RFC3339, h.Timestamp)
		current := perr == nil && now.UTC().Sub(written.UTC()) <= instancecache.NodeStaleAfter
		members = append(members, featurelevel.Member{Node: node, Level: h.FeatureLevel, Current: current})
	}
	return members, nil
}

// advanceFeatureLevel raises the recorded level when every configured node has
// a current heartbeat reporting it. Any number of daemons may call it.
func advanceFeatureLevel(ctx context.Context, store *featurelevel.Store, read func(context.Context, string) (*Heartbeat, error), nodes []string, now time.Time) (featurelevel.Record, bool, error) {
	members, err := featureLevelMembers(ctx, read, nodes, now)
	if err != nil {
		return featurelevel.Record{}, false, err
	}
	return store.Advance(ctx, members)
}

// tickFeatureLevel runs on each heartbeat tick. A failure is logged and never
// fatal: the level stays where it is until a later tick succeeds.
func (d *Daemon) tickFeatureLevel() {
	if d.jsManager == nil || d.clusterConfig == nil {
		return
	}
	// Half the interval, so a slow tick never delays the next heartbeat.
	ctx, cancel := context.WithTimeout(d.ctx, heartbeatInterval/2)
	defer cancel()
	store, err := d.jsManager.FeatureLevelStore(ctx)
	if err != nil {
		slog.Warn("Feature level: store unavailable", "error", err)
		return
	}
	// A level raised above this build while it runs (a downgrade without a
	// rollback) ends the daemon within one tick.
	if err := refuseAboveBuild(ctx, store); err != nil {
		if errors.Is(err, errStartupPermanent) {
			slog.Error("Exiting: cluster feature level is above this build", "err", err)
			d.exitFunc(1)
			return
		}
		slog.Warn("Feature level: fence read failed", "error", err)
		return
	}
	nodes := slices.Sorted(maps.Keys(d.clusterConfig.Nodes))
	rec, advanced, err := advanceFeatureLevel(ctx, store, d.jsManager.ReadHeartbeatLeader, nodes, time.Now())
	if err != nil {
		slog.Warn("Feature level: advance failed", "error", err)
		return
	}
	if advanced {
		slog.Info("Cluster feature level advanced", "level", rec.Level, "build", featurelevel.Build)
	}
}

// FeatureLevelRefusedError is returned when the cluster records a feature level
// above what this build supports, so an older build cannot join and write what
// the newer one no longer reads.
type FeatureLevelRefusedError struct {
	Build    int
	Recorded int
}

func (e *FeatureLevelRefusedError) Error() string {
	return fmt.Sprintf("cluster feature level is %d but this build supports %d; "+
		"lower the cluster first with `spx admin cluster rollback --to %d`, then install this build",
		e.Recorded, e.Build, e.Build)
}

// Unwrap makes the refusal permanent: no retry clears it.
func (e *FeatureLevelRefusedError) Unwrap() error { return errStartupPermanent }

// featureLevelReadTimeout bounds one attempt at reading the record.
const featureLevelReadTimeout = 10 * time.Second

// featureLevelSleep is the backoff seam; tests replace it.
var featureLevelSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryFeatureLevel runs fn with the startup backoff (500ms to 10s) until it
// succeeds, fails permanently, or ctx is cancelled. A cluster outage of any
// length leaves the daemon retrying rather than abandoning bootstrap.
func retryFeatureLevel(ctx context.Context, fn func() error) error {
	delay := 500 * time.Millisecond
	start := time.Now()
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			if attempt > 1 {
				slog.Info("feature level initialized successfully", "attempts", attempt, "elapsed_ms", otelsetup.Millis(time.Since(start)))
			}
			return nil
		}
		if errors.Is(err, errStartupPermanent) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("feature level check cancelled after %d attempts: %w", attempt, err)
		}
		slog.Warn("Failed to init feature level", "error", err, "attempt", attempt, "elapsed_ms", otelsetup.Millis(time.Since(start)))
		if serr := featureLevelSleep(ctx, delay); serr != nil {
			return fmt.Errorf("feature level check cancelled after %d attempts: %w", attempt, err)
		}
		delay = min(delay*2, 10*time.Second)
	}
}

// refuseAboveBuild reads the record from the leader and refuses when it is above
// this build.
func refuseAboveBuild(ctx context.Context, store *featurelevel.Store) error {
	rctx, cancel := context.WithTimeout(ctx, featureLevelReadTimeout)
	defer cancel()
	rec, _, err := store.Read(rctx)
	if err != nil {
		return err
	}
	if rec.Level > featurelevel.Build {
		return &FeatureLevelRefusedError{Build: featurelevel.Build, Recorded: rec.Level}
	}
	return nil
}

// enforceFeatureLevel refuses when the recorded level is above this build. An
// absent record is level 0. A read failure retries until ctx is cancelled and
// neither starts nor refuses.
//
// Once the level passes, publish overwrites any predecessor's heartbeat with
// this build's own, so its capability cannot count for the build now running.
// The level is read again afterwards: one that rose in between refuses.
func enforceFeatureLevel(ctx context.Context, store *featurelevel.Store, publish func() error) error {
	return retryFeatureLevel(ctx, func() error {
		if err := refuseAboveBuild(ctx, store); err != nil {
			return err
		}
		if err := publish(); err != nil {
			return fmt.Errorf("publish heartbeat: %w", err)
		}
		return refuseAboveBuild(ctx, store)
	})
}

// checkFeatureLevel runs the startup refusal against the cluster record.
func (d *Daemon) checkFeatureLevel() error {
	store, err := d.jsManager.FeatureLevelStore(d.ctx)
	if err != nil {
		return err
	}
	if err := enforceFeatureLevel(d.ctx, store, func() error { return d.jsManager.WriteHeartbeat(d.buildHeartbeat()) }); err != nil {
		return err
	}
	d.featureGate = featurelevel.NewGate(store)
	return nil
}
