package handlers_iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketAccountNames indexes normalized account name → account ID, making
// duplicate names impossible. CreateAccount does not check names itself.
const KVBucketAccountNames = "spinifex-account-names"

var (
	// ErrNameTaken means a committed reservation already owns the name.
	ErrNameTaken = errors.New("account name already taken")
	// ErrNameInFlight means another caller holds an uncommitted reservation.
	// Retryable: it resolves when that caller commits or releases.
	ErrNameInFlight = errors.New("account name reservation in flight")
)

// nameReservation is the stored value. AccountID is empty between Reserve and
// Commit, which is what distinguishes a live attempt from a finished one.
type nameReservation struct {
	AccountID   string `json:"account_id"`
	ClientToken string `json:"client_token"`
	CreatedAt   string `json:"created_at"`
}

// AccountNameIndex is the name → account-ID reservation store.
type AccountNameIndex struct {
	kv jetstream.KeyValue
}

// NewAccountNameIndex opens (or creates) the reservation bucket at the
// cluster's replica count.
func NewAccountNameIndex(ctx context.Context, js jetstream.JetStream) (*AccountNameIndex, error) {
	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketAccountNames, 5)
	if err != nil {
		return nil, fmt.Errorf("init account names bucket: %w", err)
	}
	return &AccountNameIndex{kv: kv}, nil
}

// nameKey hashes the normalized name. Hashing keeps customer email addresses
// out of KV key names and sidesteps JetStream's key character restrictions in
// one move — raw addresses contain '@' and can contain characters keys reject.
func nameKey(name string) string {
	sum := sha256.Sum256([]byte(NormalizeAccountName(name)))
	return hex.EncodeToString(sum[:])
}

// Reserve claims name for clientToken before the account ID exists. It returns
// ErrNameTaken when the name is already committed, and ErrNameInFlight when a
// different token holds an uncommitted claim. Re-reserving under the same token
// succeeds, so a resumed attempt proceeds past its own reservation.
func (x *AccountNameIndex) Reserve(ctx context.Context, name, clientToken string) error {
	value, err := json.Marshal(nameReservation{
		ClientToken: clientToken,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal reservation: %w", err)
	}

	_, err = x.kv.Create(ctx, nameKey(name), value)
	if err == nil {
		return nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("reserve account name: %w", err)
	}

	existing, err := x.get(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case existing.AccountID != "":
		return ErrNameTaken
	case existing.ClientToken == clientToken:
		return nil
	default:
		return ErrNameInFlight
	}
}

// Commit records the account ID against a reservation this caller holds,
// turning it from a live attempt into a permanent index entry.
func (x *AccountNameIndex) Commit(ctx context.Context, name, accountID, clientToken string) error {
	value, err := json.Marshal(nameReservation{
		AccountID:   accountID,
		ClientToken: clientToken,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal reservation: %w", err)
	}
	if _, err := x.kv.Put(ctx, nameKey(name), value); err != nil {
		return fmt.Errorf("commit account name: %w", err)
	}
	return nil
}

// Release drops an uncommitted reservation so the name can be retried. It never
// removes a committed entry: that would unindex a live account.
func (x *AccountNameIndex) Release(ctx context.Context, name, clientToken string) error {
	existing, err := x.get(ctx, name)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.AccountID != "" || existing.ClientToken != clientToken {
		return nil
	}
	if err := x.kv.Delete(ctx, nameKey(name)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("release account name: %w", err)
	}
	return nil
}

// Lookup returns the account ID indexed against name. found is false for both
// an absent entry and an uncommitted reservation, since neither names an
// account that exists.
func (x *AccountNameIndex) Lookup(ctx context.Context, name string) (accountID string, found bool, err error) {
	existing, err := x.get(ctx, name)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if existing.AccountID == "" {
		return "", false, nil
	}
	return existing.AccountID, true, nil
}

func (x *AccountNameIndex) get(ctx context.Context, name string) (nameReservation, error) {
	entry, err := x.kv.Get(ctx, nameKey(name))
	if err != nil {
		return nameReservation{}, err
	}
	var existing nameReservation
	if err := json.Unmarshal(entry.Value(), &existing); err != nil {
		return nameReservation{}, fmt.Errorf("unmarshal reservation: %w", err)
	}
	return existing, nil
}

// FindAccountByName scans existing accounts for a normalized name match. The
// reservation index covers accounts created through it; this covers the ones
// that predate it, which have no reservation to find. Cost is one KV scan,
// bounded in practice by the self-serve account cap.
func FindAccountByName(svc IAMService, name string) (*Account, error) {
	accounts, err := svc.ListAccounts()
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	want := NormalizeAccountName(name)
	for _, account := range accounts {
		if account != nil && NormalizeAccountName(account.AccountName) == want {
			return account, nil
		}
	}
	return nil, nil
}
