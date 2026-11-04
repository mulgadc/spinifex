//test:in-package — forges stored bytes through the unexported kv handle and
// stubs the unexported stream-status seam (stat) to reach the revalidation paths.

package mgmtipstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "mgmt-ipam-test"

var (
	testPrefix = netip.MustParsePrefix("10.11.12.0/24")
	testKey    = testPrefix.String()
)

func newStore(t *testing.T) (*Store, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	s, err := Create(t.Context(), js, testBucket, "test")
	require.NoError(t, err)
	return s, js
}

func putRecord(t *testing.T, s *Store, rec Record) uint64 {
	t.Helper()
	raw, err := encode(rec)
	require.NoError(t, err)
	return putRaw(t, s, string(raw))
}

func putRaw(t *testing.T, s *Store, raw string) uint64 {
	t.Helper()
	rev, err := s.kv.Put(t.Context(), testKey, []byte(raw))
	require.NoError(t, err)
	return rev
}

func formed(entries ...Entry) Record {
	return Record{Schema: SchemaMajor, Authority: AuthorityFormed, Subnet: testKey, Entries: entries}
}

func entry(ip, instance, node string) Entry { return Entry{IP: ip, InstanceID: instance, Node: node} }

func storedValue(t *testing.T, s *Store) ([]byte, uint64) {
	t.Helper()
	e, err := s.kv.Get(t.Context(), testKey)
	require.NoError(t, err)
	return e.Value(), e.Revision()
}

func requireUnchanged(t *testing.T, s *Store, raw string, rev uint64) {
	t.Helper()
	val, gotRev := storedValue(t, s)
	assert.Equal(t, raw, string(val))
	assert.Equal(t, rev, gotRev)
}

func TestBind_AbsentBucketIsNotCreated(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := Bind(t.Context(), js, testBucket)
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound)
	_, err = js.KeyValue(t.Context(), testBucket)
	assert.ErrorIs(t, err, jetstream.ErrBucketNotFound)
}

func TestBind_RefusesTTLBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := kvutil.GetOrCreateBucketWithOptions(t.Context(), js, kvutil.BucketOptions{Name: testBucket, History: 1, TTL: time.Hour})
	require.NoError(t, err)
	_, err = Bind(t.Context(), js, testBucket)
	require.ErrorContains(t, err, "not durable")
}

func TestBind_RefusesBoundedBucketAndAcceptsDurable(t *testing.T) {
	s, js := newStore(t)
	bound, err := Bind(t.Context(), js, testBucket)
	require.NoError(t, err)
	assert.Equal(t, s.created, bound.created)

	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "capped", History: 1, MaxBytes: 1 << 20})
	require.NoError(t, err)
	_, err = Bind(t.Context(), js, "capped")
	require.ErrorContains(t, err, "not durable")

	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "mem", History: 1, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	_, err = Bind(t.Context(), js, "mem")
	require.ErrorContains(t, err, "not durable")
}

func TestLoad_NoRecordIsNotAuthoritativeAndWritesNothing(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Load(t.Context(), testPrefix)
	require.ErrorIs(t, err, ErrNotAuthoritative)

	_, err = s.Lookup(t.Context(), testPrefix, "i-1", "node1", "10.11.12.10")
	require.ErrorIs(t, err, ErrNotAuthoritative)

	_, err = s.Insert(t.Context(), &Snapshot{owner: s, bucket: s.bucket, created: s.created, prefix: testPrefix, revision: 1}, "i-1", "node1")
	require.Error(t, err)

	_, err = s.kv.Get(t.Context(), testKey)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestLoad_RefusesBadSchemaOrAuthority(t *testing.T) {
	sub := `"subnet":"10.11.12.0/24"`
	cases := map[string]string{
		"missing schema":     `{"authority":"formed",` + sub + `,"entries":[]}`,
		"zero schema":        `{"schema":0,"authority":"formed",` + sub + `,"entries":[]}`,
		"unsupported schema": `{"schema":2,"authority":"formed",` + sub + `,"entries":[]}`,
		"future with extra":  `{"schema":2,"authority":"formed",` + sub + `,"entries":[],"extra":1}`,
		"missing authority":  `{"schema":1,` + sub + `,"entries":[]}`,
		"unknown authority":  `{"schema":1,"authority":"draft",` + sub + `,"entries":[]}`,
		"not json":           `nonsense`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newStore(t)
			rev := putRaw(t, s, raw)

			_, err := s.Load(t.Context(), testPrefix)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrUnsupportedSchema) || errors.Is(err, ErrNotAuthoritative), err.Error())
			_, err = s.Lookup(t.Context(), testPrefix, "i-1", "node1", "10.11.12.10")
			require.Error(t, err)

			requireUnchanged(t, s, raw, rev)
		})
	}
}

