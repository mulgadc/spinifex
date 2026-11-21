package readiness_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	ns *server.Server
	js jetstream.JetStream
	kv jetstream.KeyValue
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ns, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: readiness.KVBucket, History: 1})
	require.NoError(t, err)
	return fixture{ns: ns, js: js, kv: kv}
}

func (f fixture) control() *readiness.Control { return readiness.New(f.js, f.kv) }

// reconnect opens a second client on the same server, as a restarted daemon would.
func (f fixture) reconnect(t *testing.T) *readiness.Control {
	t.Helper()
	nc, err := nats.Connect(f.ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(t.Context(), readiness.KVBucket)
	require.NoError(t, err)
	return readiness.New(js, kv)
}

func read(t *testing.T, c *readiness.Control) (readiness.Record, uint64) {
	t.Helper()
	rec, rev, err := c.Read(t.Context())
	require.NoError(t, err)
	return rec, rev
}

// activate initializes the record and activates epoch 1.
func activate(t *testing.T, c *readiness.Control) {
	t.Helper()
	rev, err := c.Initialize(t.Context())
	require.NoError(t, err)
	_, err = c.Activate(t.Context(), 1, rev)
	require.NoError(t, err)
}

func freeze(t *testing.T, c *readiness.Control, rollback string) uint64 {
	t.Helper()
	_, rev := read(t, c)
	rev, err := c.Freeze(t.Context(), rollback, rev)
	require.NoError(t, err)
	return rev
}

func admit(t *testing.T, c *readiness.Control, id string) readiness.Token {
	t.Helper()
	tok, _, err := c.Admit(t.Context(), readiness.Token{ID: id, Kind: readiness.TokenLaunch, Node: "node-1", Target: "acct/model"})
	require.NoError(t, err)
	return tok
}

func journal(intent readiness.Intent) readiness.Journal {
	return readiness.Journal{Incarnation: "endpoints-1", Key: "acct.model", ExpectedRevision: 7, Intent: intent}
}

// pause blocks the first write of op until released.
type pause struct {
	op      string
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func newPause(op string) *pause {
	return &pause{op: op, reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *pause) hook(op string) {
	if op != p.op {
		return
	}
	first := false
	p.once.Do(func() { first = true })
	if first {
		close(p.reached)
		<-p.release
	}
}

// faultKV replaces the next Update with fault, once.
type faultKV struct {
	readiness.KV
	mu    sync.Mutex
	fault func(ctx context.Context, key string, value []byte, last uint64) (uint64, error)
}

func (f *faultKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	f.mu.Lock()
	fault := f.fault
	f.fault = nil
	f.mu.Unlock()
	if fault != nil {
		return fault(ctx, key, value, last)
	}
	return f.KV.Update(ctx, key, value, last)
}

func TestControl_TransitionsAdvanceEpochAndKeepTokens(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()

	_, _, err := c.Read(ctx)
	require.ErrorIs(t, err, readiness.ErrAbsent)
	rev, err := c.Initialize(ctx)
	require.NoError(t, err)
	_, err = c.Activate(ctx, 2, rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "only the recorded epoch can activate")
	rev, err = c.Activate(ctx, 1, rev)
	require.NoError(t, err)
	_, err = c.Activate(ctx, 1, rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "active cannot be activated again")

	admit(t, c, "launch-1")
	rev = freeze(t, c, "rb-1")
	_, err = c.Reopen(ctx, 1, rev)
	require.NoError(t, err)

	rec, rev := read(t, c)
	assert.Equal(t, uint64(2), rec.Epoch)
	assert.Equal(t, readiness.PhasePreparing, rec.Phase)
	assert.Empty(t, rec.Rollback)
	require.Contains(t, rec.Tokens, "launch-1", "reopen must never clear an unresolved token")
	assert.Equal(t, uint64(1), rec.Tokens["launch-1"].Epoch)

	_, err = c.Activate(ctx, 1, rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "the old epoch cannot be reactivated")
	_, err = c.Activate(ctx, 2, rev)
	require.NoError(t, err)
}

func TestControl_InitializeRefusesExistingOrDeletedRecord(t *testing.T) {
	f := newFixture(t)
	c := f.control()
	ctx := t.Context()

	activate(t, c)
	_, err := c.Initialize(ctx)
	require.ErrorIs(t, err, readiness.ErrWrongState)

	require.NoError(t, f.kv.Delete(ctx, readiness.Key))
	_, err = f.kv.Get(ctx, readiness.Key)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "an ordinary get hides the delete marker")

	_, _, err = c.Read(ctx)
	require.ErrorIs(t, err, readiness.ErrDeleted)
	_, err = c.Initialize(ctx)
	require.ErrorIs(t, err, readiness.ErrDeleted, "a deleted record must not be mistaken for a fresh one")
	_, _, err = c.Admit(ctx, readiness.Token{ID: "launch-1", Kind: readiness.TokenLaunch})
	require.ErrorIs(t, err, readiness.ErrDeleted)
}

func TestControl_InitializeLosesToConcurrentInitialization(t *testing.T) {
	f := newFixture(t)
	slow := f.control()
	p := newPause("initialize")
	readiness.SetBeforeWriteHook(slow, p.hook)

	errc := make(chan error, 1)
	go func() {
		_, err := slow.Initialize(t.Context())
		errc <- err
	}()
	<-p.reached
	fast := f.control()
	activate(t, fast)
	close(p.release)

	require.ErrorIs(t, <-errc, readiness.ErrChanged, "a revision-zero write is single-attempt")
	rec, _ := read(t, fast)
	assert.Equal(t, readiness.PhaseActive, rec.Phase, "the slow initializer must not overwrite the active record")
}

func TestControl_AdmissionPausedAcrossFreezeIsRefused(t *testing.T) {
	f := newFixture(t)
	worker := f.control()
	activate(t, worker)
	p := newPause("admit")
	readiness.SetBeforeWriteHook(worker, p.hook)

	errc := make(chan error, 1)
	go func() {
		_, _, err := worker.Admit(t.Context(), readiness.Token{ID: "launch-late", Kind: readiness.TokenLaunch})
		errc <- err
	}()
	<-p.reached
	rollback := f.control()
	freeze(t, rollback, "rb-1")
	close(p.release)

	require.ErrorIs(t, <-errc, readiness.ErrNotAdmitted, "the retry must re-read the frozen phase")
	rec, _ := read(t, rollback)
	assert.Equal(t, readiness.PhaseFrozen, rec.Phase)
	assert.Empty(t, rec.Tokens, "a token add delayed past the freeze must not land")
}

func TestControl_FreezeRewritesFrozenRecordAndFencesStaleWriters(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	activate(t, c)

	firstFreeze := freeze(t, c, "rb-1")
	secondFreeze := freeze(t, c, "rb-2")
	require.Greater(t, secondFreeze, firstFreeze, "freezing a frozen record must still move its revision")

	_, err := c.Reopen(ctx, 1, firstFreeze)
	require.ErrorIs(t, err, readiness.ErrChanged, "an unfreeze read before the second rollback must fail")
	_, err = c.Finalize(ctx, "rb-1", secondFreeze)
	require.ErrorIs(t, err, readiness.ErrWrongState, "only the current rollback can finalize")

	rec, _ := read(t, c)
	assert.Equal(t, "rb-2", rec.Rollback)
	assert.Equal(t, readiness.PhaseFrozen, rec.Phase)
}

func TestControl_CancelledRollbackRunnerCannotFreezeNewEpoch(t *testing.T) {
	f := newFixture(t)
	runner := f.control()
	ctx := t.Context()
	activate(t, runner)

	frozenAt := freeze(t, runner, "rb-cancelled")
	operator := f.control()
	rev, err := operator.Reopen(ctx, 1, frozenAt)
	require.NoError(t, err)
	_, err = operator.Activate(ctx, 2, rev)
	require.NoError(t, err)

	_, err = runner.Freeze(ctx, "rb-cancelled", frozenAt)
	require.ErrorIs(t, err, readiness.ErrChanged)
	_, err = runner.Finalize(ctx, "rb-cancelled", frozenAt)
	require.ErrorIs(t, err, readiness.ErrChanged)
	_, _, err = runner.AdmitDrain(ctx, "rb-cancelled", readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted)

	rec, _ := read(t, operator)
	assert.Equal(t, readiness.PhaseActive, rec.Phase)
	assert.Equal(t, uint64(2), rec.Epoch)
	assert.Empty(t, rec.Tokens)
}

func TestControl_FinalizeFencedByDrainAdmittedAfterZeroTokenRead(t *testing.T) {
	f := newFixture(t)
	runner := f.control()
	ctx := t.Context()
	activate(t, runner)
	freeze(t, runner, "rb-1")

	rec, zeroTokensAt := read(t, runner)
	require.Empty(t, rec.Tokens)
	drainer := f.control()
	_, _, err := drainer.AdmitDrain(ctx, "rb-1", readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain})
	require.NoError(t, err)

	_, err = runner.Finalize(ctx, "rb-1", zeroTokensAt)
	require.ErrorIs(t, err, readiness.ErrChanged, "a token admitted after the zero-token read must force a fresh inventory")

	_, err = drainer.Release(ctx, "drain-1")
	require.NoError(t, err)
	_, rev := read(t, runner)
	_, err = runner.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err)

	_, _, err = drainer.AdmitDrain(ctx, "rb-1", readiness.Token{ID: "drain-2", Kind: readiness.TokenDrain})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "finalizing admits no drain work")
	_, _, err = drainer.Admit(ctx, readiness.Token{ID: "launch-1", Kind: readiness.TokenLaunch})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "finalizing admits no new work")
}

