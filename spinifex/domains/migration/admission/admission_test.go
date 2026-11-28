package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const acct = "111122223333"

var t0 = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

func clock() time.Time { return t0 }

type terminating map[string]bool

func (m terminating) AccountTerminating(_ context.Context, id string) (bool, error) {
	return m[id], nil
}

// levelGate returns a real feature-level gate over a record at level.
func levelGate(t *testing.T, js jetstream.JetStream, record string) *featurelevel.Gate {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-test", History: 1})
	require.NoError(t, err)
	if record != "" {
		_, err = kv.Create(t.Context(), featurelevel.Key, []byte(record))
		require.NoError(t, err)
	}
	return featurelevel.NewGate(featurelevel.NewStore(kv))
}

func connection(id string) store.Record {
	return store.Record{Kind: "connection", ID: id, AccountID: acct}
}

func bucketExists(t *testing.T, js jetstream.KeyValueManager) bool {
	t.Helper()
	_, err := js.KeyValue(t.Context(), store.BucketName(acct))
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestCreate_AdmittedAtTheMigrationLevel(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	g := NewGate(js, levelGate(t, js, `{"level":4}`), clock)

	offered, err := g.Offered(t.Context())
	require.NoError(t, err)
	assert.True(t, offered)
	e, err := g.Create(t.Context(), connection("c-1"))
	require.NoError(t, err)
	assert.NotZero(t, e.Revision)
	assert.False(t, e.Record.DeleteRequested)
}

// Verifies: ADR-0014:S7. A cluster where some node has not reported the
// release with the participant refuses creation and writes nothing.
func TestS7_MixedVersionClusterRefusesCreation(t *testing.T) {
	for name, record := range map[string]string{
		"no level recorded":              "",
		"a node still runs the previous": `{"level":3}`,
		"rollback below migration":       `{"level":4,"hold":{"held":true,"level":3},"rollback":{"target":3,"prior_hold":{"held":false,"level":0}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			g := NewGate(js, levelGate(t, js, record), clock)

			offered, err := g.Offered(t.Context())
			require.NoError(t, err)
			assert.False(t, offered, "the API is not exposed")
			_, err = g.Create(t.Context(), connection("c-1"))
			require.ErrorIs(t, err, ErrNotOffered)
			assert.False(t, bucketExists(t, js), "a refused create writes nothing")
		})
	}
}

func TestCreate_NoLevelGateRefuses(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := NewGate(js, nil, clock).Create(t.Context(), connection("c-1"))
	require.ErrorIs(t, err, ErrNotOffered)
}

// Verifies: ADR-0014:S4. After the seal, creation refuses and writes nothing.
func TestS4_CreationRefusedAfterSeal(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	g := NewGate(js, levelGate(t, js, `{"level":4}`), clock)
	require.NoError(t, NewSeals(js).Seal(t.Context(), terminating{acct: true}, acct, t0))

	_, err := g.Create(t.Context(), connection("c-1"))
	require.ErrorIs(t, err, ErrAccountSealed)
	assert.False(t, bucketExists(t, js))

	other := store.Record{Kind: "connection", ID: "c-1", AccountID: "444455556666"}
	_, err = g.Create(t.Context(), other)
	require.NoError(t, err, "the seal covers only the sealed account")
}

func TestSeal_RefusedUnlessIAMHoldsTheAccountTerminating(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	seals := NewSeals(js)
	require.ErrorIs(t, seals.Seal(t.Context(), terminating{}, acct, t0), ErrNotTerminating)
	sealed, err := seals.Sealed(t.Context(), acct)
	require.NoError(t, err)
	assert.False(t, sealed)

	require.Error(t, seals.Seal(t.Context(), terminating{"000000000000": true}, "000000000000", t0), "the system account is never sealed")
	require.Error(t, seals.Seal(t.Context(), nil, acct, t0))

	require.NoError(t, seals.Seal(t.Context(), terminating{acct: true}, acct, t0))
	require.NoError(t, seals.Seal(t.Context(), terminating{acct: true}, acct, t0.Add(time.Hour)), "a second seal is success")
	sealed, err = seals.Sealed(t.Context(), acct)
	require.NoError(t, err)
	assert.True(t, sealed)
}

// sealOnCreate seals the account inside the record write, which is the
// widest window a create can race the termination command through.
type sealOnCreate struct {
	jetstream.KeyValueManager

	seal func()
}

func (s *sealOnCreate) KeyValue(ctx context.Context, name string) (jetstream.KeyValue, error) {
	kv, err := s.KeyValueManager.KeyValue(ctx, name)
	return s.wrap(name, kv), err
}

func (s *sealOnCreate) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := s.KeyValueManager.CreateKeyValue(ctx, cfg)
	return s.wrap(cfg.Bucket, kv), err
}

func (s *sealOnCreate) wrap(name string, kv jetstream.KeyValue) jetstream.KeyValue {
	if kv == nil || name != store.BucketName(acct) {
		return kv
	}
	return &sealingKV{KeyValue: kv, seal: s.seal}
}

type sealingKV struct {
	jetstream.KeyValue

	seal func()
}

func (k *sealingKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.seal()
	return k.KeyValue.Create(ctx, key, value, opts...)
}

// A create that passed the seal check before the seal landed must not leave a
// record the termination pass misses: it refuses and marks its own record.
func TestS4_CreateThatRacesTheSealMarksItsRecord(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	level := levelGate(t, js, `{"level":4}`)
	racing := &sealOnCreate{KeyValueManager: js, seal: func() {
		require.NoError(t, NewSeals(js).Seal(context.Background(), terminating{acct: true}, acct, t0))
	}}
	g := NewGate(racing, level, clock)

	_, err := g.Create(t.Context(), connection("c-1"))
	require.ErrorIs(t, err, ErrAccountSealed)

	s, err := store.Open(t.Context(), js, acct)
	require.NoError(t, err)
	e, err := s.Get(t.Context(), "connection", "c-1")
	require.NoError(t, err)
	assert.True(t, e.Record.DeleteRequested, "the raced record is left for the drain")
}
