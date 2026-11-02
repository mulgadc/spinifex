package message

import (
	"context"
	"errors"
	"fmt"
)

// deleteAttempts bounds retries after a definite revision conflict.
const deleteAttempts = 5

// DeleteInput is one DeleteMessage request after wire decoding.
type DeleteInput struct {
	Account, Region, Queue string
	ReceiptHandle          string
}

// Deleter is the DeleteMessage owner core. It writes the deleted tombstone
// only; removing the body and then the state record is the sweeper's.
type Deleter struct {
	Store  Store
	Queues ReceiveQueues
	Keys   *ReceiptKeys
}

// Delete tombstones the message a current handle names. An older handle, or
// a message already tombstoned or gone, succeeds without writing; a handle
// that does not authenticate for this queue is ErrReceiptInvalid.
func (d *Deleter) Delete(ctx context.Context, in DeleteInput) error {
	if d == nil || d.Store == nil || d.Queues == nil {
		return fmt.Errorf("%w: deleter is not configured", ErrUnavailable)
	}
	if err := d.Keys.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	t, err := d.Queues.ReceiveTarget(ctx, in.Account, in.Region, in.Queue)
	if err != nil {
		return err
	}
	if t.Account != in.Account {
		return fmt.Errorf("%w: resolved queue belongs to another account", ErrUnavailable)
	}
	if err := t.Namespace.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	rc, err := d.Keys.Decode(in.ReceiptHandle, t.Namespace.Queue)
	if err != nil {
		return err
	}
	for range deleteAttempts {
		cur, rec, done, err := d.current(ctx, t.Namespace, rc)
		if done || err != nil {
			return err
		}
		rec.s = StatusDeleted
		value, err := rec.encode()
		if err != nil {
			return fmt.Errorf("%w: encode tombstone: %w", ErrUnavailable, err)
		}
		_, err = d.Store.PutState(ctx, t.Namespace, rc.Position, value, cur.Revision)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrRevisionMismatch):
			continue
		}
		return d.settle(ctx, t.Namespace, rc, err)
	}
	return fmt.Errorf("%w: delete kept conflicting", ErrUnavailable)
}

// current reads the state a handle names. done reports an outcome that needs
// no write: no record, a tombstone, or a newer receive.
func (d *Deleter) current(ctx context.Context, ns Namespace, rc Receipt) (State, stateRecord, bool, error) {
	cur, err := d.Store.GetState(ctx, ns, rc.Position)
	if errors.Is(err, ErrNotFound) {
		return State{}, stateRecord{}, true, nil
	}
	if err != nil {
		return State{}, stateRecord{}, true, fmt.Errorf("%w: read state: %w", ErrUnavailable, err)
	}
	rec, err := decodeState(cur.Value)
	if err != nil {
		return State{}, stateRecord{}, true, err
	}
	switch {
	case rec.s == StatusDeleted || rec.s == StatusExpired || rec.s == StatusMoved:
		return State{}, stateRecord{}, true, nil
	case rec.g > rc.Generation:
		return State{}, stateRecord{}, true, nil
	case rec.g < rc.Generation:
		return State{}, stateRecord{}, true, fmt.Errorf("%w: state is older than the handle", ErrUnavailable)
	case rec.s == StatusMoving:
		return State{}, stateRecord{}, true, fmt.Errorf("%w: message is being moved", ErrUnavailable)
	}
	return cur, rec, false, nil
}

// settle decides a tombstone write whose reply was lost. A tombstone or a
// newer receive now in place is success either way; a record still at this
// handle's generation may yet take the write, so the outcome is unknown.
func (d *Deleter) settle(ctx context.Context, ns Namespace, rc Receipt, cause error) error {
	_, _, done, err := d.current(ctx, ns, rc)
	if err == nil && done {
		return nil
	}
	return fmt.Errorf("%w: delete outcome unknown: %w", ErrUnavailable, cause)
}
