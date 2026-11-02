package message

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// visibilityAttempts bounds retries after a definite revision conflict.
const visibilityAttempts = 5

var (
	// ErrNotInflight reports a handle whose message is not in flight under
	// that handle: its deadline passed, it was received again, or it is gone.
	ErrNotInflight = errors.New("message: not in flight")
	// ErrVisibilityCap reports a timeout that would end more than 12 hours
	// after the receive that issued the handle.
	ErrVisibilityCap = errors.New("message: visibility beyond the 12-hour limit")
)

// ChangeVisibilityInput is one ChangeMessageVisibility request after wire
// decoding. VisibilityTimeout is whole seconds from now.
type ChangeVisibilityInput struct {
	Account, Region, Queue string
	ReceiptHandle          string
	VisibilityTimeout      int
}

// VisibilityChanger is the ChangeMessageVisibility owner core.
type VisibilityChanger struct {
	Store  Store
	Queues ReceiveQueues
	Keys   *ReceiptKeys
	Now    func() time.Time
}

// ChangeVisibility moves the deadline of the message a current, in-flight
// handle names to now plus the timeout; zero makes it visible at once. The
// deadline may not pass the issuing receive's time plus 12 hours.
func (v *VisibilityChanger) ChangeVisibility(ctx context.Context, in ChangeVisibilityInput) error {
	if v == nil || v.Store == nil || v.Queues == nil {
		return fmt.Errorf("%w: visibility changer is not configured", ErrUnavailable)
	}
	if err := v.Keys.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if in.VisibilityTimeout < 0 || in.VisibilityTimeout > MaxVisibilitySeconds {
		return fmt.Errorf("%w: VisibilityTimeout must be between 0 and %d", ErrInvalidParameter, MaxVisibilitySeconds)
	}
	t, err := v.Queues.ReceiveTarget(ctx, in.Account, in.Region, in.Queue)
	if err != nil {
		return err
	}
	if t.Account != in.Account {
		return fmt.Errorf("%w: resolved queue belongs to another account", ErrUnavailable)
	}
	if err := t.Namespace.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	rc, err := v.Keys.Decode(in.ReceiptHandle, t.Namespace.Queue)
	if err != nil {
		return err
	}
	for range visibilityAttempts {
		cur, err := v.Store.GetState(ctx, t.Namespace, rc.Position)
		if errors.Is(err, ErrNotFound) {
			return ErrNotInflight
		}
		if err != nil {
			return fmt.Errorf("%w: read state: %w", ErrUnavailable, err)
		}
		rec, err := decodeState(cur.Value)
		if err != nil {
			return err
		}
		switch {
		case rec.s == StatusDeleted || rec.s == StatusExpired || rec.s == StatusMoved:
			return ErrNotInflight
		case rec.s == StatusMoving:
			return fmt.Errorf("%w: message is being moved", ErrUnavailable)
		case rec.g > rc.Generation:
			return ErrNotInflight
		case rec.g < rc.Generation:
			return fmt.Errorf("%w: state is older than the handle", ErrUnavailable)
		}
		now := v.now().UnixMilli()
		if rec.d <= now {
			return ErrNotInflight
		}
		d := now + int64(in.VisibilityTimeout)*1000
		if d > rec.r+MaxVisibilityTimeout.Milliseconds() {
			return ErrVisibilityCap
		}
		rec.d = d
		value, err := rec.encode()
		if err != nil {
			return fmt.Errorf("%w: encode state: %w", ErrUnavailable, err)
		}
		_, err = v.Store.PutState(ctx, t.Namespace, rc.Position, value, cur.Revision)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrRevisionMismatch):
			continue
		}
		return fmt.Errorf("%w: visibility change outcome unknown: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("%w: visibility change kept conflicting", ErrUnavailable)
}

func (v *VisibilityChanger) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
