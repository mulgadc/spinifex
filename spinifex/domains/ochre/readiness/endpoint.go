package readiness

import (
	"context"
	"errors"
	"fmt"
)

// ReadEndpoint reads key's raw last-subject revision, including a delete or
// purge marker, checking both bindings before the read and after its answer.
// The checks do not replace durable quarantine or recovery after a restart.
func (o *Operation) ReadEndpoint(ctx context.Context, key string) (SubjectRevision, error) {
	c := o.c
	if c.endpoints == nil {
		return SubjectRevision{}, fmt.Errorf("%w: endpoint read needs a guarded control", ErrInvalid)
	}
	if err := c.verify(ctx); err != nil {
		return SubjectRevision{}, err
	}
	rev, err := ReadRevision(ctx, c.js, c.endpoints.binding.Bucket, key)
	if c.afterRead != nil {
		c.afterRead()
	}
	if verr := c.verify(ctx); verr != nil {
		return SubjectRevision{}, verr
	}
	return rev, err
}

// WriteEndpoint journals the bound endpoint incarnation, key, expected revision
// and intent, then publishes value once at that revision while holding off
// Release. An unconfirmed publish or check fences the Operation.
func (o *Operation) WriteEndpoint(ctx context.Context, key string, intent Intent, value []byte, expected uint64) (uint64, error) {
	c := o.c
	if c.endpoints == nil {
		return 0, fmt.Errorf("%w: endpoint write needs a guarded control", ErrInvalid)
	}
	bucket := c.endpoints.binding.Bucket
	if err := checkKey(bucket, key); err != nil {
		return 0, err
	}
	if !validIntent(intent) {
		return 0, fmt.Errorf("%w: intent %q", ErrInvalid, intent)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.released {
		return 0, fmt.Errorf("%w: %s", ErrTokenReleased, o.id)
	}
	if o.fenced != nil {
		return 0, o.fenced
	}
	j := Journal{Incarnation: c.endpoints.binding.Incarnation(), Key: key, ExpectedRevision: expected, Intent: intent}
	if _, err := o.journalLocked(ctx, j); err != nil {
		return 0, err
	}
	if err := c.verify(ctx); err != nil {
		return 0, err
	}
	rev, err := WriteAt(ctx, c.js, bucket, key, value, expected)
	if verr := c.verify(ctx); verr != nil {
		err = fmt.Errorf("%w: endpoint write answered (%s) but %w", ErrOutcomeUnknown, publishResult(rev, err), verr)
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		o.fenced = fmt.Errorf("%w: %s after endpoint write: %w", ErrTokenFenced, o.id, err)
		return 0, err
	}
	return rev, err
}
