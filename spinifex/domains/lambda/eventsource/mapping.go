// Package eventsource holds the Lambda event-source mapping record and its
// JetStream KV store.
package eventsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// FormatVersion is the only record layout this build reads or writes.
const FormatVersion = 1

// KVBucket holds every mapping, keyed by UUID; the UUID is global in AWS.
const KVBucket = "lambda-event-source-mappings"

// Mapping states this build records. A mapping is Enabled from creation and
// Deleting from the delete request until its poller has stopped.
const (
	StateEnabled  = "Enabled"
	StateDeleting = "Deleting"
)

// ReasonUserInitiated is the transition reason of a state the caller asked for.
const ReasonUserInitiated = "USER_INITIATED"

// ErrNotFound is returned for a UUID with no mapping.
var ErrNotFound = errors.New("eventsource: mapping not found")

// ErrConflict is returned when the record changed since it was read.
var ErrConflict = errors.New("eventsource: mapping changed concurrently")

// Mapping is one SQS event-source mapping.
type Mapping struct {
	FormatVersion  int    `json:"formatVersion"`
	UUID           string `json:"uuid"`
	AccountID      string `json:"accountId"`
	Region         string `json:"region"`
	FunctionName   string `json:"functionName"`
	FunctionARN    string `json:"functionArn"`
	EventSourceARN string `json:"eventSourceArn"`
	BatchSize      int64  `json:"batchSize"`
	State          string `json:"state"`
	// StateTransitionReason is USER_INITIATED, or the last receive failure
	// prefixed "PROBLEM: " while polling cannot proceed.
	StateTransitionReason string    `json:"stateTransitionReason"`
	LastModified          time.Time `json:"lastModified"`
}

// Store reads and writes mappings in KVBucket.
type Store struct{ kv jetstream.KeyValue }

// Open returns the store, creating its bucket on first use.
func Open(ctx context.Context, js jetstream.KeyValueManager) (*Store, error) {
	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucket, 1)
	if err != nil {
		return nil, fmt.Errorf("eventsource: open bucket: %w", err)
	}
	return &Store{kv: kv}, nil
}

func key(uuid string) string { return "esm." + uuid }

// Create writes a new mapping; a UUID already present is a conflict.
func (s *Store) Create(ctx context.Context, m Mapping) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := s.kv.Create(ctx, key(m.UUID), b); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrConflict
		}
		return fmt.Errorf("eventsource: create: %w", err)
	}
	return nil
}

// Get returns the mapping and its KV revision.
func (s *Store) Get(ctx context.Context, uuid string) (Mapping, uint64, error) {
	e, err := s.kv.Get(ctx, key(uuid))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Mapping{}, 0, ErrNotFound
	}
	if err != nil {
		return Mapping{}, 0, fmt.Errorf("eventsource: get: %w", err)
	}
	m, err := decode(e.Value())
	return m, e.Revision(), err
}

// Entry is one listed mapping with the revision it was read at.
type Entry struct {
	Mapping  Mapping
	Revision uint64
}

// List returns every mapping in the bucket once.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	lister, err := s.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("eventsource: list: %w", err)
	}
	defer func() { _ = lister.Stop() }()
	var out []Entry
	// A KV listing may repeat a key that changes while it is being listed.
	seen := map[string]bool{}
	for k := range lister.Keys() {
		if seen[k] {
			continue
		}
		seen[k] = true
		e, err := s.kv.Get(ctx, k)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("eventsource: list get: %w", err)
		}
		m, err := decode(e.Value())
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Mapping: m, Revision: e.Revision()})
	}
	return out, nil
}

// Update replaces the mapping only if it is still at revision.
func (s *Store) Update(ctx context.Context, m Mapping, revision uint64) (uint64, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	rev, err := s.kv.Update(ctx, key(m.UUID), b, revision)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return 0, ErrConflict
		}
		return 0, fmt.Errorf("eventsource: update: %w", err)
	}
	return rev, nil
}

// Remove deletes the mapping only if it is still at revision.
func (s *Store) Remove(ctx context.Context, uuid string, revision uint64) error {
	if err := s.kv.Delete(ctx, key(uuid), jetstream.LastRevision(revision)); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrConflict
		}
		return fmt.Errorf("eventsource: remove: %w", err)
	}
	return nil
}

func decode(b []byte) (Mapping, error) {
	var m Mapping
	if err := json.Unmarshal(b, &m); err != nil {
		return Mapping{}, fmt.Errorf("eventsource: decode: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return Mapping{}, fmt.Errorf("eventsource: record format %d is not %d", m.FormatVersion, FormatVersion)
	}
	return m, nil
}
