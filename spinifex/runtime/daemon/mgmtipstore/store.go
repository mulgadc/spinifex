// Package mgmtipstore holds the durable per-subnet management IP record: one
// value per /24 carrying the schema major, the authority state and the
// allocations, so every operation is a compare-and-swap on a single key.
//
// The bucket's stream configuration is checked against the durability config
// contract (no expiry or eviction) before every operation, and the stream's
// creation time is bound at Bind. The remaining assumption is that the bucket
// is not deleted, recreated or rolled back between that check and the write;
// a recreate inside that window is not caught. Uniqueness of addresses and
// holders holds within one subnet record only; overlap across subnets is out
// of scope.
package mgmtipstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// SchemaMajor is the only record schema this code reads or writes.
const SchemaMajor = 1

// Host bytes handed out within a /24.
const (
	rangeMin = 10
	rangeMax = 249
)

// Authority says whether the record may decide allocations.
type Authority string

const (
	AuthorityFormed   Authority = "formed"
	AuthorityMigrated Authority = "migrated"
)

var (
	ErrNotAuthoritative  = errors.New("mgmtipstore: record is not authoritative")
	ErrUnsupportedSchema = errors.New("mgmtipstore: unsupported record schema")
	ErrUnknownField      = errors.New("mgmtipstore: record has a field this binary does not know")
	ErrNoncanonical      = errors.New("mgmtipstore: record is not in canonical encoding")
	ErrInvalidRecord     = errors.New("mgmtipstore: record content is invalid")
	ErrInvalidArgument   = errors.New("mgmtipstore: invalid argument")
	ErrStale             = errors.New("mgmtipstore: read is not current")
	ErrConflict          = errors.New("mgmtipstore: record changed since it was read")
	ErrNotFound          = errors.New("mgmtipstore: no such entry")
	ErrAlreadyHeld       = errors.New("mgmtipstore: instance already holds an address")
	ErrRangeExhausted    = errors.New("mgmtipstore: address range exhausted")
	ErrForeignSnapshot   = errors.New("mgmtipstore: snapshot does not belong to this store")
	ErrNoCreationTime    = errors.New("mgmtipstore: stream reports no creation time")
	ErrIncarnation       = errors.New("mgmtipstore: bucket was recreated since it was bound")
)

// Entry is one allocated management address.
type Entry struct {
	IP         string `json:"ip"`
	InstanceID string `json:"instance_id"`
	Node       string `json:"node"`
}

// Record is the value stored under a subnet key.
type Record struct {
	Schema    int       `json:"schema"`
	Authority Authority `json:"authority"`
	Subnet    string    `json:"subnet"`
	Entries   []Entry   `json:"entries"`
}

// Snapshot is a validated record read at one revision. It is opaque: a write
// derives its value from the captured bytes, never from caller-held fields.
type Snapshot struct {
	owner    *Store
	bucket   string
	created  time.Time
	prefix   netip.Prefix
	revision uint64
	raw      []byte
}

// Record returns a copy of the record the snapshot captured.
func (s *Snapshot) Record() Record {
	rec, err := decode(s.raw, s.prefix)
	if err != nil {
		return Record{}
	}
	return rec
}

// Revision is the key revision the snapshot was read at.
func (s *Snapshot) Revision() uint64 { return s.revision }

// Store reads and writes subnet records in an existing durable bucket.
type Store struct {
	kv      jetstream.KeyValue
	bucket  string
	created time.Time
	stat    func(ctx context.Context) (*jetstream.StreamInfo, error)
}

// Bind attaches to an existing bucket and verifies it. An absent bucket is an
// error and is never created.
func Bind(ctx context.Context, js jetstream.KeyValueManager, bucket string) (*Store, error) {
	kv, err := kvutil.BindDurable(ctx, js, bucket)
	if err != nil {
		return nil, err
	}
	return attach(ctx, kv, bucket)
}

// Create provisions the bucket and binds it. Only formation calls this; Bind
// never does.
func Create(ctx context.Context, js jetstream.JetStream, bucket, description string) (*Store, error) {
	kv, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{
		Name: bucket, Description: description, History: 1,
	})
	if err != nil {
		return nil, err
	}
	return attach(ctx, kv, bucket)
}

func attach(ctx context.Context, kv jetstream.KeyValue, bucket string) (*Store, error) {
	return newStoreWithStat(ctx, kv, bucket, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		return kvutil.VerifyDurableInfo(ctx, kv, bucket)
	})
}

