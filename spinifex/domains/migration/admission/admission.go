// Package admission decides whether a migration record may be created: the
// cluster feature level must show every node runs the termination participant,
// and the account must not be sealed by account termination.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// SealBucket holds one seal per terminated account. It is shared and outlives
// each account's own bucket, so a seal is never removed by the drain.
const SealBucket = "spinifex-migration-seals"

var (
	// ErrNotOffered refuses creation while some node may lack the participant.
	ErrNotOffered = errors.New("migration is not offered until every node runs this release")
	// ErrAccountSealed refuses creation for an account under termination.
	ErrAccountSealed = errors.New("account is sealed for migration by account termination")
)

// LevelAdmitter is the cluster feature-level gate.
type LevelAdmitter interface {
	Admit(ctx context.Context, n int) (done func(), ok bool, err error)
	Active(ctx context.Context, n int) (bool, error)
}

var _ LevelAdmitter = (*featurelevel.Gate)(nil)

// TerminationChecker reports whether IAM holds the account as TERMINATING.
type TerminationChecker interface {
	AccountTerminating(ctx context.Context, accountID string) (bool, error)
}

type sealRecord struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	SealedAt      time.Time `json:"sealed_at"`
}

// Seals reads and writes account seals. A seal is irreversible.
type Seals struct {
	js jetstream.KeyValueManager
}

// NewSeals returns the seal store over js.
func NewSeals(js jetstream.KeyValueManager) *Seals { return &Seals{js: js} }

// Sealed reports whether accountID is sealed. A read failure is an error, and
// every caller treats it as a refusal.
func (s *Seals) Sealed(ctx context.Context, accountID string) (bool, error) {
	kv, err := s.js.KeyValue(ctx, SealBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open migration seals: %w", err)
	}
	_, err = kv.Get(ctx, accountID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read migration seal: %w", err)
	}
	return true, nil
}

// Seal writes accountID's seal, only while IAM holds the account as
// TERMINATING. Sealing a sealed account is success.
func (s *Seals) Seal(ctx context.Context, terminating TerminationChecker, accountID string, now time.Time) error {
	if !awsidentifiers.IsAccountID(accountID) || accountID == awsidentifiers.GlobalAccountID {
		return fmt.Errorf("migration seal: %q is not a tenant account ID", accountID)
	}
	if terminating == nil {
		return errors.New("migration seal: no termination checker")
	}
	ok, err := terminating.AccountTerminating(ctx, accountID)
	if err != nil {
		return fmt.Errorf("migration seal: read account status: %w", err)
	}
	if !ok {
		return ErrNotTerminating
	}
	kv, err := kvstore.GetOrCreateBucket(ctx, s.js, SealBucket, 1)
	if err != nil {
		return fmt.Errorf("open migration seals: %w", err)
	}
	data, err := json.Marshal(sealRecord{SchemaVersion: 1, AccountID: accountID, SealedAt: now.UTC()})
	if err != nil {
		return fmt.Errorf("encode migration seal: %w", err)
	}
	if _, err := kv.Create(ctx, accountID, data); err != nil && !kvstore.IsRevisionConflict(err) {
		return fmt.Errorf("write migration seal: %w", err)
	}
	return nil
}

// ErrNotTerminating refuses a seal for an account IAM does not hold as TERMINATING.
var ErrNotTerminating = errors.New("account is not TERMINATING")

// Gate admits migration record creation.
type Gate struct {
	level LevelAdmitter
	seals *Seals
	js    jetstream.KeyValueManager
	now   func() time.Time
}

// NewGate returns the create gate. now is the clock.
func NewGate(js jetstream.KeyValueManager, level LevelAdmitter, now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{level: level, seals: NewSeals(js), js: js, now: now}
}

// Offered reports whether the migration API may be exposed: the feature level
// that every node runs the participant and the executor is active.
func (g *Gate) Offered(ctx context.Context) (bool, error) {
	if g.level == nil {
		return false, ErrNotOffered
	}
	return g.level.Active(ctx, featurelevel.Migration)
}

// Create writes a new record if the feature level admits it and the account
// is not sealed. The seal is checked again after the write: a record that
// raced a seal is marked for deletion, so the next termination pass drains it.
func (g *Gate) Create(ctx context.Context, rec store.Record) (store.Entry, error) {
	if g.level == nil {
		return store.Entry{}, ErrNotOffered
	}
	done, ok, err := g.level.Admit(ctx, featurelevel.Migration)
	defer done()
	if err != nil {
		return store.Entry{}, fmt.Errorf("read feature level: %w", err)
	}
	if !ok {
		return store.Entry{}, ErrNotOffered
	}
	if err := g.refuseSealed(ctx, rec.AccountID); err != nil {
		return store.Entry{}, err
	}

	s, err := store.OpenOrCreate(ctx, g.js, rec.AccountID)
	if err != nil {
		return store.Entry{}, err
	}
	rec.DeleteRequested, rec.DeleteRequestedAt, rec.Owner = false, nil, nil
	rev, err := s.Create(ctx, rec)
	if err != nil {
		return store.Entry{}, err
	}

	if err := g.refuseSealed(ctx, rec.AccountID); err != nil {
		// The mark outlives a cancelled request; a sweep also retires every
		// record of a sealed account, so a lost mark leaves nothing behind.
		markCtx := context.WithoutCancel(ctx)
		if _, markErr := s.MarkDeleteRequested(markCtx, rec.Kind, rec.ID, g.now()); markErr != nil {
			return store.Entry{}, errors.Join(err, fmt.Errorf("mark raced record: %w", markErr))
		}
		return store.Entry{}, err
	}
	rec.SchemaVersion = store.SchemaVersion
	return store.Entry{Record: rec, Revision: rev}, nil
}

func (g *Gate) refuseSealed(ctx context.Context, accountID string) error {
	sealed, err := g.seals.Sealed(ctx, accountID)
	if err != nil {
		return err
	}
	if sealed {
		return ErrAccountSealed
	}
	return nil
}