func TestLoad_RefusesUnknownFields(t *testing.T) {
	cases := map[string]string{
		"top level": `{"schema":1,"authority":"formed","subnet":"10.11.12.0/24","entries":[],"future_obligation":{"ip":"10.11.12.10"}}`,
		"in entry":  `{"schema":1,"authority":"formed","subnet":"10.11.12.0/24","entries":[{"ip":"10.11.12.10","instance_id":"i-1","node":"n","lease":"x"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newStore(t)
			rev := putRaw(t, s, raw)
			_, err := s.Load(t.Context(), testPrefix)
			require.ErrorIs(t, err, ErrUnknownField)
			_, err = s.Lookup(t.Context(), testPrefix, "i-1", "n", "10.11.12.10")
			require.ErrorIs(t, err, ErrUnknownField)
			requireUnchanged(t, s, raw, rev)
		})
	}
}

func TestLoad_RefusesAmbiguousEncodings(t *testing.T) {
	holder := `{"ip":"10.11.12.10","instance_id":"i-1","node":"n"}`
	head := `{"schema":1,"authority":"formed","subnet":"10.11.12.0/24",`
	cases := map[string]string{
		"duplicate top-level key": head + `"entries":[` + holder + `],"entries":[]}`,
		"duplicate key in entry":  head + `"entries":[{"ip":"10.11.12.10","instance_id":"i-1","node":"n","node":"m"}]}`,
		"case aliased key":        head + `"Entries":[` + holder + `]}`,
		"case aliased entry key":  head + `"entries":[{"IP":"10.11.12.10","instance_id":"i-1","node":"n"}]}`,
		"reordered":               `{"authority":"formed","schema":1,"subnet":"10.11.12.0/24","entries":[` + holder + `]}`,
		"extra whitespace":        head + ` "entries":[` + holder + `]}`,
		"null entries":            head + `"entries":null}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newStore(t)
			rev := putRaw(t, s, raw)
			_, err := s.Load(t.Context(), testPrefix)
			require.ErrorIs(t, err, ErrNoncanonical)
			_, err = s.Lookup(t.Context(), testPrefix, "i-1", "n", "10.11.12.10")
			require.ErrorIs(t, err, ErrNoncanonical)
			requireUnchanged(t, s, raw, rev)
		})
	}
}

func TestLoad_RefusesInvalidContent(t *testing.T) {
	cases := map[string]Record{
		"empty node":             formed(entry("10.11.12.10", "i-1", "")),
		"empty instance":         formed(entry("10.11.12.10", "", "n")),
		"leading zero ipv4":      formed(entry("10.11.12.010", "i-1", "n")),
		"ipv4 mapped ipv6":       formed(entry("::ffff:10.11.12.10", "i-1", "n")),
		"padded ipv4":            formed(entry(" 10.11.12.10", "i-1", "n")),
		"outside subnet":         formed(entry("10.11.13.10", "i-1", "n")),
		"not an address":         formed(entry("host", "i-1", "n")),
		"duplicate address":      formed(entry("10.11.12.10", "i-1", "n"), entry("10.11.12.10", "i-2", "n")),
		"duplicate holder":       formed(entry("10.11.12.10", "i-1", "n"), entry("10.11.12.11", "i-1", "n")),
		"stored subnet differs":  {Schema: SchemaMajor, Authority: AuthorityFormed, Subnet: "10.11.13.0/24", Entries: []Entry{}},
		"stored subnet missing":  {Schema: SchemaMajor, Authority: AuthorityFormed, Entries: []Entry{}},
		"stored subnet unmasked": {Schema: SchemaMajor, Authority: AuthorityFormed, Subnet: "10.11.12.5/24", Entries: []Entry{}},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newStore(t)
			raw, err := json.Marshal(rec)
			require.NoError(t, err)
			rev := putRaw(t, s, string(raw))
			_, err = s.Load(t.Context(), testPrefix)
			require.ErrorIs(t, err, ErrInvalidRecord)
			requireUnchanged(t, s, string(raw), rev)
		})
	}
}

