// Package store keeps migration records in one JetStream KV bucket per
// account. Every write is a compare-and-swap on the entry revision.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	migrationv1 "github.com/mulgadc/spinifex/contracts/migration/v1"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// BucketPrefix starts the name of every per-account migration bucket.
const BucketPrefix = "spinifex-migration-"

// SchemaVersion is the record envelope version this build writes.
const SchemaVersion = 1

// casAttempts bounds one read-modify-write loop against concurrent writers.
const casAttempts = 8

var (
	// ErrNotFound is a record or bucket that does not exist.
	ErrNotFound = errors.New("migration record not found")
	// ErrDeleteRequested refuses new work on a record marked for deletion.
	ErrDeleteRequested = errors.New("migration record is marked for deletion")
	// ErrClaimed refuses a claim of a record another node owns.
	ErrClaimed = errors.New("migration record is claimed by another node")
	// ErrConflict is a compare-and-swap that kept losing its race.
	ErrConflict = errors.New("migration record changed concurrently")
	// ErrNewerSchema is a record written by a newer build; this build never rewrites it.
	ErrNewerSchema = errors.New("migration record has a newer schema version")
	// ErrMismatch is a record whose kind, ID or account differs from its key.
	ErrMismatch = errors.New("migration record does not match its key")
)

// Kinds are the record kinds, one per participant class.
func Kinds() []string {
	return migrationv1.Declaration().Classes
}

// Owner is the executor that has claimed a record's job. Stopped is set by
// that executor once it makes no further write for the record.
type Owner struct {
	Node    string `json:"node"`
	Stopped bool   `json:"stopped,omitempty"`
}

// Record is the envelope every migration record shares. Body holds the
// kind-specific fields and is preserved byte for byte by envelope writes.
type Record struct {
	SchemaVersion     int             `json:"schema_version"`
	Kind              string          `json:"kind"`
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	DeleteRequested   bool            `json:"delete_requested,omitempty"`
	DeleteRequestedAt *time.Time      `json:"delete_requested_at,omitempty"`
	Owner             *Owner          `json:"owner,omitempty"`
	Body              json.RawMessage `json:"body,omitempty"`
}

// Key is the record's key in its account bucket.
func (r *Record) Key() string { return Key(r.Kind, r.ID) }

// Entry is one record read with its revision.
type Entry struct {
	Record   Record
	Revision uint64
}

// Key builds the record key `<kind>.<id>`.
func Key(kind, id string) string { return kind + "." + id }

// BucketName is the account's migration bucket.
func BucketName(accountID string) string { return BucketPrefix + accountID }

// ValidKind reports whether kind is a declared record kind.
func ValidKind(kind string) bool {
	return slices.Contains(Kinds(), kind)
}

// ValidID reports whether id is one KV key token of letters, digits and '-'.
func ValidID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func validAccount(accountID string) error {
	if !awsidentifiers.IsAccountID(accountID) {
		return fmt.Errorf("migration store: %q is not an account ID", accountID)
	}
	return nil
}

// Store is one account's migration bucket.
type Store struct {
	accountID string
	kv        jetstream.KeyValue
}

// Open attaches to the account's bucket. A missing bucket is ErrNotFound and
// is never created here, so a reader cannot bring a deleted bucket back.
func Open(ctx context.Context, js jetstream.KeyValueManager, accountID string) (*Store, error) {
	if err := validAccount(accountID); err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(ctx, BucketName(accountID))
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open migration bucket: %w", err)
	}
	return &Store{accountID: accountID, kv: kv}, nil
}

// OpenOrCreate attaches to the account's bucket, creating it when absent. Only
// a create path that has passed admission calls it.
func OpenOrCreate(ctx context.Context, js jetstream.KeyValueManager, accountID string) (*Store, error) {
	if err := validAccount(accountID); err != nil {
		return nil, err
	}
	kv, err := kvstore.GetOrCreateBucket(ctx, js, BucketName(accountID), 1)
	if err != nil {
		return nil, fmt.Errorf("open migration bucket: %w", err)
	}
	return &Store{accountID: accountID, kv: kv}, nil
}

// DeleteBucket removes the account's bucket. An absent bucket is success.
func DeleteBucket(ctx context.Context, js jetstream.KeyValueManager, accountID string) error {
	if err := validAccount(accountID); err != nil {
		return err
	}
	return kvstore.DeleteBucketIfExists(ctx, js, BucketName(accountID))
}

// AccountIDs lists every account that has a migration bucket. A failed
// listing is an error, never a shorter list.
func AccountIDs(ctx context.Context, js jetstream.KeyValueManager) ([]string, error) {
	names, err := kvstore.BucketNames(ctx, js)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if id, ok := strings.CutPrefix(name, BucketPrefix); ok && awsidentifiers.IsAccountID(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// AccountID is the account the store belongs to.
func (s *Store) AccountID() string { return s.accountID }

// Create writes a new record. A record that already exists is a conflict.
func (s *Store) Create(ctx context.Context, rec Record) (uint64, error) {
	if err := s.check(&rec); err != nil {
		return 0, err
	}
	rec.SchemaVersion = SchemaVersion
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode migration record: %w", err)
	}
	rev, err := s.kv.Create(ctx, rec.Key(), data)
	if kvstore.IsRevisionConflict(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("create migration record: %w", err)
	}
	return rev, nil
}

// Get reads one record.
func (s *Store) Get(ctx context.Context, kind, id string) (Entry, error) {
	entry, err := s.kv.Get(ctx, Key(kind, id))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read migration record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return Entry{}, fmt.Errorf("decode migration record: %w", err)
	}
	if rec.SchemaVersion > SchemaVersion {
		return Entry{}, ErrNewerSchema
	}
	if rec.Kind != kind || rec.ID != id || rec.AccountID != s.accountID {
		return Entry{}, fmt.Errorf("%w: key %s holds %s.%s of account %q", ErrMismatch, Key(kind, id), rec.Kind, rec.ID, rec.AccountID)
	}
	return Entry{Record: rec, Revision: entry.Revision()}, nil
}

