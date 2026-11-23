// Package readiness holds the durable control record that admits, freezes and
// drains readiness-safe endpoint work across daemons, restarts and rollback.
package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucket is the bucket holding the control record, separate from the endpoint
// records it governs. Key is its only key, which is never deleted.
const (
	KVBucket = "bedrock-readiness-control"
	Key      = "control"
)

// SchemaVersion is the only record format this code reads or writes. Any other
// version, field or enum value fails closed and is never rewritten.
const SchemaVersion = 1

// Phase gates which work the record admits.
type Phase string

const (
	PhasePreparing  Phase = "preparing"
	PhaseActive     Phase = "active"
	PhaseFrozen     Phase = "frozen"
	PhaseFinalizing Phase = "finalizing"
)

// TokenKind says what a token covers. Launch and sweep tokens need an active
// phase; drain tokens are admitted only while frozen for the current rollback.
type TokenKind string

const (
	TokenLaunch TokenKind = "launch"
	TokenSweep  TokenKind = "sweep"
	TokenDrain  TokenKind = "drain"
)

// Intent classifies an endpoint write a token is about to send. Claim and hold
// create new obligations; settle only finishes or removes existing ones.
type Intent string

const (
	IntentClaim  Intent = "claim"
	IntentHold   Intent = "hold"
	IntentSettle Intent = "settle"
)

