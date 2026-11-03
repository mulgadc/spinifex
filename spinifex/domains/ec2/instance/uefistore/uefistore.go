// Package uefistore holds the types that describe an instance's UEFI variable
// store and the bucket of store records that carry its deletion and write-back
// obligations past the life of the instance record.
package uefistore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// BucketName holds one store record per instance, keyed by instance ID.
	BucketName = "spinifex-uefi-stores"
	// BucketVersion is the bucket's schema stamp.
	BucketVersion = 1
	// RecordSchemaVersion is the version every record is written at.
	RecordSchemaVersion uint16 = 1

	// StateLive is a store whose objects belong to a running or stopped instance.
	StateLive = "live"
	// StateDeleting is a store whose objects are to be deleted, and then the record.
	StateDeleting = "deleting"

	objectRoot = "uefi/"
)

// ErrInvalidRecord is returned for a store record that breaks its invariants,
// whether it is about to be written or was just read.
var ErrInvalidRecord = errors.New("uefistore: invalid store record")

// FirmwareIdentity names the firmware a variable store was written under. A
// store only boots where Arch and VarsSize match the node's firmware, because
// pflash needs a byte-exact region; CodeSHA256 is recorded, not enforced.
type FirmwareIdentity struct {
	Arch       string `json:"arch"`
	VarsSize   int64  `json:"vars_size"`
	CodeSHA256 string `json:"code_sha256"`
}

// UEFIStoreRecord is the durable owner of one instance's variable store
// objects. It outlives the instance record, so an obligation recorded here is
// not lost when a terminated record expires.
type UEFIStoreRecord struct {
	SchemaVersion uint16         `json:"schema_version"`
	InstanceID    string         `json:"instance_id"`
	AccountID     string         `json:"account_id"`
	State         string         `json:"state"`
	WriteBack     *UEFIWriteBack `json:"write_back,omitempty"`
}

// UEFIWriteBack is a write-back that failed and is still owed. Key is the
// object every retry puts to, so retries leave no extra objects; SeededFrom is
// the pointer Node seeded from, which a retry must still find on the record.
type UEFIWriteBack struct {
	Node       string    `json:"node"`
	Key        string    `json:"key"`
	SeededFrom string    `json:"seeded_from"`
	Since      time.Time `json:"since"`
}

// KeyPrefix is the object-store prefix every store object of ownerID (an
// instance or image ID) is written under.
func KeyPrefix(ownerID string) string {
	return objectRoot + ownerID + "/"
}

// ownsKey reports whether key names one object directly under ownerID's prefix.
func ownsKey(ownerID, key string) bool {
	name, ok := strings.CutPrefix(key, KeyPrefix(ownerID))
	return ok && name != "" && !strings.Contains(name, "/")
}

// validate checks rec against key. A write-back key outside the instance's own
// prefix is refused because a superseded obligation deletes that key.
func (rec *UEFIStoreRecord) validate(key string) error {
	switch {
	case rec.SchemaVersion > RecordSchemaVersion:
		return fmt.Errorf("%w: %s is at schema_version %d, newer than this node understands (%d)",
			ErrInvalidRecord, key, rec.SchemaVersion, RecordSchemaVersion)
	case rec.SchemaVersion != RecordSchemaVersion:
		return fmt.Errorf("%w: %s has schema_version %d", ErrInvalidRecord, key, rec.SchemaVersion)
	case !strings.HasPrefix(rec.InstanceID, "i-"):
		return fmt.Errorf("%w: instance_id %q is not an instance ID", ErrInvalidRecord, rec.InstanceID)
	case rec.InstanceID != key:
		return fmt.Errorf("%w: instance_id %s stored at key %s", ErrInvalidRecord, rec.InstanceID, key)
	case rec.State != StateLive && rec.State != StateDeleting:
		return fmt.Errorf("%w: %s has state %q", ErrInvalidRecord, key, rec.State)
	}
	if wb := rec.WriteBack; wb != nil {
		switch {
		case wb.Node == "":
			return fmt.Errorf("%w: %s write-back names no node", ErrInvalidRecord, key)
		case wb.Since.IsZero():
			return fmt.Errorf("%w: %s write-back has no since", ErrInvalidRecord, key)
		case !ownsKey(rec.InstanceID, wb.Key):
			return fmt.Errorf("%w: %s write-back key %q is not under %s", ErrInvalidRecord, key, wb.Key, KeyPrefix(rec.InstanceID))
		case !ownsKey(rec.InstanceID, wb.SeededFrom):
			return fmt.Errorf("%w: %s write-back seeded_from %q is not under %s", ErrInvalidRecord, key, wb.SeededFrom, KeyPrefix(rec.InstanceID))
		}
	}
	return nil
}

