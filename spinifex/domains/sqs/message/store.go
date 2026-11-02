// Package message owns SQS message state and the store-neutral interface the
// domain reaches the message store through (ADR-0011:S6). No store type,
// subject, stream, bucket or option crosses this interface.
package message

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MinRetentionBackstop is the shortest retention backstop a store may apply:
// 14 days, the longest MessageRetentionPeriod, plus 1 hour (ADR-0011:S12).
const MinRetentionBackstop = 14*24*time.Hour + time.Hour

// MaxQueueIDLen bounds a QueueID so every store can carry it in a key.
const MaxQueueIDLen = 64

var (
	// ErrNotFound reports that no message or state record exists at the key.
	ErrNotFound = errors.New("message: not found")
	// ErrRevisionMismatch reports that a conditional write's expected
	// revision is not the record's current revision.
	ErrRevisionMismatch = errors.New("message: revision mismatch")
	// ErrInvalidArgument reports a malformed namespace, key or input.
	ErrInvalidArgument = errors.New("message: invalid argument")
	// ErrFeedClosed is returned by a Feed's Next once Close has begun.
	ErrFeedClosed = errors.New("message: feed closed")
)

// Shard is an opaque allocation a queue was given when it was created. The
// domain records it in the queue record and it never changes, whatever the
// store's later topology (ADR-0011:S10); the store maps it to its own layout.
type Shard uint32

// QueueID is the opaque per-CreateQueue identifier. It is never the queue
// name, the account or any other tenant input (ADR-0011:S9).
type QueueID string

// Namespace scopes every message and state record to one queue. It needs no
// store call to exist: the first append to it is its creation. MessageShard
// places its messages and StateShard its state records, independently.
type Namespace struct {
	MessageShard Shard
	StateShard   Shard
	Queue        QueueID
}

// Validate rejects a namespace a store could not key safely. A QueueID is
// 1 to MaxQueueIDLen characters of [A-Za-z0-9_-]. Neither shard is checked
// against a current topology: an allocation outlives a count change.
func (n Namespace) Validate() error {
	if n.Queue == "" || len(n.Queue) > MaxQueueIDLen {
		return fmt.Errorf("%w: queue ID length %d outside 1..%d", ErrInvalidArgument, len(n.Queue), MaxQueueIDLen)
	}
	for _, r := range n.Queue {
		if !isQueueIDRune(r) {
			return fmt.Errorf("%w: queue ID contains %q", ErrInvalidArgument, r)
		}
	}
	return nil
}

func isQueueIDRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

// Position is a message's stable place in its namespace. Positions are
// non-zero, strictly increase in append order within a namespace, survive a
// restart, and are never reused, including after deletion.
type Position uint64

// Revision is a state record's version for conditional writes. Zero means
// "no record": a write expecting zero creates the record.
type Revision uint64

// Cursor is an opaque point in a shard's change feed. Zero is the start.
type Cursor uint64

// AppendInput is one message to store. Meta is small metadata the change feed
// carries without the body; the store neither reads nor interprets either.
type AppendInput struct {
	Body []byte
	Meta map[string]string
	// IdempotencyKey, when set, makes the append idempotent within the
	// store's IdempotencyWindow: a repeat with the same key in the same
	// namespace stores nothing and returns the first append's position.
	IdempotencyKey string
}

// AppendResult reports where a message was stored.
type AppendResult struct {
	Position Position
	// Duplicate is true when IdempotencyKey matched an earlier append and
	// nothing new was stored.
	Duplicate bool
}

// Message is a stored message as read back.
type Message struct {
	Position Position
	Body     []byte
	Meta     map[string]string
}

// State is a per-message state record. Value is the domain's encoding; the
// store holds no copy of any SQS semantic in its delivery machinery (S5).
type State struct {
	Value    []byte
	Revision Revision
}

// Change is one append observed on a message shard's change feed.
type Change struct {
	Namespace Namespace
	Position  Position
	Meta      map[string]string
	// Cursor resumes the feed immediately after this change.
	Cursor Cursor
}

// Usage is what a namespace currently holds in the store.
type Usage struct {
	Messages uint64
	States   uint64
}

// NamespaceUsage is one namespace in a message shard enumeration. It carries
// the complete namespace, both allocations included, so a caller without the
// queue record can still count and clean a namespace holding only state.
type NamespaceUsage struct {
	Namespace Namespace
	Usage     Usage
}

// Limits are the store's configured guarantees.
type Limits struct {
	// RetentionBackstop is the age past which the store may drop any
	// message; never less than MinRetentionBackstop and never the
	// retention a message reports (ADR-0011:S5, S12).
	RetentionBackstop time.Duration
	// IdempotencyWindow is how long an IdempotencyKey is remembered.
	IdempotencyWindow time.Duration
}