func TestControl_FrozenTokensSettleButCannotClaimOrHold(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	activate(t, c)
	admit(t, c, "launch-1")
	_, err := c.JournalWrite(ctx, "launch-1", journal(readiness.IntentClaim))
	require.NoError(t, err)

	_, _, err = c.AdmitDrain(ctx, "rb-1", readiness.Token{ID: "drain-early", Kind: readiness.TokenDrain})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "drain admission needs the frozen rollback")
	freeze(t, c, "rb-1")

	for _, intent := range []readiness.Intent{readiness.IntentClaim, readiness.IntentHold} {
		_, err = c.JournalWrite(ctx, "launch-1", journal(intent))
		require.ErrorIs(t, err, readiness.ErrNotAdmitted, intent)
	}
	_, err = c.JournalWrite(ctx, "launch-1", journal(readiness.IntentSettle))
	require.NoError(t, err, "admitted work may still make its launch safe")

	_, _, err = c.AdmitDrain(ctx, "rb-other", readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted)
	_, _, err = c.AdmitDrain(ctx, "rb-1", readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain})
	require.NoError(t, err)
	_, err = c.JournalWrite(ctx, "drain-1", journal(readiness.IntentHold))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "a drain token never adds a hold")
	_, err = c.JournalWrite(ctx, "drain-1", journal(readiness.IntentSettle))
	require.NoError(t, err)

	rec, rev := read(t, c)
	assert.Equal(t, []readiness.Journal{journal(readiness.IntentClaim), journal(readiness.IntentSettle)}, rec.Tokens["launch-1"].Journal)
	assert.Equal(t, "rb-1", rec.Tokens["drain-1"].Rollback)
	_, err = c.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)

	for _, id := range []string{"launch-1", "drain-1"} {
		_, err = c.Release(ctx, id)
		require.NoError(t, err)
	}
	_, rev = read(t, c)
	_, err = c.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err)
}