// Listing is every record of the bucket. Unreadable counts keys whose value
// could not be read or decoded, per kind; any count there makes the scan
// incomplete. Unknown counts keys of no declared kind.
type Listing struct {
	Entries    []Entry
	Unreadable map[string]int
	Unknown    int
}

// List reads every record. A failed key listing is an error.
func (s *Store) List(ctx context.Context) (Listing, error) {
	keys, err := kvstore.Keys(ctx, s.kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return Listing{Unreadable: map[string]int{}}, nil
	}
	if err != nil {
		return Listing{}, fmt.Errorf("list migration records: %w", err)
	}
	out := Listing{Unreadable: map[string]int{}}
	for _, key := range keys {
		kind, id, ok := strings.Cut(key, ".")
		if !ok || !ValidKind(kind) {
			out.Unknown++
			continue
		}
		e, err := s.Get(ctx, kind, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			out.Unreadable[kind]++
			continue
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// UnreadableCount is the number of keys the listing could not read.
func (l Listing) UnreadableCount() int {
	n := 0
	for _, c := range l.Unreadable {
		n += c
	}
	return n
}

// MarkDeleteRequested sets DeleteRequested on a record. It is idempotent and
// keeps the first mark time. A record that is already gone is ErrNotFound.
func (s *Store) MarkDeleteRequested(ctx context.Context, kind, id string, now time.Time) (Entry, error) {
	return s.mutate(ctx, kind, id, func(r *Record) (bool, error) {
		if r.DeleteRequested {
			return false, nil
		}
		t := now.UTC()
		r.DeleteRequested, r.DeleteRequestedAt = true, &t
		return true, nil
	})
}

// Claim records node as the record's executor. It refuses a record marked for
// deletion and a record another node holds unstopped.
func (s *Store) Claim(ctx context.Context, kind, id, node string) (Entry, error) {
	return s.mutate(ctx, kind, id, func(r *Record) (bool, error) {
		if r.DeleteRequested {
			return false, ErrDeleteRequested
		}
		if r.Owner != nil && r.Owner.Node != node && !r.Owner.Stopped {
			return false, ErrClaimed
		}
		if r.Owner != nil && r.Owner.Node == node && !r.Owner.Stopped {
			return false, nil
		}
		r.Owner = &Owner{Node: node}
		return true, nil
	})
}

// RecordStopped records that node, the record's executor, makes no further
// write for it. A record owned by another node is left unchanged.
func (s *Store) RecordStopped(ctx context.Context, kind, id, node string) (Entry, error) {
	return s.mutate(ctx, kind, id, func(r *Record) (bool, error) {
		if r.Owner == nil || r.Owner.Node != node || r.Owner.Stopped {
			return false, nil
		}
		r.Owner.Stopped = true
		return true, nil
	})
}

// Delete removes a record at revision rev. A record that changed since it was
// read is ErrConflict; one that is already gone is success.
func (s *Store) Delete(ctx context.Context, kind, id string, rev uint64) error {
	err := s.kv.Delete(ctx, Key(kind, id), jetstream.LastRevision(rev))
	if err == nil || errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if kvstore.IsRevisionConflict(err) {
		return ErrConflict
	}
	return fmt.Errorf("delete migration record: %w", err)
}

func (s *Store) check(rec *Record) error {
	if !ValidKind(rec.Kind) {
		return fmt.Errorf("migration store: unknown kind %q", rec.Kind)
	}
	if !ValidID(rec.ID) {
		return fmt.Errorf("migration store: invalid id %q", rec.ID)
	}
	if rec.AccountID != s.accountID {
		return fmt.Errorf("migration store: record names account %q, bucket is %q", rec.AccountID, s.accountID)
	}
	return nil
}

func (s *Store) mutate(ctx context.Context, kind, id string, f func(*Record) (bool, error)) (Entry, error) {
	for range casAttempts {
		e, err := s.Get(ctx, kind, id)
		if err != nil {
			return Entry{}, err
		}
		changed, err := f(&e.Record)
		if err != nil || !changed {
			return e, err
		}
		data, err := json.Marshal(e.Record)
		if err != nil {
			return Entry{}, fmt.Errorf("encode migration record: %w", err)
		}
		rev, err := s.kv.Update(ctx, Key(kind, id), data, e.Revision)
		if err == nil {
			return Entry{Record: e.Record, Revision: rev}, nil
		}
		if !kvstore.IsRevisionConflict(err) {
			return Entry{}, fmt.Errorf("update migration record: %w", err)
		}
		if ctx.Err() != nil {
			return Entry{}, ctx.Err()
		}
	}
	return Entry{}, ErrConflict
}

// CountAll counts every migration record across the cluster. An empty bucket
// holds nothing to drain and counts as none. A failed read is an error, never
// a lower count.
func CountAll(ctx context.Context, js jetstream.KeyValueManager) (int, error) {
	accounts, err := AccountIDs(ctx, js)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, account := range accounts {
		s, err := Open(ctx, js, account)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		l, err := s.List(ctx)
		if err != nil {
			return 0, err
		}
		total += len(l.Entries) + l.Unknown + l.UnreadableCount()
	}
	return total, nil
}
