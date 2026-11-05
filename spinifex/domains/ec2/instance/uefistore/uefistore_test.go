package uefistore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const instanceID = "i-0123456789abcdef0"

func openStore(t *testing.T) (*uefistore.Store, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	s := uefistore.NewStore(js)
	require.NoError(t, s.Open(t.Context()))
	return s, js
}

func liveRecord() *uefistore.UEFIStoreRecord {
	return &uefistore.UEFIStoreRecord{InstanceID: instanceID, AccountID: "111122223333", State: uefistore.StateLive}
}

func pointer(name string) *uefistore.UEFIVarStoreRef {
	return &uefistore.UEFIVarStoreRef{
		Key:        keyOf(name),
		Firmware:   uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: 540672, CodeSHA256: "c0de"},
		WriterNode: "node-2",
		WrittenAt:  time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC),
	}
}

func keyOf(name string) string { return uefistore.KeyPrefix(instanceID) + name }

// pointedRecord is a live record stamped at generation 7 that points at "b".
func pointedRecord() *uefistore.UEFIStoreRecord {
	rec := liveRecord()
	rec.Generation = 7
	rec.Pointer = pointer("b")
	return rec
}

func rawPut(t *testing.T, js jetstream.JetStream, key, body string) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), uefistore.BucketName)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, []byte(body))
	require.NoError(t, err)
}

// The records carry obligations past the 1 h terminated-record TTL, so the
// bucket must not expire them, and it is stamped so a newer schema is refused.
func TestOpen_CreatesBucketWithNoTTLAndStampsVersion(t *testing.T) {
	_, js := openStore(t)

	kv, err := js.KeyValue(t.Context(), uefistore.BucketName)
	require.NoError(t, err)
	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.History())
	assert.Zero(t, status.TTL(), "a TTL would expire deletion obligations before they are settled")

	v, err := kvstore.ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, uefistore.BucketVersion, v)
}

func TestCreate_RoundTripsAndStampsSchemaVersion(t *testing.T) {
	s, _ := openStore(t)
	rec := pointedRecord()

	rev, err := s.Create(t.Context(), rec)
	require.NoError(t, err)

	got, gotRev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, uefistore.RecordSchemaVersion, got.SchemaVersion)
	assert.Equal(t, rec, got)
}

