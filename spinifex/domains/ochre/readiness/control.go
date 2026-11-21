// Package readiness holds the durable control record that admits, freezes and
// drains readiness-safe endpoint work across daemons, restarts and rollback.
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucket is the bucket holding the control record, separate from the endpoint
// records it governs. Key is its only key, which is never deleted.
const (
	KVBucket = "bedrock-readiness-control"
	Key      = "control"
)

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

// Token is one durable operation. It stays in the record until its holder has
// seen every covered write and side effect return definitively.
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
	// ErrChanged is a fixed-revision write that lost to a newer revision.
	ErrChanged = errors.New("readiness control record changed")
	// ErrOutcomeUnknown is a write that may or may not have landed. Callers must
	// assume it did: an added token is retained and a removed one is not gone.
	ErrOutcomeUnknown = errors.New("readiness control write outcome unknown")
	// ErrWrongState is a transition whose phase, epoch or rollback does not hold.
	ErrWrongState = errors.New("readiness control record is in the wrong state")
	// ErrNotAdmitted refuses new work in the current phase, epoch or rollback.
	ErrNotAdmitted = errors.New("readiness control refuses new work")
	// ErrTokensOutstanding refuses finalizing while any token remains.
	ErrTokensOutstanding = errors.New("readiness control tokens outstanding")
	ErrTokenExists       = errors.New("readiness control token already exists")
	ErrTokenNotFound     = errors.New("readiness control token not found")
	ErrInvalid           = errors.New("invalid readiness control request")
	// ErrContended is a token update that kept losing to other writers.
	ErrContended = errors.New("readiness control record contended")
)

// KV is the bucket handle the record is written through. The caller opens and
// binds it; Control never creates or recreates the bucket.
type KV interface {
	Bucket() string
	Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error)
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
	var rec Record
	if err := json.Unmarshal(msg.Data, &rec); err != nil {
		return Record{}, msg.Sequence, fmt.Errorf("decode readiness control: %w", err)
	}
	return rec, msg.Sequence, nil
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
	return c.write(ctx, "initialize", Record{Epoch: 1, Phase: PhasePreparing}, 0)
}

// Activate moves preparing(epoch) to active at rev, the revision the caller read
// before checking the feature level.
func (c *Control) Activate(ctx context.Context, epoch, rev uint64) (uint64, error) {
	return c.transition(ctx, "activate", rev, func(r *Record) error {
		if r.Phase != PhasePreparing || r.Epoch != epoch {
			return fmt.Errorf("%w: activate epoch %d from %s epoch %d", ErrWrongState, epoch, r.Phase, r.Epoch)
		}
		r.Phase = PhaseActive
		return nil
	})
}