func bucketConfig() kvstore.Config {
	return kvstore.Config{
		Name:        BucketName,
		Description: "Spinifex UEFI variable store records (deletion and write-back obligations)",
		History:     1,
		// Left false: a lost bucket has lost obligations, and an empty
		// recreated one would report every store as having none.
		RecreateIfMissing: false,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, BucketName, kv, BucketVersion)
		},
		Missing: "UEFI store KV bucket not initialized",
	}
}

// Store reads and writes store records. Every change is guarded by the
// revision the caller read, so a lost race is reported rather than retried.
type Store struct {
	kv *kvstore.Store[UEFIStoreRecord]
}

// NewStore returns a Store over js. The bucket is created on Open or first use.
func NewStore(js jetstream.JetStream) *Store {
	return &Store{kv: kvstore.New[UEFIStoreRecord](js, bucketConfig())}
}

// Open creates the bucket if it is missing and stamps its schema version.
func (s *Store) Open(ctx context.Context) error {
	_, err := s.kv.KV(ctx)
	return err
}

// Get returns instanceID's record and the revision it was read at. A missing
// record is kvstore.ErrNotFound; one that breaks its invariants is
// ErrInvalidRecord, so a newer or corrupt record is never acted on.
func (s *Store) Get(ctx context.Context, instanceID string) (*UEFIStoreRecord, uint64, error) {
	rec, rev, err := s.kv.Get(ctx, instanceID)
	if err != nil {
		return nil, 0, err
	}
	if err := rec.validate(instanceID); err != nil {
		return nil, 0, err
	}
	return rec, rev, nil
}

// Create writes a new record, which must be live: a store is recorded before
// its first object is written. An existing record is kvstore.ErrExists. Every
// write stamps rec with RecordSchemaVersion.
func (s *Store) Create(ctx context.Context, rec *UEFIStoreRecord) (uint64, error) {
	rec.SchemaVersion = RecordSchemaVersion
	if rec.State != StateLive {
		return 0, fmt.Errorf("%w: %s created in state %q, not %q", ErrInvalidRecord, rec.InstanceID, rec.State, StateLive)
	}
	if err := rec.validate(rec.InstanceID); err != nil {
		return 0, err
	}
	return s.kv.Create(ctx, rec.InstanceID, rec)
}

// CompareAndSet replaces the record only if it is still at rev, returning
// kvstore.ErrConflict when it is not, and the revision written otherwise.
func (s *Store) CompareAndSet(ctx context.Context, rec *UEFIStoreRecord, rev uint64) (uint64, error) {
	rec.SchemaVersion = RecordSchemaVersion
	if err := rec.validate(rec.InstanceID); err != nil {
		return 0, err
	}
	return s.kv.CompareAndSet(ctx, rec.InstanceID, rec, rev)
}

// CompareAndDelete removes instanceID's record only if it is still at rev,
// returning kvstore.ErrConflict when it is not.
func (s *Store) CompareAndDelete(ctx context.Context, instanceID string, rev uint64) error {
	return s.kv.CompareAndDelete(ctx, instanceID, rev)
}
