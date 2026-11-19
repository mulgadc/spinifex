package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedRecord answers every read with its record and revision, or err.
type fixedRecord struct {
	rec Record
	rev uint64
	err error
}

type Record = featurelevel.Record

func (f *fixedRecord) read(context.Context) (featurelevel.Record, uint64, error) {
	return f.rec, f.rev, f.err
}

// Prefix-list peers grant only while level 2 is active: a pending rollback
// below it withdraws the grant at the revision it is observed, and a failed
// read changes nothing.
func TestLevelWatch_GrantFollowsLevel2(t *testing.T) {
	src := &fixedRecord{rec: Record{Level: 1}, rev: 1}
	w := &levelWatch{read: src.read}
	assert.False(t, w.Granted(), "nothing is granted before the first read")

	require.NoError(t, w.observe(t.Context()))
	assert.False(t, w.Granted())

	src.rec, src.rev = Record{Level: 2}, 2
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, w.Granted())

	src.err = errors.New("leader unreachable")
	require.Error(t, w.observe(t.Context()))
	assert.True(t, w.Granted(), "an unreadable record keeps the last view")
	assert.Equal(t, uint64(2), w.Revision())

	src.err = nil
	src.rec, src.rev = Record{Level: 2, Rollback: &featurelevel.Rollback{Target: 1}}, 3
	require.NoError(t, w.observe(t.Context()))
	assert.False(t, w.Granted(), "a pending rollback below 2 withdraws the grant")
	assert.Equal(t, uint64(3), w.Revision())

	src.rec, src.rev = Record{Level: 2, Rollback: &featurelevel.Rollback{Target: 2}}, 4
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, w.Granted(), "a rollback that keeps level 2 keeps the grant")
}

// vpcd refuses a level above its build at start, before it serves anything,
// and retries any other failure until a read succeeds.
func TestLevelWatch_StartRefusesAboveBuildAndRetriesOtherwise(t *testing.T) {
	sleeps := 0
	prev := levelRetrySleep
	levelRetrySleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	t.Cleanup(func() { levelRetrySleep = prev })

	above := &levelWatch{read: (&fixedRecord{rec: Record{Level: featurelevel.Build + 1}, rev: 5}).read}
	err := above.start(t.Context())
	_, ok := errors.AsType[*LevelAboveBuildError](err)
	require.True(t, ok, "got %v", err)
	assert.Zero(t, sleeps, "a level above this build is never retried")
	assert.False(t, above.Granted())

	calls := 0
	flaky := &levelWatch{read: func(context.Context) (featurelevel.Record, uint64, error) {
		calls++
		if calls < 3 {
			return Record{}, 0, errors.New("no leader yet")
		}
		return Record{Level: 2}, 7, nil
	}}
	require.NoError(t, flaky.start(t.Context()))
	assert.Equal(t, 2, sleeps)
	assert.True(t, flaky.Granted())
}

// A level raised above this build while vpcd runs is refused and withdraws
// the grant.
func TestLevelWatch_ObserveAboveBuildWithdrawsTheGrant(t *testing.T) {
	src := &fixedRecord{rec: Record{Level: 2}, rev: 1}
	w := &levelWatch{read: src.read}
	require.NoError(t, w.observe(t.Context()))
	src.rec, src.rev = Record{Level: featurelevel.Build + 1}, 2
	err := w.observe(t.Context())
	_, ok := errors.AsType[*LevelAboveBuildError](err)
	require.True(t, ok)
	assert.False(t, w.Granted())
}

// Each grant flip wakes the reconcile pass once, so a rollback withdraws an
// installed prefix-list ACL without waiting for the resync; a re-read that
// changes nothing wakes nothing.
func TestLevelWatch_GrantFlipWakesTheReconcilePass(t *testing.T) {
	src := &fixedRecord{rec: Record{Level: 1}, rev: 1}
	w := &levelWatch{read: src.read, changed: make(chan struct{}, 1)}
	woken := func() bool {
		select {
		case <-w.changed:
			return true
		default:
			return false
		}
	}

	require.NoError(t, w.observe(t.Context()))
	assert.False(t, woken(), "level 1 grants nothing, so nothing flipped")
	src.rec, src.rev = Record{Level: 2}, 2
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, woken(), "the grant arriving wakes a pass")
	src.rev = 3
	require.NoError(t, w.observe(t.Context()))
	assert.False(t, woken(), "an unchanged grant wakes nothing")
	src.rec, src.rev = Record{Level: 2, Rollback: &featurelevel.Rollback{Target: 1}}, 4
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, woken(), "a rollback withdrawing the grant wakes a pass")
}

// An absent record is level 0; a level raised above this build while vpcd
// runs withdraws the grant and is handed to fatal, which stops vpcd.
func TestLevelWatch_RunStopsVpcdWhenTheLevelRisesAboveItsBuild(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	read := featureLevelRecord(js)
	rec, rev, err := read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Record{}, rec, "an absent bucket is level 0")
	assert.Zero(t, rev)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: featureLevelBucket})
	require.NoError(t, err)
	store := featurelevel.NewStore(kv)
	_, _, err = store.Advance(t.Context(), []featurelevel.Member{{Node: "n1", Level: featurelevel.Build, Current: true}})
	require.NoError(t, err)
	w := &levelWatch{read: read, changed: make(chan struct{}, 1)}
	require.NoError(t, w.start(t.Context()))
	require.True(t, w.Granted())
	<-w.changed

	fatal := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.run(ctx, js, func(err error) { fatal <- err })
	// A newer build's record: Advance caps at this build, so it is written raw.
	above, err := json.Marshal(Record{Level: featurelevel.Build + 1})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), featurelevel.Key, above)
	require.NoError(t, err)

	select {
	case err := <-fatal:
		_, above := errors.AsType[*LevelAboveBuildError](err)
		assert.True(t, above, "fatal gets the refusal: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("vpcd kept running above its build")
	}
	assert.False(t, w.Granted(), "the grant is withdrawn before vpcd stops")
	select {
	case <-w.changed:
	default:
		t.Fatal("the withdrawal wakes the reconcile pass")
	}
}
