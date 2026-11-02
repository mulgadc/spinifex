package message

import (
	"context"
	"errors"
	"fmt"
)

// SweepResult counts one sweep pass.
type SweepResult struct {
	// Scanned is how many surviving bodies the pass read.
	Scanned int
	// Swept is how many tombstoned bodies it deleted with their records.
	Swept int
}

// Sweep makes one bounded pass over a message shard, deleting each body whose
// record terminal reports as a tombstone, then the record (ADR-0011 K5). Body
// first: a crash between leaves a bodiless record, which reads as gone.
func Sweep(ctx context.Context, s Store, messageShard Shard, terminal func(State) bool, pageSize int) (SweepResult, error) {
	var res SweepResult
	var from ScanCursor
	for {
		page, err := s.Scan(ctx, messageShard, from, pageSize)
		if err != nil {
			return res, fmt.Errorf("sweep shard %d: %w", messageShard, err)
		}
		for _, c := range page.Changes {
			res.Scanned++
			swept, err := sweepOne(ctx, s, c, terminal)
			if err != nil {
				return res, err
			}
			if swept {
				res.Swept++
			}
		}
		if page.Done {
			return res, nil
		}
		from = page.Next
	}
}

func sweepOne(ctx context.Context, s Store, c Change, terminal func(State) bool) (bool, error) {
	st, err := s.GetState(ctx, c.Namespace, c.Position)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("sweep %s/%d: read state: %w", c.Namespace.Queue, c.Position, err)
	case !terminal(st):
		return false, nil
	}
	if err := s.Delete(ctx, c.Namespace, c.Position); err != nil {
		return false, fmt.Errorf("sweep %s/%d: delete body: %w", c.Namespace.Queue, c.Position, err)
	}
	err = s.DeleteState(ctx, c.Namespace, c.Position, st.Revision)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		// Another sweeper finished this record first.
		return true, nil
	default:
		return false, fmt.Errorf("sweep %s/%d: delete state: %w", c.Namespace.Queue, c.Position, err)
	}
}
