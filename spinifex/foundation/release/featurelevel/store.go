package featurelevel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Key is where the cluster record lives in the cluster-config bucket, which has
// no TTL.
const Key = "feature-level"

const casAttempts = 16

// Hold caps advance. Held distinguishes a hold at level 0 from no hold.
type Hold struct {
	Held  bool `json:"held"`
	Level int  `json:"level"`
}

// Rollback is a rollback to Target that has not lowered the level yet.
type Rollback struct {
	Target int `json:"target"`
	// PriorHold is the hold in force before the rollback, which a cancel restores.
	PriorHold Hold `json:"prior_hold"`
}

// Record is the one cluster record. A missing key reads as the zero Record.
type Record struct {
	Level    int       `json:"level"`
	Hold     Hold      `json:"hold"`
	Rollback *Rollback `json:"rollback,omitempty"`
}

// Member is one configured node's reported level. Current is false for a node
// with no heartbeat or a stale one, which blocks advance.
type Member struct {
	Node    string
	Level   int
	Current bool
}

// KV is the part of a JetStream key-value bucket the store uses.
type KV interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error)
	Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error)
}

// Store reads and compare-and-sets the cluster record.
type Store struct{ kv KV }

// NewStore returns a store over kv.
func NewStore(kv KV) *Store { return &Store{kv: kv} }

// Read returns the record and the revision it was read at (0 when absent).
func (s *Store) Read(ctx context.Context) (Record, uint64, error) {
	entry, err := s.kv.Get(ctx, Key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Record{}, 0, nil
	}
	if err != nil {
		return Record{}, 0, fmt.Errorf("read feature level: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return Record{}, 0, fmt.Errorf("decode feature level: %w", err)
	}
	return rec, entry.Revision(), nil
}

// mutate applies f to the record under a compare-and-set loop. f returns false
// to leave the record as read. It returns the record and the revision it holds.
func (s *Store) mutate(ctx context.Context, f func(*Record) (bool, error)) (Record, uint64, error) {
	var lastErr error
	for i := range casAttempts {
		rec, rev, err := s.Read(ctx)
		if err != nil {
			return Record{}, 0, err
		}
		changed, err := f(&rec)
		if err != nil {
			return Record{}, 0, err
		}
		if !changed {
			return rec, rev, nil
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return Record{}, 0, fmt.Errorf("encode feature level: %w", err)
		}
		if rev == 0 {
			rev, err = s.kv.Create(ctx, Key, data)
		} else {
			rev, err = s.kv.Update(ctx, Key, data, rev)
		}
		if err == nil {
			return rec, rev, nil
		}
		if !kvstore.IsRevisionConflict(err) {
			return Record{}, 0, fmt.Errorf("write feature level: %w", err)
		}
		lastErr = err
		backoff := time.Duration(i+1) * 2 * time.Millisecond
		select {
		case <-time.After(backoff/2 + rand.N(backoff)): //nolint:gosec // jitter
		case <-ctx.Done():
			return Record{}, 0, ctx.Err()
		}
	}
	return Record{}, 0, fmt.Errorf("feature level contended, CAS exhausted: %w", lastErr)
}

// candidate is the highest level every member supports, capped at Build, or -1
// when any member is not current or none are known.
func candidate(members []Member) int {
	if len(members) == 0 {
		return -1
	}
	level := Build
	for _, m := range members {
		if !m.Current {
			return -1
		}
		level = min(level, m.Level)
	}
	return level
}

// Advance raises the level to the highest one every member reports, respecting a
// hold and never lowering it. It writes only when that is above the recorded
// level, so any number of daemons may call it. A stale member, an unknown member
// set or a rollback in progress leaves the record alone.
func (s *Store) Advance(ctx context.Context, members []Member) (Record, bool, error) {
	want := candidate(members)
	advanced := false
	rec, _, err := s.mutate(ctx, func(r *Record) (bool, error) {
		advanced = false
		if r.Rollback != nil {
			return false, nil
		}
		to := want
		if r.Hold.Held {
			to = min(to, r.Hold.Level)
		}
		if to <= r.Level {
			return false, nil
		}
		r.Level = to
		advanced = true
		return true, nil
	})
	return rec, advanced, err
}

// StartRollback records the rollback to target and a hold at target in one
// write, and returns the revision to wait for. cliBuild is the caller's own
// Build: a recorded level above it refuses before anything is written. Repeating
// the same rollback resumes it.
func (s *Store) StartRollback(ctx context.Context, target, cliBuild int) (Record, uint64, error) {
	return s.mutate(ctx, func(r *Record) (bool, error) {
		if r.Level > cliBuild {
			return false, fmt.Errorf("%w: recorded %d, this build %d", ErrCLITooOld, r.Level, cliBuild)
		}
		if r.Rollback != nil {
			if r.Rollback.Target != target {
				return false, fmt.Errorf("%w: target %d", ErrRollbackInProgress, r.Rollback.Target)
			}
			return false, nil
		}
		if target < 0 || target >= r.Level {
			return false, fmt.Errorf("%w: target %d, level %d", ErrBadTarget, target, r.Level)
		}
		r.Rollback = &Rollback{Target: target, PriorHold: r.Hold}
		r.Hold = Hold{Held: true, Level: target}
		return true, nil
	})
}

// FinishRollback lowers the level to the target and clears the rollback, keeping
// the hold so running newer builds cannot advance it again.
func (s *Store) FinishRollback(ctx context.Context) (Record, error) {
	rec, _, err := s.mutate(ctx, func(r *Record) (bool, error) {
		if r.Rollback == nil {
			return false, ErrNoRollback
		}
		r.Level = r.Rollback.Target
		r.Rollback = nil
		return true, nil
	})
	return rec, err
}

// CancelRollback clears a rollback that has not lowered the level and restores
// the hold that existed before it started.
func (s *Store) CancelRollback(ctx context.Context) (Record, error) {
	rec, _, err := s.mutate(ctx, func(r *Record) (bool, error) {
		if r.Rollback == nil {
			return false, ErrNoRollback
		}
		r.Hold = r.Rollback.PriorHold
		r.Rollback = nil
		return true, nil
	})
	return rec, err
}

// ReleaseHold clears the hold once the operator has finished or abandoned the
// downgrade. It refuses while a rollback is in progress.
func (s *Store) ReleaseHold(ctx context.Context) (Record, error) {
	rec, _, err := s.mutate(ctx, func(r *Record) (bool, error) {
		if r.Rollback != nil {
			return false, ErrRollbackInProgress
		}
		if !r.Hold.Held {
			return false, nil
		}
		r.Hold = Hold{}
		return true, nil
	})
	return rec, err
}