func TestPrefix_OnlyCanonicalIPv4Slash24(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed())

	for _, p := range []string{"10.11.12.5/24", "10.11.0.0/16", "10.11.12.0/25", "fd00::/24", "10.11.12.0/32"} {
		_, err := s.Load(t.Context(), netip.MustParsePrefix(p))
		require.ErrorIs(t, err, ErrInvalidArgument, p)
	}
	_, err := s.Load(t.Context(), netip.Prefix{})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// An address inside the subnet reaches the one record only once masked.
	masked := netip.MustParsePrefix("10.11.12.77/24").Masked()
	snap, err := s.Load(t.Context(), masked)
	require.NoError(t, err)
	assert.Equal(t, testKey, snap.Record().Subnet)
}

func TestLookup_RefusesEmptyOrNoncanonicalArguments(t *testing.T) {
	s, _ := newStore(t)
	rev := putRecord(t, s, formed(entry("10.11.12.10", "i-1", "n")))
	raw, _ := storedValue(t, s)

	for _, a := range [][3]string{{"", "n", "10.11.12.10"}, {"i-1", "", "10.11.12.10"}, {"i-1", "n", "10.11.12.010"}, {"i-1", "n", "10.11.13.10"}} {
		_, err := s.Lookup(t.Context(), testPrefix, a[0], a[1], a[2])
		require.ErrorIs(t, err, ErrInvalidArgument, a)
	}
	requireUnchanged(t, s, string(raw), rev)
}

func TestOperations_RefuseAfterConfigDrift(t *testing.T) {
	drifts := map[string]func(*jetstream.StreamConfig){
		"max age":   func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour },
		"max bytes": func(c *jetstream.StreamConfig) { c.MaxBytes = 1 << 20 },
	}
	for name, drift := range drifts {
		t.Run(name, func(t *testing.T) {
			s, js := newStore(t)
			rev := putRecord(t, s, formed())
			raw, _ := storedValue(t, s)
			snap, err := s.Load(t.Context(), testPrefix)
			require.NoError(t, err)

			stream, err := js.Stream(t.Context(), "KV_"+testBucket)
			require.NoError(t, err)
			cfg := stream.CachedInfo().Config
			drift(&cfg)
			_, err = js.UpdateStream(t.Context(), cfg)
			require.NoError(t, err)

			_, err = s.Load(t.Context(), testPrefix)
			require.ErrorContains(t, err, "not durable")
			require.ErrorContains(t, s.Confirm(t.Context(), snap), "not durable")
			_, err = s.Insert(t.Context(), snap, "i-1", "node1")
			require.ErrorContains(t, err, "not durable")
			_, err = s.Lookup(t.Context(), testPrefix, "i-1", "node1", "10.11.12.10")
			require.ErrorContains(t, err, "not durable")

			requireUnchanged(t, s, string(raw), rev)
		})
	}
}

func TestLookup_ReturnsConfirmedEntry(t *testing.T) {
	s, _ := newStore(t)
	want := entry("10.11.12.10", "i-1", "node1")
	rev := putRecord(t, s, formed(want))

	got, err := s.Lookup(t.Context(), testPrefix, "i-1", "node1", "10.11.12.10")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	_, gotRev := storedValue(t, s)
	assert.Greater(t, gotRev, rev, "the confirmation is an acknowledged write")

	for _, miss := range [][3]string{{"i-2", "node1", "10.11.12.10"}, {"i-1", "node2", "10.11.12.10"}, {"i-1", "node1", "10.11.12.11"}} {
		_, err = s.Lookup(t.Context(), testPrefix, miss[0], miss[1], miss[2])
		assert.ErrorIs(t, err, ErrNotFound)
	}
}

func TestConfirm_StaleReadFails(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed(entry("10.11.12.10", "i-1", "node1")))
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	putRecord(t, s, formed())
	raw, rev := storedValue(t, s)

	require.ErrorIs(t, s.Confirm(t.Context(), snap), ErrStale)
	requireUnchanged(t, s, string(raw), rev)
}

func TestInsert_PicksFirstFreeAddress(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed(entry("10.11.12.10", "i-1", "node1")))
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	got, err := s.Insert(t.Context(), snap, "i-2", "node2")
	require.NoError(t, err)
	assert.Equal(t, entry("10.11.12.11", "i-2", "node2"), got)

	reloaded, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	rec := reloaded.Record()
	assert.Len(t, rec.Entries, 2)
	assert.Equal(t, AuthorityFormed, rec.Authority)
}