func TestControl_ReopenedEpochOnlyLetsOldTokensSettle(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	activate(t, c)
	admit(t, c, "launch-old")
	rev, err := c.Reopen(ctx, 1, freeze(t, c, "rb-1"))
	require.NoError(t, err)
	_, err = c.Activate(ctx, 2, rev)
	require.NoError(t, err)

	_, err = c.JournalWrite(ctx, "launch-old", journal(readiness.IntentClaim))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "an earlier epoch's token cannot start a claim")
	_, err = c.JournalWrite(ctx, "launch-old", journal(readiness.IntentSettle))
	require.NoError(t, err)

	tok := admit(t, c, "launch-new")
	assert.Equal(t, uint64(2), tok.Epoch)
	_, err = c.JournalWrite(ctx, "launch-new", journal(readiness.IntentClaim))
	require.NoError(t, err)
}

func TestControl_UnknownAdmitOutcomeRetainsTokenAcrossRestart(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	activate(t, f.control())

	lossy := &faultKV{KV: f.kv}
	lossy.fault = func(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
		if _, err := f.kv.Update(ctx, key, value, last); err != nil {
			return 0, err
		}
		return 0, context.DeadlineExceeded
	}
	_, _, err := readiness.New(f.js, lossy).Admit(ctx, readiness.Token{ID: "launch-1", Kind: readiness.TokenLaunch})
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

	restarted := f.reconnect(t)
	rec, _ := read(t, restarted)
	require.Contains(t, rec.Tokens, "launch-1", "a landed add whose reply was lost stays recorded")
	freeze(t, restarted, "rb-1")
	_, rev := read(t, restarted)
	_, err = restarted.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "an uncertain token blocks rollback")
}

func TestControl_UnknownReleaseOutcomeKeepsToken(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	c := f.control()
	activate(t, c)
	admit(t, c, "launch-1")
	freeze(t, c, "rb-1")

	lossy := &faultKV{KV: f.kv}
	lossy.fault = func(context.Context, string, []byte, uint64) (uint64, error) {
		return 0, nats.ErrTimeout
	}
	_, err := readiness.New(f.js, lossy).Release(ctx, "launch-1")
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

	rec, rev := read(t, f.reconnect(t))
	require.Contains(t, rec.Tokens, "launch-1")
	_, err = c.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
}

func TestControl_AdmitRefusedUnlessActive(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	tok := readiness.Token{ID: "launch-1", Kind: readiness.TokenLaunch}

	_, _, err := c.Admit(ctx, tok)
	require.ErrorIs(t, err, readiness.ErrAbsent, "an absent record refuses work")
	_, err = c.Initialize(ctx)
	require.NoError(t, err)
	_, _, err = c.Admit(ctx, tok)
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "preparing refuses work")
	_, _, err = c.Admit(ctx, readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain})
	require.ErrorIs(t, err, readiness.ErrInvalid, "drain tokens use drain admission")
}

func TestControl_ConcurrentAdmissionsAreAllRecorded(t *testing.T) {
	f := newFixture(t)
	activate(t, f.control())

	const workers = 4
	var wg sync.WaitGroup
	errs := make([]error, workers)
	controls := make([]*readiness.Control, workers)
	for i := range workers {
		controls[i] = f.reconnect(t)
	}
	for i := range workers {
		wg.Go(func() {
			_, _, errs[i] = controls[i].Admit(t.Context(), readiness.Token{ID: fmt.Sprintf("launch-%d", i), Kind: readiness.TokenLaunch})
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "worker %d", i)
	}
	rec, _ := read(t, f.control())
	assert.Len(t, rec.Tokens, workers)
}
