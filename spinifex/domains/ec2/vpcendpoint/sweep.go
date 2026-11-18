package vpcendpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// SweepResult counts what one sweep pass removed.
type SweepResult struct {
	Records int // pending records past the grace, and deleting records
	Keys    int // claims with no effective owner
}

// Sweep is the one pass that scans the bucket. It removes what an interrupted
// request left: records that never became available or were part way through
// a delete, and stale claims. A key it cannot decide is logged and skipped.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	keys, err := kvstore.Keys(ctx, s.kv)
	if err != nil {
		return res, fmt.Errorf("list %s: %w", KVBucketVpcEndpoints, err)
	}
	var errs []error
	for _, key := range keys {
		if key == kvstore.VersionKey {
			continue
		}
		if table, ok := rtbFromClaimKey(key); ok {
			err = s.sweepClaim(ctx, key, table, &res)
		} else {
			err = s.sweepRecord(ctx, key, &res)
		}
		if err != nil {
			slog.WarnContext(ctx, "VPC endpoint sweep skipped a key", "key", key, "err", err)
			errs = append(errs, fmt.Errorf("sweep %s: %w", key, err))
		}
	}
	return res, errors.Join(errs...)
}

// sweepRecord finishes a delete stopped after its fence, and removes a
// pending record past the grace with the claims it took.
func (s *Store) sweepRecord(ctx context.Context, key string, res *SweepResult) error {
	rec, rev, err := getJSON[EndpointRecord](ctx, s.kv, key)
	if err != nil || rec == nil || rec.ID == "" {
		return err
	}
	switch {
	case rec.State == StateDeleting:
		err = s.finishDelete(ctx, rec, rev)
	case rec.State == StatePending && s.now().Sub(rec.CreatedAt) >= claimGrace:
		if err = deleteAt(ctx, s.kv, key, rev); err == nil {
			err = s.releaseClaims(ctx, rec.RouteTableIds, rec.ID)
		}
	default:
		return nil
	}
	if errors.Is(err, errConflict) {
		return nil
	}
	if err == nil {
		res.Records++
	}
	return err
}

// sweepClaim removes a claim past the grace that names no effective
// association, fencing its holder first as a taker would.
func (s *Store) sweepClaim(ctx context.Context, key, table string, res *SweepResult) error {
	c, rev, err := getJSON[claim](ctx, s.kv, key)
	if err != nil || c == nil || s.now().Sub(c.ClaimedAt) < claimGrace {
		return err
	}
	holder, holderRev, err := s.load(ctx, c.AccountID, c.VpceID)
	if err != nil {
		return err
	}
	switch {
	case holder == nil || holder.State == StateDeleting:
	case holder.State == StateAvailable && holder.lists(table):
		vpc, err := s.tableVpc(ctx, holder.AccountID, table)
		if err != nil || vpc != "" {
			return err
		}
	default:
		if err := s.fence(ctx, holder, holderRev); err != nil {
			if errors.Is(err, errConflict) {
				return nil
			}
			return err
		}
	}
	return s.removeKey(ctx, key, rev, res)
}

func (s *Store) removeKey(ctx context.Context, key string, rev uint64, res *SweepResult) error {
	if err := deleteAt(ctx, s.kv, key, rev); err != nil {
		if errors.Is(err, errConflict) {
			return nil
		}
		return err
	}
	res.Keys++
	slog.DebugContext(ctx, "VPC endpoint sweep removed an orphaned key", "key", key)
	return nil
}

// SweepInterval is how often the cluster leader scans the bucket.
const SweepInterval = 10 * time.Minute

// SweepReaper runs Sweep for the GC backstop at most once per SweepInterval.
// A failed pass is retried on the next tick.
type SweepReaper struct {
	store *Store

	mu   sync.Mutex
	last time.Time
}

// NewSweepReaper returns the residue sweep for the cluster-wide GC.
func (s *Store) NewSweepReaper() *SweepReaper { return &SweepReaper{store: s} }

func (r *SweepReaper) Class() string { return "vpc-endpoint-sweep" }

func (r *SweepReaper) Sweep(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.store.now()
	if !r.last.IsZero() && now.Sub(r.last) < SweepInterval {
		return 0, nil
	}
	res, err := r.store.Sweep(ctx)
	n := res.Records + res.Keys
	if err != nil {
		return n, err
	}
	r.last = now
	if n > 0 {
		slog.InfoContext(ctx, "VPC endpoint sweep", "records", res.Records, "keys", res.Keys)
	}
	return n, nil
}