func TestInsert_RangeExhausted(t *testing.T) {
	s, _ := newStore(t)
	var full []Entry
	for i := rangeMin; i <= rangeMax; i++ {
		full = append(full, entry(fmt.Sprintf("10.11.12.%d", i), fmt.Sprintf("i-%d", i), "node1"))
	}
	putRecord(t, s, formed(full...))
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	_, err = s.Insert(t.Context(), snap, "i-new", "node1")
	require.ErrorIs(t, err, ErrRangeExhausted)
}

func TestInsert_RefusesInstanceThatAlreadyHoldsAnAddress(t *testing.T) {
	s, _ := newStore(t)
	rev := putRecord(t, s, formed(entry("10.11.12.10", "i-1", "node1")))
	raw, _ := storedValue(t, s)
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	_, err = s.Insert(t.Context(), snap, "i-1", "node2")
	require.ErrorIs(t, err, ErrAlreadyHeld)
	_, err = s.Insert(t.Context(), snap, "", "node2")
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = s.Insert(t.Context(), snap, "i-2", "")
	require.ErrorIs(t, err, ErrInvalidArgument)
	requireUnchanged(t, s, string(raw), rev)
}

func TestInsert_ConcurrentSameAddressOneWins(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed())
	a, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	b, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, snap := range []*Snapshot{a, b} {
		wg.Go(func() {
			_, errs[i] = s.Insert(t.Context(), snap, fmt.Sprintf("i-%d", i), "node1")
		})
	}
	wg.Wait()

	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		} else {
			assert.ErrorIs(t, e, ErrConflict)
		}
	}
	require.Equal(t, 1, wins)
	reloaded, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	entries := reloaded.Record().Entries
	require.Len(t, entries, 1)
	assert.Equal(t, "10.11.12.10", entries[0].IP)
}

func TestInsert_FailsAfterStateTransitionLands(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed())
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	require.Equal(t, AuthorityFormed, snap.Record().Authority)

	transition := formed()
	transition.Authority = AuthorityMigrated
	putRecord(t, s, transition)

	_, err = s.Insert(t.Context(), snap, "i-1", "node1")
	require.ErrorIs(t, err, ErrConflict)

	reloaded, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	assert.Equal(t, AuthorityMigrated, reloaded.Record().Authority)
	assert.Empty(t, reloaded.Record().Entries)
}

func TestInsert_IgnoresCallerMutationOfSnapshotRecord(t *testing.T) {
	s, _ := newStore(t)
	putRecord(t, s, formed(entry("10.11.12.10", "i-1", "node1")))
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	tampered := snap.Record()
	tampered.Entries = nil
	tampered.Authority = AuthorityMigrated
	tampered.Schema = 9

	got, err := s.Insert(t.Context(), snap, "i-2", "node1")
	require.NoError(t, err)
	assert.Equal(t, "10.11.12.11", got.IP, "the holder the caller tried to drop still blocks its address")

	reloaded, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	rec := reloaded.Record()
	assert.Equal(t, AuthorityFormed, rec.Authority)
	assert.Equal(t, SchemaMajor, rec.Schema)
	assert.Len(t, rec.Entries, 2)
}

func TestSnapshot_ForgedOrForeignIsRefusedWithNothingWritten(t *testing.T) {
	s, js := newStore(t)
	rev := putRecord(t, s, formed(entry("10.11.12.10", "i-1", "node1")))
	raw, _ := storedValue(t, s)

	other, err := Create(t.Context(), js, "mgmt-ipam-other", "test")
	require.NoError(t, err)
	_, err = other.kv.Put(t.Context(), testKey, raw)
	require.NoError(t, err)
	foreign, err := other.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	for name, snap := range map[string]*Snapshot{"nil": nil, "zero": {}, "other store": foreign} {
		_, err := s.Insert(t.Context(), snap, "i-2", "node1")
		require.ErrorIs(t, err, ErrForeignSnapshot, name)
		require.ErrorIs(t, s.Confirm(t.Context(), snap), ErrForeignSnapshot, name)
	}
	requireUnchanged(t, s, string(raw), rev)

	sibling, err := Bind(t.Context(), js, testBucket)
	require.NoError(t, err)
	mine, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	_, err = sibling.Insert(t.Context(), mine, "i-2", "node1")
	require.ErrorIs(t, err, ErrForeignSnapshot)
	requireUnchanged(t, s, string(raw), rev)
}

