package queue

import (
	"context"
	"errors"
	"fmt"
)

// MarkDeleting records DeleteQueue's durable intent: one compare-and-swap of
// the pinned active queue to deleting with requestedAt and generation + 1.
// It removes nothing; reservation, purge and removal follow separately.
func (s *Store) MarkDeleting(ctx context.Context, account, region, name string) (Record, error) {
	var pinned string
	for range updateAttempts {
		r, rev, err := s.resolvePinned(ctx, account, region, name, pinned)
		if err != nil {
			return Record{}, err
		}
		pinned = r.QueueID
		if err := settable(&r); err != nil {
			return Record{}, err
		}

		next := r
		next.State = StateDeleting
		next.Generation = r.Generation + 1
		d := Deletion{}
		if r.Deletion != nil {
			d = *r.Deletion
		}
		d.RequestedAt = s.now().UTC()
		d.Generation = next.Generation
		next.Deletion = &d
		enc, err := next.Encode()
		if err != nil {
			return Record{}, err
		}
		_, err = s.kv.update(ctx, recordKey(account, r.QueueID), enc, rev)
		switch {
		case err == nil:
			return next, nil
		case isConflict(err):
			continue
		}

		done, retry, uerr := s.settleDelete(ctx, account, region, name, r.QueueID, rev, err)
		if done != nil {
			return *done, nil
		}
		if !retry {
			return Record{}, uerr
		}
	}
	return Record{}, fmt.Errorf("%w: DeleteQueue %s lost %d races", ErrUnavailable, name, updateAttempts)
}

// settleDelete decides a deletion whose reply was lost. A record now deleting
// satisfies the request, whichever writer moved it; an unmoved revision may
// still take the write, so it fails closed; any other move is retried.
func (s *Store) settleDelete(ctx context.Context, account, region, name, queueID string, rev uint64, cause error) (*Record, bool, error) {
	r, grev, err := s.resolvePinned(ctx, account, region, name, queueID)
	switch {
	case errors.Is(err, ErrQueueDoesNotExist):
		return nil, false, err
	case err != nil, grev == rev:
		return nil, false, fmt.Errorf("%w: DeleteQueue %s outcome unknown: %w", ErrUnavailable, name, cause)
	case r.State == StateDeleting:
		return &r, false, nil
	}
	return nil, true, nil
}