// The wire names are what every release reads, so they are pinned.
func TestRecord_WireShape(t *testing.T) {
	s, js := openStore(t)
	_, err := s.Create(t.Context(), pointedRecord())
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), uefistore.BucketName)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"schema_version": 1,
		"instance_id": "i-0123456789abcdef0",
		"account_id": "111122223333",
		"state": "live",
		"generation": 7,
		"pointer": {
			"key": "uefi/i-0123456789abcdef0/b",
			"firmware": {"arch": "x86_64", "vars_size": 540672, "code_sha256": "c0de"},
			"writer_node": "node-2",
			"written_at": "2026-11-03T01:02:03Z"
		}
	}`, string(entry.Value()))
}

func TestGet_MissingIsNotFound(t *testing.T) {
	s, _ := openStore(t)
	_, _, err := s.Get(t.Context(), instanceID)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

func TestCreate_ExistingRecordIsErrExists(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)

	dup := liveRecord()
	dup.AccountID = "999999999999"
	_, err = s.Create(t.Context(), dup)
	require.ErrorIs(t, err, kvstore.ErrExists)

	got, _, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "111122223333", got.AccountID, "the second create must not overwrite the first")
}

// A store is recorded live before its first object, so a record that starts
// out deleting has no object it could be describing.
func TestCreate_RefusesDeleting(t *testing.T) {
	s, _ := openStore(t)
	rec := liveRecord()
	rec.State = uefistore.StateDeleting

	_, err := s.Create(t.Context(), rec)
	require.ErrorIs(t, err, uefistore.ErrInvalidRecord)
	_, _, err = s.Get(t.Context(), instanceID)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

func TestCompareAndSet_StaleRevisionConflictsAndWritesNothing(t *testing.T) {
	s, _ := openStore(t)
	first, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)

	moved := pointedRecord()
	second, err := s.CompareAndSet(t.Context(), moved, first)
	require.NoError(t, err)
	assert.Greater(t, second, first)

	stale := liveRecord()
	stale.State = uefistore.StateDeleting
	_, err = s.CompareAndSet(t.Context(), stale, first)
	require.ErrorIs(t, err, kvstore.ErrConflict)

	got, rev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, second, rev)
	assert.Equal(t, uefistore.StateLive, got.State)
	assert.Equal(t, pointer("b"), got.Pointer)
}

// Two actors that read the same revision race to change it; exactly one may
// win, so neither can overwrite an obligation the other recorded.
func TestCompareAndSet_ConcurrentWritersFromOneReadHaveOneWinner(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	_, rev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)

	const writers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []string
	)
	for i := range writers {
		wg.Go(func() {
			rec := pointedRecord()
			rec.Pointer.WriterNode = fmt.Sprintf("node-%d", i)
			if _, err := s.CompareAndSet(context.Background(), rec, rev); err == nil {
				mu.Lock()
				wins = append(wins, rec.Pointer.WriterNode)
				mu.Unlock()
			} else {
				assert.ErrorIs(t, err, kvstore.ErrConflict)
			}
		})
	}
	wg.Wait()

	require.Len(t, wins, 1)
	got, _, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, wins[0], got.Pointer.WriterNode)
}

func TestCompareAndDelete_StaleRevisionKeepsRecord(t *testing.T) {
	s, _ := openStore(t)
	first, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	deleting := liveRecord()
	deleting.State = uefistore.StateDeleting
	second, err := s.CompareAndSet(t.Context(), deleting, first)
	require.NoError(t, err)

	require.ErrorIs(t, s.CompareAndDelete(t.Context(), instanceID, first), kvstore.ErrConflict)
	_, _, err = s.Get(t.Context(), instanceID)
	require.NoError(t, err, "a delete guarded on a stale revision must leave the record")

	require.NoError(t, s.CompareAndDelete(t.Context(), instanceID, second))
	_, _, err = s.Get(t.Context(), instanceID)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

// A deletion acts on the key the record points at, so a key outside the
// instance's own prefix must never reach the bucket.
func TestCompareAndSet_RefusesInvalidRecords(t *testing.T) {
	other := uefistore.KeyPrefix("i-0fedcba9876543210") + "b"
	cases := map[string]func(*uefistore.UEFIStoreRecord){
		"unknown state":                   func(r *uefistore.UEFIStoreRecord) { r.State = "gone" },
		"not an instance ID":              func(r *uefistore.UEFIStoreRecord) { r.InstanceID = "ami-0123456789abcdef0" },
		"pointer under another instance":  func(r *uefistore.UEFIStoreRecord) { r.Pointer.Key = other },
		"pointer under an image":          func(r *uefistore.UEFIStoreRecord) { r.Pointer.Key = uefistore.KeyPrefix("ami-0123456789abcdef0") + "b" },
		"pointer is the prefix itself":    func(r *uefistore.UEFIStoreRecord) { r.Pointer.Key = uefistore.KeyPrefix(instanceID) },
		"pointer nested below the prefix": func(r *uefistore.UEFIStoreRecord) { r.Pointer.Key = keyOf("x/y") },
		"pointer with no firmware arch":   func(r *uefistore.UEFIStoreRecord) { r.Pointer.Firmware.Arch = "" },
		"pointer with no VARS size":       func(r *uefistore.UEFIStoreRecord) { r.Pointer.Firmware.VarsSize = 0 },
		"pointer with no writer node":     func(r *uefistore.UEFIStoreRecord) { r.Pointer.WriterNode = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := openStore(t)
			rev, err := s.Create(t.Context(), liveRecord())
			require.NoError(t, err)

			rec := pointedRecord()
			mutate(rec)
			_, err = s.CompareAndSet(t.Context(), rec, rev)
			require.ErrorIs(t, err, uefistore.ErrInvalidRecord)

			got, gotRev, err := s.Get(t.Context(), instanceID)
			require.NoError(t, err)
			assert.Equal(t, rev, gotRev, "an invalid record must not be written")
			assert.Nil(t, got.Pointer)
		})
	}
}

// A record a newer release wrote, or one that does not describe its own key,
// is reported rather than decoded into something a reaper would act on.
func TestGet_RefusesRecordsItCannotTrust(t *testing.T) {
	cases := map[string]string{
		"newer schema":     `{"schema_version":2,"instance_id":"i-0123456789abcdef0","state":"live"}`,
		"no schema":        `{"instance_id":"i-0123456789abcdef0","state":"live"}`,
		"another instance": `{"schema_version":1,"instance_id":"i-0fedcba9876543210","state":"deleting"}`,
		"unknown state":    `{"schema_version":1,"instance_id":"i-0123456789abcdef0","state":"purged"}`,
		"foreign pointer":  `{"schema_version":1,"instance_id":"i-0123456789abcdef0","state":"live","generation":3,"pointer":{"key":"uefi/i-0fedcba9876543210/b","firmware":{"arch":"x86_64","vars_size":540672},"writer_node":"n"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, js := openStore(t)
			require.True(t, json.Valid([]byte(body)))
			rawPut(t, js, instanceID, body)

			_, _, err := s.Get(t.Context(), instanceID)
			require.ErrorIs(t, err, uefistore.ErrInvalidRecord)
		})
	}
}