func TestSnapshot_FromAnotherServerWithSameBucketAndRevisionIsRefused(t *testing.T) {
	a, _ := newStore(t)
	b, _ := newStore(t)
	revA := putRecord(t, a, formed(entry("10.11.12.10", "i-a", "node1")))
	revB := putRecord(t, b, formed(entry("10.11.12.10", "i-b", "node1")))
	require.Equal(t, revA, revB)
	rawA, _ := storedValue(t, a)
	rawB, _ := storedValue(t, b)

	snapA, err := a.Load(t.Context(), testPrefix)
	require.NoError(t, err)
	_, err = b.Insert(t.Context(), snapA, "i-2", "node1")
	require.ErrorIs(t, err, ErrForeignSnapshot)
	require.ErrorIs(t, b.Confirm(t.Context(), snapA), ErrForeignSnapshot)

	requireUnchanged(t, a, string(rawA), revA)
	requireUnchanged(t, b, string(rawB), revB)
}

// Documents the hazard the store narrows: a bound handle names the stream, not
// an incarnation, so a recreated bucket at the same revision satisfies an old CAS.
func TestKV_RecreatedBucketSatisfiesOldRevision(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	old, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{Name: testBucket, History: 1})
	require.NoError(t, err)
	oldRev, err := old.Put(ctx, "k", []byte("old holder"))
	require.NoError(t, err)

	require.NoError(t, js.DeleteKeyValue(ctx, testBucket))
	fresh, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{Name: testBucket, History: 1})
	require.NoError(t, err)
	newRev, err := fresh.Put(ctx, "k", []byte("new holder"))
	require.NoError(t, err)
	require.Equal(t, oldRev, newRev)

	_, err = old.Update(ctx, "k", []byte("overwrite"), oldRev)
	require.NoError(t, err, "the old handle's conditional write lands on the new incarnation")
}

func TestStore_RefusesRetainedStateAfterBucketRecreated(t *testing.T) {
	old, js := newStore(t)
	ctx := t.Context()
	putRecord(t, old, formed(entry("10.11.12.10", "i-old", "node1")))
	snap, err := old.Load(ctx, testPrefix)
	require.NoError(t, err)

	require.NoError(t, js.DeleteKeyValue(ctx, testBucket))
	fresh, err := Create(ctx, js, testBucket, "test")
	require.NoError(t, err)
	freshRev := putRecord(t, fresh, formed(entry("10.11.12.10", "i-new", "node2")))
	require.Equal(t, snap.Revision(), freshRev, "the old revision would satisfy the new key's CAS")
	raw, _ := storedValue(t, fresh)

	_, err = old.Insert(ctx, snap, "i-3", "node1")
	require.ErrorIs(t, err, ErrIncarnation)
	require.ErrorIs(t, old.Confirm(ctx, snap), ErrIncarnation)
	_, err = old.Load(ctx, testPrefix)
	require.ErrorIs(t, err, ErrIncarnation)
	_, err = old.Lookup(ctx, testPrefix, "i-new", "node2", "10.11.12.10")
	require.ErrorIs(t, err, ErrIncarnation)

	_, err = fresh.Insert(ctx, snap, "i-3", "node1")
	require.ErrorIs(t, err, ErrForeignSnapshot)
	require.ErrorIs(t, fresh.Confirm(ctx, snap), ErrForeignSnapshot)

	requireUnchanged(t, fresh, string(raw), freshRev)
}

func TestStore_RefusesStreamWithoutCreationTime(t *testing.T) {
	s, _ := newStore(t)
	rev := putRecord(t, s, formed())
	raw, _ := storedValue(t, s)
	snap, err := s.Load(t.Context(), testPrefix)
	require.NoError(t, err)

	zeroCreated := func(context.Context) (*jetstream.StreamInfo, error) { return &jetstream.StreamInfo{}, nil }

	_, err = newStoreWithStat(t.Context(), s.kv, testBucket, zeroCreated)
	require.ErrorIs(t, err, ErrNoCreationTime)

	s.stat = zeroCreated
	_, err = s.Load(t.Context(), testPrefix)
	require.ErrorIs(t, err, ErrNoCreationTime)
	require.ErrorIs(t, s.Confirm(t.Context(), snap), ErrNoCreationTime)
	_, err = s.Insert(t.Context(), snap, "i-1", "node1")
	require.ErrorIs(t, err, ErrNoCreationTime)
	requireUnchanged(t, s, string(raw), rev)
}
