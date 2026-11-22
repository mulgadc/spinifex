package vpcd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go/jetstream"
)

// levelWatch is vpcd's view of the cluster feature level. It refuses a level
// above this build, as the daemon does, and reports whether level 2 is active:
// whether security-group rules naming a prefix list may grant, and whether a
// proxied session may carry its endpoint's policy.
type levelWatch struct {
	read func(ctx context.Context) (featurelevel.Record, uint64, error)
	// withdraw, set before run, closes the sessions carrying a policy once the
	// grant is withdrawn, before the revision that withdrew it is reported.
	withdraw func()

	granted  atomic.Bool
	revision atomic.Uint64 // the record revision granted was last set from
	// changed receives, without blocking, each time the grant flips, so the
	// reconcile pass re-renders installed ACLs without waiting for a resync.
	changed chan struct{}
}

// LevelAboveBuildError is a recorded level this vpcd cannot serve; no retry
// clears it.
type LevelAboveBuildError struct{ Recorded int }

func (e *LevelAboveBuildError) Error() string {
	return fmt.Sprintf("cluster feature level is %d but this vpcd supports %d; roll the cluster back before installing this build",
		e.Recorded, featurelevel.Build)
}

// Granted reports whether level 2 is active. It is false until the first
// successful read.
func (w *levelWatch) Granted() bool { return w.granted.Load() }

// Revision is the record revision Granted reflects.
func (w *levelWatch) Revision() uint64 { return w.revision.Load() }

// observe reads the record once. A failed read keeps the last view, so an
// unreadable record never withdraws a grant nor restores one.
func (w *levelWatch) observe(ctx context.Context) error {
	rec, rev, err := w.read(ctx)
	if err != nil {
		return err
	}
	if rec.Level > featurelevel.Build {
		w.grant(false)
		return &LevelAboveBuildError{Recorded: rec.Level}
	}
	// Granted before the revision, so a status reporting this revision
	// already reflects it.
	w.grant(rec.Active(featurelevel.GatewayEndpoints))
	w.revision.Store(rev)
	return nil
}

func (w *levelWatch) grant(granted bool) {
	if w.granted.Swap(granted) == granted {
		return
	}
	if !granted && w.withdraw != nil {
		w.withdraw()
	}
	w.wake()
}

// wake asks for a reconcile pass without blocking.
func (w *levelWatch) wake() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// levelRetrySleep is the startup backoff seam; tests replace it.
var levelRetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start reads the record until one read succeeds, backing off from 500ms to
// 10s; a level above this build refuses at once. It runs before vpcd
// subscribes to security-group updates or starts its reconcile passes.
func (w *levelWatch) start(ctx context.Context) error {
	delay := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := w.observe(ctx)
		if err == nil {
			return nil
		}
		if _, above := errors.AsType[*LevelAboveBuildError](err); above {
			return err
		}
		slog.WarnContext(ctx, "vpcd: feature level unreadable; retrying", "attempt", attempt, "retry_ms", otelsetup.Millis(delay), "err", err)
		if serr := levelRetrySleep(ctx, delay); serr != nil {
			return fmt.Errorf("read feature level: %w", err)
		}
		delay = min(delay*2, 10*time.Second)
	}
}

// run re-reads the record on every change and on the resync. A level raised
// above this build while vpcd runs is handed to fatal, which stops vpcd.
func (w *levelWatch) run(ctx context.Context, js jetstream.JetStream, fatal func(error)) {
	reconciler.Run(ctx, reconciler.Config{
		Name:    "vpc-feature-level",
		Sources: []reconciler.Source{reconciler.Fixed(kvstore.NewBucket(js, kvstore.Config{Name: featureLevelBucket, AttachOnly: true}), featurelevel.Key)},
		Reconcile: func(ctx context.Context) (time.Duration, error) {
			err := w.observe(ctx)
			if _, above := errors.AsType[*LevelAboveBuildError](err); above {
				fatal(err)
				return 0, nil
			}
			return 0, err
		},
		Resync: reconciler.DefaultResync,
	})
}

// featureLevelRecord reads the record from the stream leader; a bucket or
// record never written is level 0 at revision 0.
func featureLevelRecord(js jetstream.JetStream) func(ctx context.Context) (featurelevel.Record, uint64, error) {
	level := kvstore.NewBucket(js, kvstore.Config{Name: featureLevelBucket, AttachOnly: true})
	return func(ctx context.Context) (featurelevel.Record, uint64, error) {
		kv, err := level.KV(ctx)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return featurelevel.Record{}, 0, nil
		}
		if err != nil {
			return featurelevel.Record{}, 0, fmt.Errorf("open %s: %w", featureLevelBucket, err)
		}
		return featurelevel.NewStore(featurelevel.LeaderKV(js, kv)).Read(ctx)
	}
}
