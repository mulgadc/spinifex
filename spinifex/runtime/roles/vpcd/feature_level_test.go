package vpcd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
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

// Prefix-list peers grant only while level 3 is active: a pending rollback
// below it withdraws the grant at the revision it is observed, and a failed
// read changes nothing.
func TestLevelWatch_GrantFollowsLevel3(t *testing.T) {
	src := &fixedRecord{rec: Record{Level: 2}, rev: 1}
	w := &levelWatch{read: src.read}
	assert.False(t, w.Granted(), "nothing is granted before the first read")

	require.NoError(t, w.observe(t.Context()))
	assert.False(t, w.Granted())

	src.rec, src.rev = Record{Level: 3}, 2
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, w.Granted())

	src.err = errors.New("leader unreachable")
	require.Error(t, w.observe(t.Context()))
	assert.True(t, w.Granted(), "an unreadable record keeps the last view")
	assert.Equal(t, uint64(2), w.Revision())

	src.err = nil
	src.rec, src.rev = Record{Level: 3, Rollback: &featurelevel.Rollback{Target: 2}}, 3
	require.NoError(t, w.observe(t.Context()))
	assert.False(t, w.Granted(), "a pending rollback below 3 withdraws the grant")
	assert.Equal(t, uint64(3), w.Revision())

	src.rec, src.rev = Record{Level: 3, Rollback: &featurelevel.Rollback{Target: 3}}, 4
	require.NoError(t, w.observe(t.Context()))
	assert.True(t, w.Granted(), "a rollback that keeps level 3 keeps the grant")
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
		return Record{Level: 3}, 7, nil
	}}
	require.NoError(t, flaky.start(t.Context()))
	assert.Equal(t, 2, sleeps)
	assert.True(t, flaky.Granted())
}

// A level raised above this build while vpcd runs is refused and withdraws
// the grant.
func TestLevelWatch_ObserveAboveBuildWithdrawsTheGrant(t *testing.T) {
	src := &fixedRecord{rec: Record{Level: 3}, rev: 1}
	w := &levelWatch{read: src.read}
	require.NoError(t, w.observe(t.Context()))
	src.rec, src.rev = Record{Level: featurelevel.Build + 1}, 2
	err := w.observe(t.Context())
	_, ok := errors.AsType[*LevelAboveBuildError](err)
	require.True(t, ok)
	assert.False(t, w.Granted())
}
