// Package uefistore holds the types that describe an instance's UEFI variable
// store, the bucket of store records that carry its pointer and deletion
// obligation past the life of the instance record, and the claim that fences it.
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

	// ObjectRoot is the object-store prefix every store object is under.
	ObjectRoot = "uefi/"
)

// ErrInvalidRecord is returned for a store record that breaks its invariants,
// whether it is about to be written or was just read.
var ErrInvalidRecord = errors.New("uefistore: invalid store record")

// ErrDeleting is returned for a change that would return a deleting store to
// live. Deletion follows only a decided terminate, which is final, so a writer
// that meets it drops whatever it was about to record.
var ErrDeleting = errors.New("uefistore: store is being deleted")

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
//
// Pointer names the only version of the store the record keeps, the object the
// instance boots from, and only a writer whose claim token has generation
// Generation may move it.
type UEFIStoreRecord struct {
	SchemaVersion uint16           `json:"schema_version"`
	InstanceID    string           `json:"instance_id"`
	AccountID     string           `json:"account_id"`
	State         string           `json:"state"`
	Generation    uint64           `json:"generation"`
	Pointer       *UEFIVarStoreRef `json:"pointer,omitempty"`
}

// UEFIVarStoreRef names the store object an instance launches from, with the
// firmware it was written under, so a launch can check it without reading the
// object's metadata.
type UEFIVarStoreRef struct {
	Key        string           `json:"key"`
	Firmware   FirmwareIdentity `json:"firmware"`
	WriterNode string           `json:"writer_node"`
	WrittenAt  time.Time        `json:"written_at"`
}

// KeyPrefix is the object-store prefix every store object of ownerID (an
// instance or image ID) is written under.
func KeyPrefix(ownerID string) string {
	return ObjectRoot + ownerID + "/"
}

// ownsKey reports whether key names one object directly under ownerID's prefix.
func ownsKey(ownerID, key string) bool {
	name, ok := strings.CutPrefix(key, KeyPrefix(ownerID))
	return ok && name != "" && !strings.Contains(name, "/")
}

// validate checks rec against key. The pointer must name one object directly
// under the instance's own prefix, because a deletion acts on it.
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
	if p := rec.Pointer; p != nil {
		if err := p.validate(rec.InstanceID); err != nil {
			return fmt.Errorf("%w: %s pointer: %w", ErrInvalidRecord, key, err)
		}
	}
	return nil
}

// validate checks ref as a pointer of instanceID's store.
func (ref *UEFIVarStoreRef) validate(instanceID string) error {
	switch {
	case !ownsKey(instanceID, ref.Key):
		return fmt.Errorf("key %q is not under %s", ref.Key, KeyPrefix(instanceID))
	case ref.Firmware.Arch == "" || ref.Firmware.VarsSize <= 0:
		return fmt.Errorf("key %q names no firmware arch and VARS size", ref.Key)
	case ref.WriterNode == "":
		return fmt.Errorf("key %q names no writer node", ref.Key)
	}
	return nil
}

func bucketConfig() kvstore.Config {
	return kvstore.Config{
		Name:        BucketName,
		Description: "Spinifex UEFI variable store records (pointers and deletion obligations)",
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

// NewReader returns a Store over the existing bucket that neither creates it
// nor migrates it, for an operator report that must not change the cluster.
func NewReader(js jetstream.JetStream) *Store {
	cfg := bucketConfig()
	cfg.AttachOnly = true
	cfg.OnOpen = nil
	return &Store{kv: kvstore.New[UEFIStoreRecord](js, cfg)}
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
// kvstore.ErrConflict when it is not, and the revision written otherwise. A
// record at rev that is deleting cannot be set live again: that is ErrDeleting.
func (s *Store) CompareAndSet(ctx context.Context, rec *UEFIStoreRecord, rev uint64) (uint64, error) {
	rec.SchemaVersion = RecordSchemaVersion
	if err := rec.validate(rec.InstanceID); err != nil {
		return 0, err
	}
	if rec.State == StateLive {
		// Read unvalidated: only the state at rev matters, and any other
		// revision fails the guarded write below anyway.
		cur, curRev, err := s.kv.Get(ctx, rec.InstanceID)
		if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
			return 0, err
		}
		if err == nil && curRev == rev && cur.State == StateDeleting {
			return 0, fmt.Errorf("%w: %s", ErrDeleting, rec.InstanceID)
		}
	}
	return s.kv.CompareAndSet(ctx, rec.InstanceID, rec, rev)
}

// CompareAndDelete removes instanceID's record only if it is still at rev,
// returning kvstore.ErrConflict when it is not.
func (s *Store) CompareAndDelete(ctx context.Context, instanceID string, rev uint64) error {
	return s.kv.CompareAndDelete(ctx, instanceID, rev)
}
