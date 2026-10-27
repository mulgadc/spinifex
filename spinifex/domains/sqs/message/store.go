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
)

// Shard is the opaque shard a queue was assigned when it was created. The
// domain records it in the queue record; the store maps it to its own layout.
type Shard uint32

// QueueID is the opaque per-CreateQueue identifier. It is never the queue
// name, the account or any other tenant input (ADR-0011:S9).
type QueueID string

// Namespace scopes every message and state record to one queue. It needs no
// store call to exist: the first append to it is its creation.
type Namespace struct {
	Shard Shard
	Queue QueueID
}

// Validate rejects a namespace a store could not key safely. A QueueID is
// 1 to MaxQueueIDLen characters of [A-Za-z0-9_-].
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

// Change is one append observed on a shard's change feed.
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

// NamespaceUsage is one namespace in a shard enumeration.
type NamespaceUsage struct {
	Queue QueueID
	Usage Usage
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
	// Get reads a message by position; ErrNotFound once deleted.
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

	// Feed opens the shard's ordered change feed after cursor. Within a
	// namespace changes arrive in position order; an append deleted before
	// delivery may be omitted. The feed carries no acknowledgement state.
	Feed(ctx context.Context, shard Shard, after Cursor) (Feed, error)

	// DeleteNamespace removes every message and state record of the
	// namespace. It is idempotent and leaves other namespaces untouched.
	DeleteNamespace(ctx context.Context, ns Namespace) error
	// Count reports what the namespace holds; zero for a namespace never
	// written to.
	Count(ctx context.Context, ns Namespace) (Usage, error)
	// Namespaces enumerates every namespace in the shard holding any
	// message or state record.
	Namespaces(ctx context.Context, shard Shard) ([]NamespaceUsage, error)

	// Limits reports the store's configured guarantees.
	Limits() Limits
}

// Feed is an open change feed.
type Feed interface {
	// Next blocks until the next change or until ctx is done.
	Next(ctx context.Context) (Change, error)
	Close() error
}
