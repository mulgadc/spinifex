package queue

import (
	"context"
	"fmt"
	"maps"
	"time"
)

// updateAttempts bounds how often an update retries after losing a
// compare-and-swap to another writer.
const updateAttempts = 5

// change describes one desired-state update: apply produces the record to
// write, or an error that rejects the whole request.
type change struct {
	op    string
	apply func(r *Record) (Record, error)
}

// SetAttributes applies attrs to the named queue all or nothing, advancing its
// generation on every accepted call, same values included (ADR-0012:S3).
func (s *Store) SetAttributes(ctx context.Context, account, region, name string, attrs map[string]string) (Record, error) {
	supplied, err := ValidateSet(attrs)
	if err != nil {
		return Record{}, err
	}
	return s.update(ctx, account, region, name, change{
		op: "SetQueueAttributes",
		apply: func(r *Record) (Record, error) {
			next := *r
			next.Attributes = maps.Clone(r.Attributes)
			maps.Copy(next.Attributes, supplied)
			next.AttributesModifiedAt = s.now().UTC().Truncate(time.Second)
			return next, nil
		},
	})
}

// update applies c to the named active queue by compare-and-swap, advancing
// its generation, and retries only a definite conflict. It is pinned to the
// queue it first resolves, so a replacement under the name reports absent.
func (s *Store) update(ctx context.Context, account, region, name string, c change) (Record, error) {
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
		next, err := c.apply(&r)
		if err != nil {
			return Record{}, err
		}
		next.Generation = r.Generation + 1
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
		// Another writer leaving the same values cannot prove this write
		// applied, so a lost reply is never settled by re-reading.
		return Record{}, fmt.Errorf("%w: %s %s outcome unknown: %w", ErrUnavailable, c.op, name, err)
	}
	return Record{}, fmt.Errorf("%w: %s %s lost %d races", ErrUnavailable, c.op, name, updateAttempts)
}

// resolvePinned resolves the name and, once pinned, refuses any queue other
// than the pinned one: the original was deleted and the name reused.
func (s *Store) resolvePinned(ctx context.Context, account, region, name, pinned string) (Record, uint64, error) {
	r, rev, err := s.resolve(ctx, account, region, name)
	if err != nil {
		return Record{}, 0, err
	}
	if pinned != "" && r.QueueID != pinned {
		return Record{}, 0, ErrQueueDoesNotExist
	}
	return r, rev, nil
}

// settable refuses a queue that is not active: a deleting queue is already
// absent to the tenant, and a state this release does not know is not its to
// change.
func settable(r *Record) error {
	switch r.State {
	case StateActive:
		if r.Attributes == nil {
			return fmt.Errorf("%w: queue %s has no attributes", ErrUnavailable, r.QueueID)
		}
		return nil
	case StateDeleting:
		return ErrQueueDoesNotExist
	}
	return fmt.Errorf("%w: queue %s in state %q", ErrUnavailable, r.QueueID, r.State)
}
