package readiness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// ReadEndpoint reads key's raw last-subject revision, including a delete or
// purge marker, checking both bindings before the read and after its answer.
// The checks do not replace durable quarantine or recovery after a restart.
func (o *Operation) ReadEndpoint(ctx context.Context, key string) (SubjectRevision, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.usable(); err != nil {
		return SubjectRevision{}, err
	}
	return o.readEndpointLocked(ctx, key)
}

func (o *Operation) readEndpointLocked(ctx context.Context, key string) (SubjectRevision, error) {
	c := o.c
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

// Settlement is one proposed settle: the admitted token, the key, the observed
// revision and value, and the replacement, nil for a removal. Owned reports
// that Revision is this Operation's own confirmed write.
type Settlement struct {
	TokenID  string
	Kind     TokenKind
	Epoch    uint64
	Rollback string
	Node     string
	Target   string
	Key      string
	Revision uint64
	Owned    bool
	Old      []byte
	Next     []byte
}

// SettleCheck is the endpoint domain's rule that a Settlement only finishes or
// removes obligations the old value holds. It gets copies of both values. A
// guarded Control without one refuses every settle.
type SettleCheck func(Settlement) error

// WriteEndpoint journals a claim or hold on key at expected, then publishes
// value once at that exact revision while holding off Release. An unconfirmed
// publish or check fences the Operation.
func (o *Operation) WriteEndpoint(ctx context.Context, key string, intent Intent, value []byte, expected uint64) (uint64, error) {
	if intent != IntentClaim && intent != IntentHold {
		return 0, fmt.Errorf("%w: endpoint write intent %q; settle through SettleEndpoint or DeleteEndpoint", ErrInvalid, intent)
	}
	value = bytes.Clone(value)
	return o.endpointWrite(ctx, key, intent, expected, false, nil, func(bucket string) (uint64, error) {
		return WriteAt(ctx, o.c.js, bucket, key, value, expected)
	})
}

// SettleEndpoint rewrites the live value at exactly expected as the SettleCheck
// allows; only a drain token may rewrite a record it did not write. ErrChanged
// voids the observation and any probe of it: re-read and re-probe first.
func (o *Operation) SettleEndpoint(ctx context.Context, key string, value []byte, expected uint64) (uint64, error) {
	if value == nil {
		return 0, fmt.Errorf("%w: settle of %s needs a value; remove through DeleteEndpoint", ErrInvalid, key)
	}
	value = bytes.Clone(value)
	return o.endpointWrite(ctx, key, IntentSettle, expected, false, func(ctx context.Context) error {
		own, ok := o.owned[key]
		owned := ok && own == expected && expected != 0
		if !owned && o.tok.Kind != TokenDrain {
			return fmt.Errorf("%w: %s at revision %d is not this operation's confirmed write", ErrNotSettlement, key, expected)
		}
		return o.checkSettle(ctx, key, expected, owned, value)
	}, func(bucket string) (uint64, error) {
		return WriteAt(ctx, o.c.js, bucket, key, value, expected)
	})
}

// DeleteEndpoint removes the live value at exactly expected as the
// SettleCheck allows. A drain token never removes a record.
func (o *Operation) DeleteEndpoint(ctx context.Context, key string, expected uint64) (uint64, error) {
	return o.endpointWrite(ctx, key, IntentSettle, expected, true, func(ctx context.Context) error {
		if o.tok.Kind == TokenDrain {
			return fmt.Errorf("%w: a drain token does not remove %s", ErrNotSettlement, key)
		}
		own, ok := o.owned[key]
		return o.checkSettle(ctx, key, expected, ok && own == expected && expected != 0, nil)
	}, func(bucket string) (uint64, error) {
		return deleteAt(ctx, o.c.js, bucket, key, expected)
	})
}

// checkSettle reads the live value at exactly expected and asks the domain
// rule whether next settles it. The fixed-revision send keeps that value.
func (o *Operation) checkSettle(ctx context.Context, key string, expected uint64, owned bool, next []byte) error {
	if o.c.settles == nil {
		return fmt.Errorf("%w: no endpoint settlement rule is configured", ErrNotSettlement)
	}
	cur, err := o.readEndpointLocked(ctx, key)
	if err != nil {
		return err
	}
	if !cur.Live() || cur.Seq != expected {
		return fmt.Errorf("%w: %s holds %s at revision %d, not a value at %d", ErrNotSettlement, key, cur.Op, cur.Seq, expected)
	}
	s := Settlement{
		TokenID: o.id, Kind: o.tok.Kind, Epoch: o.tok.Epoch, Rollback: o.tok.Rollback, Node: o.tok.Node, Target: o.tok.Target,
		Key: key, Revision: expected, Owned: owned, Old: bytes.Clone(cur.Value), Next: bytes.Clone(next),
	}
	if err := o.c.settles(s); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotSettlement, key, err)
	}
	return nil
}

func (o *Operation) usable() error {
	if o.c.endpoints == nil {
		return fmt.Errorf("%w: endpoint access needs a guarded control", ErrInvalid)
	}
	if o.released {
		return fmt.Errorf("%w: %s", ErrTokenReleased, o.id)
	}
	return o.fenced
}

// endpointWrite runs check, journals the write, checks both bindings, sends it
// once and checks them again, all under o.mu. Any unknown outcome fences o;
// a confirmed write is owned unless it removes the key.
func (o *Operation) endpointWrite(ctx context.Context, key string, intent Intent, expected uint64, removes bool,
	check func(context.Context) error, send func(bucket string) (uint64, error),
) (uint64, error) {
	c := o.c
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.usable(); err != nil {
		return 0, err
	}
	bucket := c.endpoints.binding.Bucket
	if err := checkKey(bucket, key); err != nil {
		return 0, err
	}
	incarnation, err := c.endpoints.binding.Incarnation()
	if err != nil {
		return 0, err
	}
	if check != nil {
		if err := check(ctx); err != nil {
			return 0, err
		}
	}
	j := Journal{Incarnation: incarnation, Key: key, ExpectedRevision: expected, Intent: intent}
	if _, err := o.journalLocked(ctx, j); err != nil {
		return 0, err
	}
	if err := c.verify(ctx); err != nil {
		return 0, err
	}
	rev, err := send(bucket)
	if verr := c.verify(ctx); verr != nil {
		err = fmt.Errorf("%w: endpoint write answered (%s) but %w", ErrOutcomeUnknown, publishResult(rev, err), verr)
	}
	switch {
	case errors.Is(err, ErrOutcomeUnknown):
		o.fenced = fmt.Errorf("%w: %s after endpoint write: %w", ErrTokenFenced, o.id, err)
		return 0, err
	case err != nil:
		return 0, err
	}
	if removes {
		delete(o.owned, key)
		return rev, nil
	}
	if o.owned == nil {
		o.owned = map[string]uint64{}
	}
	o.owned[key] = rev
	return rev, nil
}