// Journal names one endpoint write before it is sent: the target stream's
// incarnation, the key and the revision the write expects.
type Journal struct {
	Incarnation      string `json:"incarnation"`
	Key              string `json:"key"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Intent           Intent `json:"intent"`
}

// Token is one durable operation, admitted for one epoch. It stays in the record
// until its holder has seen every covered write and side effect return definitively.
type Token struct {
	ID       string    `json:"id"`
	Kind     TokenKind `json:"kind"`
	Epoch    uint64    `json:"epoch"`
	Node     string    `json:"node,omitempty"`
	Target   string    `json:"target,omitempty"`
	Rollback string    `json:"rollback,omitempty"`
	Journal  []Journal `json:"journal,omitempty"`
}

// Record is the control record. Rollback is the identity of the rollback that
// last froze it, kept through finalizing.
type Record struct {
	Version  int              `json:"version"`
	Epoch    uint64           `json:"epoch"`
	Phase    Phase            `json:"phase"`
	Rollback string           `json:"rollback,omitempty"`
	Tokens   map[string]Token `json:"tokens,omitempty"`
}

var (
	// ErrAbsent is a key that has never been written.
	ErrAbsent = errors.New("readiness control record is absent")
	// ErrDeleted is a key carrying a delete or purge marker. The record is never
	// deleted, so this is damage, not an empty record.
	ErrDeleted = errors.New("readiness control record was deleted")
	// ErrChanged is a fixed-revision write of any readiness record that lost to
	// a newer revision.
	ErrChanged = errors.New("readiness record changed")
	// ErrOutcomeUnknown is a readiness write that may or may not have landed. A
	// control add, journal or transition must be assumed landed; a release may be.
	ErrOutcomeUnknown = errors.New("readiness write outcome unknown")
	// ErrWrongState is a transition whose phase, epoch or rollback does not hold.
	ErrWrongState = errors.New("readiness control record is in the wrong state")
	// ErrNotAdmitted refuses new work in the current phase, epoch or rollback.
	ErrNotAdmitted = errors.New("readiness control refuses new work")
	// ErrTokensOutstanding refuses finalizing while any token remains.
	ErrTokensOutstanding = errors.New("readiness control tokens outstanding")
	ErrTokenExists       = errors.New("readiness control token already exists")
	ErrTokenNotFound     = errors.New("readiness control token not found")
	ErrInvalid           = errors.New("invalid readiness control request")
	// ErrTokenReleased refuses work on an Operation after its release attempt.
	ErrTokenReleased = errors.New("readiness control token was released")
	// ErrUnsupported is a record in a version or form this code does not know.
	ErrUnsupported = errors.New("unsupported readiness control record")
	// ErrContended is a token update that kept losing to other writers.
	ErrContended = errors.New("readiness control record contended")
)

// KV names the bucket holding the record. The caller opens and binds it;
// Control never creates or recreates the bucket, and writes through js.
type KV interface {
	Bucket() string
}

var _ KV = jetstream.KeyValue(nil)

const (
	updateAttempts = 32
	backoffBase    = 2 * time.Millisecond
)

// Control reads the record from the stream leader and writes it only by a
// single compare-and-set at the revision read.
type Control struct {
	js          jetstream.JetStream
	kv          KV
	beforeWrite func(op string)
}

// New returns a Control over an already-opened kv; js serves leader reads.
func New(js jetstream.JetStream, kv KV) *Control {
	return &Control{js: js, kv: kv}
}

// Read returns the record and its revision from the stream leader.
func (c *Control) Read(ctx context.Context) (Record, uint64, error) {
	bucket := c.kv.Bucket()
	msg, err := kvstore.LeaderMsgGet(ctx, c.js, "KV_"+bucket, kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + Key})
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return Record{}, 0, ErrAbsent
	}
	if err != nil {
		return Record{}, 0, fmt.Errorf("read readiness control: %w", err)
	}
	if op := msg.Header.Get("KV-Operation"); op == "DEL" || op == "PURGE" {
		return Record{}, msg.Sequence, fmt.Errorf("%w at revision %d", ErrDeleted, msg.Sequence)
	}
	rec, err := decode(msg.Data)
	if err != nil {
		return Record{}, msg.Sequence, fmt.Errorf("revision %d: %w", msg.Sequence, err)
	}
	return rec, msg.Sequence, nil
}

// decode accepts only SchemaVersion with known fields and enum values.
func decode(data []byte) (Record, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if head.Version != SchemaVersion {
		return Record{}, fmt.Errorf("%w: version %d", ErrUnsupported, head.Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	switch rec.Phase {
	case PhasePreparing, PhaseActive, PhaseFrozen, PhaseFinalizing:
	default:
		return Record{}, fmt.Errorf("%w: phase %q", ErrUnsupported, rec.Phase)
	}
	for id, tok := range rec.Tokens {
		if tok.ID != id || (tok.Kind != TokenLaunch && tok.Kind != TokenSweep && tok.Kind != TokenDrain) {
			return Record{}, fmt.Errorf("%w: token %q kind %q", ErrUnsupported, id, tok.Kind)
		}
		for _, j := range tok.Journal {
			if !validIntent(j.Intent) {
				return Record{}, fmt.Errorf("%w: token %q intent %q", ErrUnsupported, id, j.Intent)
			}
		}
	}
	return rec, nil
}

// Initialize writes the first record, preparing at epoch 1. It refuses a key
// that exists or carries a delete marker.
func (c *Control) Initialize(ctx context.Context) (uint64, error) {
	_, _, err := c.Read(ctx)
	switch {
	case errors.Is(err, ErrAbsent):
	case err == nil:
		return 0, fmt.Errorf("%w: already initialized", ErrWrongState)
	default:
		return 0, err
	}
	return c.write(ctx, "initialize", Record{Version: SchemaVersion, Epoch: 1, Phase: PhasePreparing}, 0)
}

// Activate moves preparing(epoch) with no tokens to active at rev, the revision
// the caller read before checking the feature level.
func (c *Control) Activate(ctx context.Context, epoch, rev uint64) (uint64, error) {
	return c.transition(ctx, "activate", rev, func(r *Record) error {
		if r.Phase != PhasePreparing || r.Epoch != epoch {
			return fmt.Errorf("%w: activate epoch %d from %s epoch %d", ErrWrongState, epoch, r.Phase, r.Epoch)
		}
		if len(r.Tokens) > 0 {
			return fmt.Errorf("%w: %d", ErrTokensOutstanding, len(r.Tokens))
		}
		r.Phase = PhaseActive
		return nil
	})
}

// Freeze records rollback as frozen at rev. It always writes, so a writer holding
// an earlier revision fails even if the record was frozen. Finalizing stays
// closed: only Reopen leaves it.
func (c *Control) Freeze(ctx context.Context, rollback string, rev uint64) (uint64, error) {
	if rollback == "" {
		return 0, fmt.Errorf("%w: empty rollback identity", ErrInvalid)
	}
	return c.transition(ctx, "freeze", rev, func(r *Record) error {
		if r.Phase == PhaseFinalizing {
			return fmt.Errorf("%w: freeze for rollback %q while finalizing rollback %q", ErrWrongState, rollback, r.Rollback)
		}
		r.Phase = PhaseFrozen
		r.Rollback = rollback
		return nil
	})
}

// Finalize moves frozen to finalizing at rev, the revision at which the caller
// saw zero tokens before taking its inventory.
func (c *Control) Finalize(ctx context.Context, rollback string, rev uint64) (uint64, error) {
	return c.transition(ctx, "finalize", rev, func(r *Record) error {
		if r.Phase != PhaseFrozen || r.Rollback != rollback {
			return fmt.Errorf("%w: finalize rollback %q from %s rollback %q", ErrWrongState, rollback, r.Phase, r.Rollback)
		}
		if len(r.Tokens) > 0 {
			return fmt.Errorf("%w: %d", ErrTokensOutstanding, len(r.Tokens))
		}
		r.Phase = PhaseFinalizing
		return nil
	})
}

// Reopen moves frozen or finalizing(epoch) to preparing(epoch+1) at rev. Every
// token must be settled first, so uncertainty keeps the record frozen.
func (c *Control) Reopen(ctx context.Context, epoch, rev uint64) (uint64, error) {
	return c.transition(ctx, "reopen", rev, func(r *Record) error {
		if (r.Phase != PhaseFrozen && r.Phase != PhaseFinalizing) || r.Epoch != epoch {
			return fmt.Errorf("%w: reopen epoch %d from %s epoch %d", ErrWrongState, epoch, r.Phase, r.Epoch)
		}
		if len(r.Tokens) > 0 {
			return fmt.Errorf("%w: %d", ErrTokensOutstanding, len(r.Tokens))
		}
		r.Epoch++
		r.Phase = PhasePreparing
		r.Rollback = ""
		return nil
	})
}

// Operation is the owner's handle on one admitted token. It serializes the
// owner's journal entries with release and refuses both after a release
// attempt; it is an in-process aid, not a durable fence.
type Operation struct {
	c  *Control
	id string

	mu       sync.Mutex
	released bool
}

// ID is the token's unique ID, which the owner must never reuse.
func (o *Operation) ID() string { return o.id }

// Admit adds a launch or sweep token while tok.Epoch is active. Every retry
// re-reads phase and epoch, so an add delayed past a freeze or into a later
// epoch never lands. An unknown outcome returns no Operation.
func (c *Control) Admit(ctx context.Context, tok Token) (*Operation, uint64, error) {
	if tok.Kind != TokenLaunch && tok.Kind != TokenSweep {
		return nil, 0, fmt.Errorf("%w: admit kind %q", ErrInvalid, tok.Kind)
	}
	return c.add(ctx, "admit", tok, func(r *Record) error {
		if r.Phase != PhaseActive || r.Epoch != tok.Epoch {
			return fmt.Errorf("%w: epoch %d in %s epoch %d", ErrNotAdmitted, tok.Epoch, r.Phase, r.Epoch)
		}
		return nil
	})
}

// AdmitDrain adds a drain token while tok.Epoch is frozen for rollback. Drain
// tokens may only journal settle writes.
func (c *Control) AdmitDrain(ctx context.Context, rollback string, tok Token) (*Operation, uint64, error) {
	if tok.Kind != TokenDrain || rollback == "" {
		return nil, 0, fmt.Errorf("%w: drain kind %q rollback %q", ErrInvalid, tok.Kind, rollback)
	}
	tok.Rollback = rollback
	return c.add(ctx, "admit-drain", tok, func(r *Record) error {
		if r.Phase != PhaseFrozen || r.Rollback != rollback || r.Epoch != tok.Epoch {
			return fmt.Errorf("%w: drain for rollback %q epoch %d in %s rollback %q epoch %d",
				ErrNotAdmitted, rollback, tok.Epoch, r.Phase, r.Rollback, r.Epoch)
		}
		return nil
	})
}

// Journal records j on the token before its write is sent. New claims and
// holds need an active phase and a token from the current epoch.
func (o *Operation) Journal(ctx context.Context, j Journal) (uint64, error) {
	if j.Incarnation == "" || j.Key == "" || !validIntent(j.Intent) {
		return 0, fmt.Errorf("%w: journal %+v", ErrInvalid, j)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.released {
		return 0, fmt.Errorf("%w: %s", ErrTokenReleased, o.id)
	}
	return o.c.update(ctx, "journal", func(r *Record) error {
		tok, ok := r.Tokens[o.id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrTokenNotFound, o.id)
		}
		if r.Phase == PhaseFinalizing {
			return fmt.Errorf("%w: phase %s", ErrNotAdmitted, r.Phase)
		}
		if j.Intent != IntentSettle && (r.Phase != PhaseActive || tok.Kind == TokenDrain || tok.Epoch != r.Epoch) {
			return fmt.Errorf("%w: %s by %s token from epoch %d in %s epoch %d", ErrNotAdmitted, j.Intent, tok.Kind, tok.Epoch, r.Phase, r.Epoch)
		}
		tok.Journal = append(tok.Journal, j)
		r.Tokens[o.id] = tok
		return nil
	})
}

// Release removes the token in any phase. The owner calls it only after every
// covered call, side effect and retry has stopped, and resumes no work after
// it, even when its outcome is unknown; only the release itself may be retried.
func (o *Operation) Release(ctx context.Context) (uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.released = true
	return o.c.update(ctx, "release", func(r *Record) error {
		if _, ok := r.Tokens[o.id]; !ok {
			return errUnchanged
		}
		delete(r.Tokens, o.id)
		return nil
	})
}

// errUnchanged ends an update that has nothing to write.
var errUnchanged = errors.New("unchanged")

func validIntent(i Intent) bool {
	return i == IntentClaim || i == IntentHold || i == IntentSettle
}

func (c *Control) add(ctx context.Context, op string, tok Token, admit func(*Record) error) (*Operation, uint64, error) {
	if tok.ID == "" || tok.Epoch == 0 {
		return nil, 0, fmt.Errorf("%w: token id %q epoch %d", ErrInvalid, tok.ID, tok.Epoch)
	}
	tok.Journal = nil
	rev, err := c.update(ctx, op, func(r *Record) error {
		if err := admit(r); err != nil {
			return err
		}
		if _, ok := r.Tokens[tok.ID]; ok {
			return fmt.Errorf("%w: %s", ErrTokenExists, tok.ID)
		}
		r.Tokens[tok.ID] = tok
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return &Operation{c: c, id: tok.ID}, rev, nil
}

// transition applies f to the record read at rev and writes it once at rev.
func (c *Control) transition(ctx context.Context, op string, rev uint64, f func(*Record) error) (uint64, error) {
	rec, cur, err := c.Read(ctx)
	if err != nil {
		return 0, err
	}
	if cur != rev {
		return 0, fmt.Errorf("%w: %s expected revision %d, found %d", ErrChanged, op, rev, cur)
	}
	if err := f(&rec); err != nil {
		return 0, err
	}
	return c.write(ctx, op, rec, rev)
}

// update re-reads and retries f on a lost race. It never retries an unknown
// outcome, which would be a second publish racing the first.
func (c *Control) update(ctx context.Context, op string, f func(*Record) error) (uint64, error) {
	for i := range updateAttempts {
		rec, rev, err := c.Read(ctx)
		if err != nil {
			return 0, err
		}
		if rec.Tokens == nil {
			rec.Tokens = map[string]Token{}
		}
		if err := f(&rec); err != nil {
			if errors.Is(err, errUnchanged) {
				return rev, nil
			}
			return 0, err
		}
		newRev, err := c.write(ctx, op, rec, rev)
		if !errors.Is(err, ErrChanged) {
			return newRev, err
		}
		backoff := backoffBase * time.Duration(i+1)
		jitter := time.Duration(rand.Int64N(int64(backoff))) //nolint:gosec // jitter, not cryptographic
		select {
		case <-time.After(backoff/2 + jitter):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, fmt.Errorf("%w: %s after %d attempts", ErrContended, op, updateAttempts)
}

func (c *Control) write(ctx context.Context, op string, rec Record, rev uint64) (uint64, error) {
	if len(rec.Tokens) == 0 {
		rec.Tokens = nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode readiness control: %w", err)
	}
	if c.beforeWrite != nil {
		c.beforeWrite(op)
	}
	bucket := c.kv.Bucket()
	msg := &nats.Msg{Subject: "$KV." + bucket + "." + Key, Data: data}
	return publishOnce(ctx, c.js, msg, bucket, rev, op)
}