func newStoreWithStat(ctx context.Context, kv jetstream.KeyValue, bucket string, stat func(context.Context) (*jetstream.StreamInfo, error)) (*Store, error) {
	s := &Store{kv: kv, bucket: bucket, stat: stat}
	info, err := s.inspect(ctx)
	if err != nil {
		return nil, err
	}
	s.created = info.Created
	return s, nil
}

// inspect re-reads the stream and refuses one that fails the durability config
// contract or reports no creation time, since a zero time proves no identity.
func (s *Store) inspect(ctx context.Context) (*jetstream.StreamInfo, error) {
	info, err := s.stat(ctx)
	if err != nil {
		return nil, err
	}
	if info.Created.IsZero() {
		return nil, fmt.Errorf("%w: bucket %s", ErrNoCreationTime, s.bucket)
	}
	return info, nil
}

// check re-reads the stream configuration, because binding does not freeze it,
// and refuses when the stream is no longer the one that was bound.
func (s *Store) check(ctx context.Context) error {
	info, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if !info.Created.Equal(s.created) {
		return fmt.Errorf("%w: bucket %s", ErrIncarnation, s.bucket)
	}
	return nil
}

// validPrefix requires a canonical (already masked) IPv4 /24.
func validPrefix(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() != 24 || p.Masked() != p {
		return fmt.Errorf("%w: subnet %v is not a canonical IPv4 /24", ErrInvalidArgument, p)
	}
	return nil
}

func keyOf(p netip.Prefix) string { return p.String() }

// Load reads and validates a subnet's record. A missing key, or a record with
// a missing or unsupported schema or authority state, is not authoritative.
func (s *Store) Load(ctx context.Context, prefix netip.Prefix) (*Snapshot, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	key := keyOf(prefix)
	entry, err := s.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("%w: subnet %s has no record", ErrNotAuthoritative, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read subnet %s: %w", key, err)
	}
	if _, err := decode(entry.Value(), prefix); err != nil {
		return nil, fmt.Errorf("subnet %s: %w", key, err)
	}
	return &Snapshot{
		owner: s, bucket: s.bucket, created: s.created, prefix: prefix,
		revision: entry.Revision(), raw: slices.Clone(entry.Value()),
	}, nil
}

// encode is the one canonical encoding the store writes and requires.
func encode(rec Record) ([]byte, error) {
	if rec.Entries == nil {
		rec.Entries = []Entry{}
	}
	return json.Marshal(rec)
}

func decode(raw []byte, prefix netip.Prefix) (Record, error) {
	var head struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrNotAuthoritative, err)
	}
	if head.Schema != SchemaMajor {
		return Record{}, fmt.Errorf("%w: schema %d, want %d", ErrUnsupportedSchema, head.Schema, SchemaMajor)
	}
	var rec Record
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return Record{}, fmt.Errorf("%w: %w", ErrUnknownField, err)
		}
		return Record{}, fmt.Errorf("%w: %w", ErrNotAuthoritative, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: trailing data", ErrNotAuthoritative)
	}
	if rec.Authority != AuthorityFormed && rec.Authority != AuthorityMigrated {
		return Record{}, fmt.Errorf("%w: authority state %q", ErrNotAuthoritative, rec.Authority)
	}
	if err := validateContent(rec, prefix); err != nil {
		return Record{}, err
	}
	// Duplicate keys, case aliases, reordering and whitespace all decode to a
	// record the store would then rewrite differently, so only its own bytes pass.
	canon, err := encode(rec)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if !bytes.Equal(canon, raw) {
		return Record{}, ErrNoncanonical
	}
	return rec, nil
}

func validateContent(rec Record, prefix netip.Prefix) error {
	if rec.Subnet != keyOf(prefix) {
		return fmt.Errorf("%w: record names subnet %q, key is %q", ErrInvalidRecord, rec.Subnet, keyOf(prefix))
	}
	ips := make(map[string]struct{}, len(rec.Entries))
	holders := make(map[string]struct{}, len(rec.Entries))
	for _, e := range rec.Entries {
		if e.InstanceID == "" || e.Node == "" {
			return fmt.Errorf("%w: entry %q has no instance or node", ErrInvalidRecord, e.IP)
		}
		if err := canonicalIPIn(e.IP, prefix); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
		}
		if _, dup := ips[e.IP]; dup {
			return fmt.Errorf("%w: address %s held twice", ErrInvalidRecord, e.IP)
		}
		if _, dup := holders[e.InstanceID]; dup {
			return fmt.Errorf("%w: instance %s holds two addresses", ErrInvalidRecord, e.InstanceID)
		}
		ips[e.IP] = struct{}{}
		holders[e.InstanceID] = struct{}{}
	}
	return nil
}

