package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// ReservationPeriod is how long a deleted queue's name stays reserved after
// its deletion was requested (lifecycle 4.6 step 2).
const ReservationPeriod = 60 * time.Second

// ownedBy refuses a record whose identity does not match the account it was
// read under or cannot name a binding, so it never selects another's key.
func ownedBy(r *Record, account string) error {
	if r.Account != account || validIdentity(r.Account, r.Region) != nil || ValidateName(r.Name) != nil {
		return fmt.Errorf("%w: queue %s record identity is inconsistent", ErrUnavailable, r.QueueID)
	}
	return nil
}

// reservedUntil is when a reserved name may be reused: the later of the
// binding's deadline and its deleting queue's intent, if the record remains.
// A remaining record that is inconsistent or not deleting fails closed.
func (s *Store) reservedUntil(ctx context.Context, account, region, name string, b Binding) (time.Time, error) {
	r, _, err := s.readRecord(ctx, account, b.QueueID)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return b.ReuseAfter, nil
	case err != nil:
		return time.Time{}, err
	case !r.names(account, region, name):
		return time.Time{}, fmt.Errorf("%w: reservation %s names queue %s of another name", ErrUnavailable, name, b.QueueID)
	}
	until, err := r.reuseAfter()
	if err != nil {
		return time.Time{}, err
	}
	if b.ReuseAfter.After(until) {
		return b.ReuseAfter, nil
	}
	return until, nil
}

// errReusable reports a name bound to a deleting queue whose reservation has
// lapsed, so CreateQueue may rebind it.
var errReusable = errors.New("name reusable")

// reuseAfter derives when a deleting queue's name may be reused from its
// durable deletion intent, refusing an intent it cannot trust.
func (r *Record) reuseAfter() (time.Time, error) {
	d := r.Deletion
	if r.State != StateDeleting || d == nil || d.RequestedAt.IsZero() || d.Generation < 1 || d.Generation > r.Generation {
		return time.Time{}, fmt.Errorf("%w: queue %s has no valid deletion intent", ErrUnavailable, r.QueueID)
	}
	return d.RequestedAt.Add(ReservationPeriod), nil
}

// Reserve reserves deleting queue queueID's name until its intent's deadline
// by compare-and-swap of a binding still naming it, extending an earlier one.
// Other bindings, or none, are left alone; the record is never written.
func (s *Store) Reserve(ctx context.Context, account, queueID string) error {
	r, _, err := s.readRecord(ctx, account, queueID)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return ErrQueueDoesNotExist
	case err != nil:
		return err
	}
	if err := ownedBy(&r, account); err != nil {
		return err
	}
	until, err := r.reuseAfter()
	if err != nil {
		return err
	}
	key := bindingKey(r.Account, r.Region, r.Name)
	for range updateAttempts {
		b, brev, err := s.readBinding(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return nil
		case err != nil:
			return err
		case b.QueueID != queueID:
			return nil
		case b.State == BindingReserved && !b.ReuseAfter.Before(until):
			return nil
		case b.State != BindingBound && b.State != BindingReserved:
			return fmt.Errorf("%w: binding %s in state %q", ErrUnavailable, key, b.State)
		case b.V > RecordVersion:
			return fmt.Errorf("%w: binding %s has schema version %d, newer than %d", ErrUnavailable, key, b.V, RecordVersion)
		}

		nb := b
		nb.State, nb.ReuseAfter = BindingReserved, until
		enc, err := nb.Encode()
		if err != nil {
			return err
		}
		_, err = s.kv.update(ctx, key, enc, brev)
		if err == nil {
			return nil
		}
		if isConflict(err) {
			continue
		}
		// A lost reply is settled by the next pass: an applied write shows the
		// reservation; an unmoved or unreadable binding may still take it.
		_, grev, rerr := s.readBinding(ctx, key)
		if (rerr != nil && !errors.Is(rerr, jetstream.ErrKeyNotFound)) || (rerr == nil && grev == brev) {
			return fmt.Errorf("%w: reserve %s outcome unknown: %w", ErrUnavailable, key, err)
		}
	}
	return fmt.Errorf("%w: reserve %s lost %d races", ErrUnavailable, key, updateAttempts)
}