// Freeze records rollback as frozen at rev from any phase. It always writes, so
// a writer holding an earlier revision fails even if the record was frozen.
func (c *Control) Freeze(ctx context.Context, rollback string, rev uint64) (uint64, error) {
	if rollback == "" {
		return 0, fmt.Errorf("%w: empty rollback identity", ErrInvalid)
	}
	return c.transition(ctx, "freeze", rev, func(r *Record) error {
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

// Reopen moves frozen or finalizing(epoch) to preparing(epoch+1) at rev. It
// keeps every token; only the new epoch can later be activated.
func (c *Control) Reopen(ctx context.Context, epoch, rev uint64) (uint64, error) {
	return c.transition(ctx, "reopen", rev, func(r *Record) error {
		if (r.Phase != PhaseFrozen && r.Phase != PhaseFinalizing) || r.Epoch != epoch {
			return fmt.Errorf("%w: reopen epoch %d from %s epoch %d", ErrWrongState, epoch, r.Phase, r.Epoch)
		}
		r.Epoch++
		r.Phase = PhasePreparing
		r.Rollback = ""
		return nil
	})
}

// Admit adds a launch or sweep token while active and returns it as recorded.
// Every retry re-reads the phase, so an add delayed past a freeze never lands.
func (c *Control) Admit(ctx context.Context, tok Token) (Token, uint64, error) {
	if tok.Kind != TokenLaunch && tok.Kind != TokenSweep {
		return Token{}, 0, fmt.Errorf("%w: admit kind %q", ErrInvalid, tok.Kind)
	}
	return c.add(ctx, "admit", tok, func(r *Record) error {
		if r.Phase != PhaseActive {
			return fmt.Errorf("%w: phase %s", ErrNotAdmitted, r.Phase)
		}
		return nil
	})
}

// AdmitDrain adds a drain token while frozen for rollback. Drain tokens may
// only journal settle writes.
func (c *Control) AdmitDrain(ctx context.Context, rollback string, tok Token) (Token, uint64, error) {
	if tok.Kind != TokenDrain || rollback == "" {
		return Token{}, 0, fmt.Errorf("%w: drain kind %q rollback %q", ErrInvalid, tok.Kind, rollback)
	}
	tok.Rollback = rollback
	return c.add(ctx, "admit-drain", tok, func(r *Record) error {
		if r.Phase != PhaseFrozen || r.Rollback != rollback {
			return fmt.Errorf("%w: drain for rollback %q in %s rollback %q", ErrNotAdmitted, rollback, r.Phase, r.Rollback)
		}
		return nil
	})
}

// JournalWrite records j on token id before its write is sent. New claims and
// holds need an active phase and a token from the current epoch.
func (c *Control) JournalWrite(ctx context.Context, id string, j Journal) (uint64, error) {
	if j.Incarnation == "" || j.Key == "" || !validIntent(j.Intent) {
		return 0, fmt.Errorf("%w: journal %+v", ErrInvalid, j)
	}
	return c.update(ctx, "journal", func(r *Record) error {
		tok, ok := r.Tokens[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrTokenNotFound, id)
		}
		if r.Phase == PhaseFinalizing {
			return fmt.Errorf("%w: phase %s", ErrNotAdmitted, r.Phase)
		}
		if j.Intent != IntentSettle && (r.Phase != PhaseActive || tok.Kind == TokenDrain || tok.Epoch != r.Epoch) {
			return fmt.Errorf("%w: %s by %s token from epoch %d in %s epoch %d", ErrNotAdmitted, j.Intent, tok.Kind, tok.Epoch, r.Phase, r.Epoch)
		}
		tok.Journal = append(tok.Journal, j)
		r.Tokens[id] = tok
		return nil
	})
}

// Release removes token id in any phase. A token already absent is not an
// error; an unknown outcome means the caller must still treat it as held.
func (c *Control) Release(ctx context.Context, id string) (uint64, error) {
	return c.update(ctx, "release", func(r *Record) error {
		if _, ok := r.Tokens[id]; !ok {
			return errUnchanged
		}
		delete(r.Tokens, id)
		return nil
	})
}

// errUnchanged ends an update that has nothing to write.
var errUnchanged = errors.New("unchanged")

func validIntent(i Intent) bool {
	return i == IntentClaim || i == IntentHold || i == IntentSettle
}

func (c *Control) add(ctx context.Context, op string, tok Token, admit func(*Record) error) (Token, uint64, error) {
	if tok.ID == "" {
		return Token{}, 0, fmt.Errorf("%w: empty token id", ErrInvalid)
	}
	tok.Journal = nil
	var added Token
	rev, err := c.update(ctx, op, func(r *Record) error {
		if err := admit(r); err != nil {
			return err
		}
		if _, ok := r.Tokens[tok.ID]; ok {
			return fmt.Errorf("%w: %s", ErrTokenExists, tok.ID)
		}
		added = tok
		added.Epoch = r.Epoch
		r.Tokens[tok.ID] = added
		return nil
	})
	if err != nil && !errors.Is(err, ErrOutcomeUnknown) {
		return Token{}, 0, err
	}
	return added, rev, err
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
	newRev, err := c.kv.Update(ctx, Key, data, rev)
	switch {
	case err == nil:
		return newRev, nil
	case kvstore.IsRevisionConflict(err):
		return 0, fmt.Errorf("%w: %s at revision %d: %w", ErrChanged, op, rev, err)
	default:
		return 0, fmt.Errorf("%w: %s at revision %d: %w", ErrOutcomeUnknown, op, rev, err)
	}
}