func canonicalIPIn(ip string, prefix netip.Prefix) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() || addr.String() != ip {
		return fmt.Errorf("address %q is not canonical IPv4", ip)
	}
	if !prefix.Contains(addr) {
		return fmt.Errorf("address %s is outside %s", ip, prefix)
	}
	return nil
}

// owns reports whether this store produced the snapshot and the bucket is still
// the incarnation it was read from, and returns its record re-validated from the
// captured bytes. Creation time is not a global epoch, so identity is the store.
func (s *Store) owns(snap *Snapshot) (Record, error) {
	if snap == nil || snap.raw == nil || snap.owner != s || snap.bucket != s.bucket || !snap.created.Equal(s.created) {
		return Record{}, ErrForeignSnapshot
	}
	if err := validPrefix(snap.prefix); err != nil {
		return Record{}, ErrForeignSnapshot
	}
	rec, err := decode(snap.raw, snap.prefix)
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// Confirm proves a snapshot is current with an acknowledged revision-conditional
// write of the same bytes, because a plain read can be served by a lagging
// follower. A conflict means the read was stale.
func (s *Store) Confirm(ctx context.Context, snap *Snapshot) error {
	if _, err := s.owns(snap); err != nil {
		return err
	}
	if err := s.check(ctx); err != nil {
		return err
	}
	key := keyOf(snap.prefix)
	if _, err := s.kv.Update(ctx, key, snap.raw, snap.revision); err != nil {
		if kvutil.IsRevisionConflict(err) {
			return fmt.Errorf("%w: subnet %s revision %d", ErrStale, key, snap.revision)
		}
		return fmt.Errorf("confirm subnet %s: %w", key, err)
	}
	return nil
}

// Lookup returns the entry matching instance, node and address, only after the
// read has been confirmed current.
func (s *Store) Lookup(ctx context.Context, prefix netip.Prefix, instanceID, node, ip string) (Entry, error) {
	if instanceID == "" || node == "" {
		return Entry{}, fmt.Errorf("%w: instance and node are required", ErrInvalidArgument)
	}
	if err := canonicalIPIn(ip, prefix); err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	}
	snap, err := s.Load(ctx, prefix)
	if err != nil {
		return Entry{}, err
	}
	if err := s.Confirm(ctx, snap); err != nil {
		return Entry{}, err
	}
	for _, e := range snap.Record().Entries {
		if e.InstanceID == instanceID && e.Node == node && e.IP == ip {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%w: instance %s on %s at %s", ErrNotFound, instanceID, node, ip)
}

// Insert adds the first unused address in the subnet's host range, conditional
// on the snapshot's revision. An instance that already holds an address is
// refused. A changed record yields ErrConflict; the caller may reload and retry.
func (s *Store) Insert(ctx context.Context, snap *Snapshot, instanceID, node string) (Entry, error) {
	if instanceID == "" || node == "" {
		return Entry{}, fmt.Errorf("%w: instance and node are required", ErrInvalidArgument)
	}
	rec, err := s.owns(snap)
	if err != nil {
		return Entry{}, err
	}
	if err := s.check(ctx); err != nil {
		return Entry{}, err
	}
	for _, e := range rec.Entries {
		if e.InstanceID == instanceID {
			return Entry{}, fmt.Errorf("%w: %s holds %s", ErrAlreadyHeld, instanceID, e.IP)
		}
	}
	ip, err := firstFree(snap.prefix, rec.Entries)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{IP: ip, InstanceID: instanceID, Node: node}
	rec.Entries = append(slices.Clone(rec.Entries), entry)
	raw, err := encode(rec)
	if err != nil {
		return Entry{}, fmt.Errorf("encode subnet %s: %w", rec.Subnet, err)
	}
	if _, err := s.kv.Update(ctx, rec.Subnet, raw, snap.revision); err != nil {
		if kvutil.IsRevisionConflict(err) {
			return Entry{}, fmt.Errorf("%w: subnet %s revision %d", ErrConflict, rec.Subnet, snap.revision)
		}
		return Entry{}, fmt.Errorf("insert into subnet %s: %w", rec.Subnet, err)
	}
	return entry, nil
}

func firstFree(prefix netip.Prefix, entries []Entry) (string, error) {
	used := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		used[e.IP] = struct{}{}
	}
	b := prefix.Addr().As4()
	for i := byte(rangeMin); i <= rangeMax; i++ {
		b[3] = i
		candidate := netip.AddrFrom4(b).String()
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w (%s host %d-%d)", ErrRangeExhausted, prefix, rangeMin, rangeMax)
}