// Store is the message store as the SQS domain sees it (ADR-0011:S6).
type Store interface {
	// Append durably stores a message and returns only after the write is
	// acknowledged as durable (ADR-0011:S3).
	Append(ctx context.Context, ns Namespace, in AppendInput) (AppendResult, error)
	// Get reads a message by position authoritatively: once deleted it
	// returns ErrNotFound or fails, never the deleted body.
	Get(ctx context.Context, ns Namespace, pos Position) (Message, error)
	// Delete removes one message. Deleting an absent message succeeds, so
	// a replay after a crash is safe.
	Delete(ctx context.Context, ns Namespace, pos Position) error
	// DeleteThrough removes every message in the namespace at or before
	// pos, leaving later appends and every state record in place.
	DeleteThrough(ctx context.Context, ns Namespace, pos Position) error

	// GetState reads a message's state record; ErrNotFound if none.
	GetState(ctx context.Context, ns Namespace, pos Position) (State, error)
	// PutState writes a state record only if its current revision equals
	// expected, and returns the new revision; else ErrRevisionMismatch.
	PutState(ctx context.Context, ns Namespace, pos Position, value []byte, expected Revision) (Revision, error)
	// DeleteState removes a state record only if its current revision
	// equals expected; else ErrRevisionMismatch or ErrNotFound.
	DeleteState(ctx context.Context, ns Namespace, pos Position, expected Revision) error

	// Feed opens a message shard's ordered change feed after cursor. Within a
	// namespace changes arrive in position order; an append deleted before
	// delivery may be omitted. The feed carries no acknowledgement state.
	Feed(ctx context.Context, messageShard Shard, after Cursor) (Feed, error)
	// Scan returns a page of at most limit surviving appends on a message
	// shard as metadata only, in position order, within a bounded pass.
	// The zero ScanCursor starts a pass and fixes its bound at the shard's
	// latest position; pass each page's Next to continue it.
	Scan(ctx context.Context, messageShard Shard, from ScanCursor, limit int) (ScanPage, error)

	// DeleteNamespace removes every message and state record of the
	// namespace. It is idempotent and leaves other namespaces untouched.
	DeleteNamespace(ctx context.Context, ns Namespace) error
	// Count reports what the namespace holds; zero for a namespace never
	// written to.
	Count(ctx context.Context, ns Namespace) (Usage, error)
	// Namespaces enumerates every namespace whose MessageShard is
	// messageShard and that holds any message or state record, including
	// one whose bodies are all deleted but whose state remains.
	Namespaces(ctx context.Context, messageShard Shard) ([]NamespaceUsage, error)

	// Limits reports the store's configured guarantees.
	Limits() Limits
}

// ScanCursor is a point in a bounded scan pass. A zero Bound starts a new
// pass; every later page of that pass carries the same Bound.
type ScanCursor struct {
	// After is the last position the pass has covered.
	After Cursor
	// Bound is the shard's latest position when the pass started; an
	// append after it is never returned by this pass.
	Bound Cursor
}

// ScanPage is one page of a scan pass. Changes skip deleted positions, so a
// page may be short or empty before the pass is Done.
type ScanPage struct {
	Changes []Change
	Next    ScanCursor
	// Done is true once Next has reached the pass's Bound.
	Done bool
}

// Feed is an open change feed. One goroutine calls Next at a time; Close may
// run from any goroutine, concurrently and repeatedly. An open or reopen begun
// before ctx ended may finish; it is released best-effort and ctx's error returned.
type Feed interface {
	// Next blocks until the next change, until ctx is done or until Close. A
	// change is claimed atomically against Close: claimed first, it is returned;
	// otherwise Next returns ErrFeedClosed and a feed from the last cursor has it.
	Next(ctx context.Context) (Change, error)
	// Close ends the feed. Nil means every resource the feed knows it holds is
	// released; one whose acquisition was ambiguous may outlive Close until the
	// store's bounded expiry. An error means some remain; a later Close retries.
	Close() error
}

// ValidateScan rejects a scan request no store can serve: a non-positive
// limit, or a cursor already past its pass's bound.
func ValidateScan(from ScanCursor, limit int) error {
	switch {
	case limit < 1:
		return fmt.Errorf("%w: scan limit %d", ErrInvalidArgument, limit)
	case from.After > from.Bound:
		return fmt.Errorf("%w: scan cursor after %d is past its bound %d", ErrInvalidArgument, from.After, from.Bound)
	}
	return nil
}
