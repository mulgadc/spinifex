package vpcpeering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// SweepResult counts what one sweep pass changed.
type SweepResult struct {
	Moved          int // expired or rejected requests, and interrupted moves, finished
	DiscardedStale int // pending records no request claim names
	KeysRemoved    int // index keys and claims with no live owner
	TagsRemoved    int // central tag entries of connections in neither bucket
}

// Sweep is the one pass that scans the live bucket. It moves expired requests
// and requests whose VPC is gone to the terminal bucket, and removes residue
// older than the claim grace, which keeps each VPC's index near its limit.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	keys, err := kvstore.Keys(ctx, s.live)
	if err != nil {
		return res, fmt.Errorf("list %s: %w", KVBucketPeeringConnections, err)
	}
	var rest []string
	for _, key := range keys {
		id, ok := strings.CutPrefix(key, "pcx.")
		if !ok {
			rest = append(rest, key)
			continue
		}
		if err := s.sweepRecord(ctx, id, &res); err != nil {
			return res, fmt.Errorf("sweep %s: %w", id, err)
		}
	}
	for _, key := range rest {
		if err := s.sweepKey(ctx, key, &res); err != nil {
			return res, fmt.Errorf("sweep %s: %w", key, err)
		}
	}
	if err := s.sweepTags(ctx, &res); err != nil {
		return res, fmt.Errorf("sweep central tags: %w", err)
	}
	return res, nil
}

func (s *Store) sweepRecord(ctx context.Context, id string, res *SweepResult) error {
	for range moveAttempts {
		rec, rev, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
		if err != nil || rec == nil {
			return err
		}
		now := s.now()
		switch {
		case rec.isTerminalStatus():
			err = s.finishMove(ctx, rec, rev)
		case rec.Status != StatePendingAcceptance:
			return nil
		default:
			v, verr := s.liveView(ctx, rec, rev)
			if verr != nil {
				return verr
			}
			if v == nil {
				if now.Sub(rec.CreatedAt) < claimGrace {
					return nil
				}
				if err = deleteAt(ctx, s.live, recordKey(id), rev); err == nil {
					err = s.deleteIndexes(ctx, rec)
					res.DiscardedStale++
				}
				break
			}
			if !v.isTerminal() {
				return nil
			}
			ended := v.ended()
			rec = &ended
			err = s.moveTerminal(ctx, rec, rev)
		}
		if errors.Is(err, errConflict) {
			continue
		}
		if err == nil && rec.isTerminalStatus() {
			res.Moved++
		}
		return err
	}
	return errConflict
}

// sweepKey removes an index key or claim older than the grace whose record is
// no longer live, or is a pending record no request claim names.
func (s *Store) sweepKey(ctx context.Context, key string, res *SweepResult) error {
	kind, _, _ := strings.Cut(key, ".")
	var id string
	switch kind {
	case "acct", "vpc":
		entry, rev, err := getJSON[indexEntry](ctx, s.live, key)
		if err != nil || entry == nil || s.now().Sub(entry.CreatedAt) < claimGrace {
			return err
		}
		id = idFromIndexKey(key)
		return s.removeOrphan(ctx, key, id, rev, res)
	case "req", "pair", "link":
		c, rev, err := getJSON[claim](ctx, s.live, key)
		if err != nil || c == nil || s.now().Sub(c.ClaimedAt) < claimGrace {
			return err
		}
		return s.removeOrphan(ctx, key, c.PcxID, rev, res)
	}
	return nil
}

func (s *Store) removeOrphan(ctx context.Context, key, id string, rev uint64, res *SweepResult) error {
	rec, _, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
	if err != nil {
		return err
	}
	if rec != nil && !rec.isTerminalStatus() {
		if rec.Status != StatePendingAcceptance {
			return nil
		}
		valid, err := s.reqClaimNames(ctx, rec)
		if err != nil || valid {
			return err
		}
	}
	if err := deleteAt(ctx, s.live, key, rev); err != nil {
		if errors.Is(err, errConflict) {
			return nil
		}
		return err
	}
	res.KeysRemoved++
	slog.DebugContext(ctx, "VPC peering sweep removed an orphaned key", "key", key)
	return nil
}

// SweepInterval is how often the cluster leader scans the live bucket.
const SweepInterval = time.Hour

// SweepReaper runs Sweep for the GC backstop at most once per SweepInterval.
// A failed pass is retried on the next tick.
type SweepReaper struct {
	store *Store

	mu   sync.Mutex
	last time.Time
}

// NewSweepReaper returns the expiry sweep for the cluster-wide GC.
func (s *Store) NewSweepReaper() *SweepReaper { return &SweepReaper{store: s} }

func (r *SweepReaper) Class() string { return "vpc-peering-sweep" }

func (r *SweepReaper) Sweep(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.store.now()
	if !r.last.IsZero() && now.Sub(r.last) < SweepInterval {
		return 0, nil
	}
	res, err := r.store.Sweep(ctx)
	n := res.Moved + res.DiscardedStale + res.KeysRemoved + res.TagsRemoved
	if err != nil {
		return n, err
	}
	r.last = now
	if n > 0 {
		slog.InfoContext(ctx, "VPC peering sweep", "moved", res.Moved, "discarded_stale", res.DiscardedStale, "keys_removed", res.KeysRemoved, "tags_removed", res.TagsRemoved)
	}
	return n, nil
}
