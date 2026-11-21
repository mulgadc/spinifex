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

func launch(id string, epoch uint64) readiness.Token {
	return readiness.Token{ID: id, Kind: readiness.TokenLaunch, Epoch: epoch, Node: "node-1", Target: "acct/model"}
}

func drain(id string, epoch uint64) readiness.Token {
	return readiness.Token{ID: id, Kind: readiness.TokenDrain, Epoch: epoch, Node: "node-1"}
}

func admit(t *testing.T, c *readiness.Control, id string, epoch uint64) *readiness.Operation {
	t.Helper()
	op, _, err := c.Admit(t.Context(), launch(id, epoch))
	require.NoError(t, err)
	return op
}

func admitDrain(t *testing.T, c *readiness.Control, rollback, id string, epoch uint64) *readiness.Operation {
	t.Helper()
	op, _, err := c.AdmitDrain(t.Context(), rollback, drain(id, epoch))
	require.NoError(t, err)
	return op
}

func release(t *testing.T, op *readiness.Operation) {
	t.Helper()
	_, err := op.Release(t.Context())
	require.NoError(t, err)
}

// landThenLoseReply performs the write and then reports a lost reply.
func landThenLoseReply(kv jetstream.KeyValue) func(context.Context, string, []byte, uint64) (uint64, error) {
	return func(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
		if _, err := kv.Update(ctx, key, value, last); err != nil {
			return 0, err
		}
		return 0, context.DeadlineExceeded
	}
}

// loseBeforeLanding reports a timeout without sending the write.
func loseBeforeLanding(context.Context, string, []byte, uint64) (uint64, error) {
	return 0, nats.ErrTimeout
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


// setFault arms fault for the next Update.
func (f *faultKV) setFault(fault func(context.Context, string, []byte, uint64) (uint64, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault = fault
}

func TestControl_TransitionsAdvanceEpochOnlyAfterTokensSettle(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()

	_, _, err := c.Read(ctx)
	require.ErrorIs(t, err, readiness.ErrAbsent)
	rev, err := c.Initialize(ctx)
	require.NoError(t, err)
	rec, _ := read(t, c)
	assert.Equal(t, readiness.SchemaVersion, rec.Version)
	_, err = c.Activate(ctx, 2, rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "only the recorded epoch can activate")
	rev, err = c.Activate(ctx, 1, rev)
	require.NoError(t, err)
	_, err = c.Activate(ctx, 1, rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "active cannot be activated again")

	op := admit(t, c, "launch-1", 1)
	rev = freeze(t, c, "rb-1")
	_, err = c.Reopen(ctx, 1, rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "an unresolved token keeps the record frozen")

	release(t, op)
	_, rev = read(t, c)
	_, err = c.Reopen(ctx, 1, rev)
	require.NoError(t, err)

	rec, rev = read(t, c)
	assert.Equal(t, uint64(2), rec.Epoch)
	assert.Equal(t, readiness.PhasePreparing, rec.Phase)
	assert.Empty(t, rec.Rollback)
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
	_, _, err = c.Admit(ctx, launch("launch-1", 1))
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
		_, _, err := worker.Admit(t.Context(), launch("launch-late", 1))
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

func TestControl_AdmissionPausedAcrossEpochChangeIsRefused(t *testing.T) {
	f := newFixture(t)
	worker := f.control()
	activate(t, worker)
	p := newPause("admit")
	readiness.SetBeforeWriteHook(worker, p.hook)

	errc := make(chan error, 1)
	go func() {
		_, _, err := worker.Admit(t.Context(), launch("launch-epoch-1", 1))
		errc <- err
	}()
	<-p.reached
	operator := f.control()
	rev, err := operator.Reopen(t.Context(), 1, freeze(t, operator, "rb-1"))
	require.NoError(t, err)
	_, err = operator.Activate(t.Context(), 2, rev)
	require.NoError(t, err)
	close(p.release)

	require.ErrorIs(t, <-errc, readiness.ErrNotAdmitted, "an admission authorized in epoch 1 must not land in epoch 2")
	rec, _ := read(t, operator)
	assert.Equal(t, readiness.PhaseActive, rec.Phase)
	assert.Equal(t, uint64(2), rec.Epoch)
	assert.Empty(t, rec.Tokens)
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

func TestControl_SecondRunnerCannotReopenFinalizingForDrain(t *testing.T) {
	f := newFixture(t)
	first := f.control()
	second := f.control()
	ctx := t.Context()
	activate(t, first)
	freeze(t, first, "rb-1")

	rec, zeroTokensAt := read(t, first)
	require.Empty(t, rec.Tokens)
	_, err := first.Finalize(ctx, "rb-1", zeroTokensAt)
	require.NoError(t, err)

	_, rev := read(t, second)
	_, err = second.Freeze(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrWrongState, "a second runner must not refreeze a finalizing record")
	_, _, err = second.AdmitDrain(ctx, "rb-1", drain("drain-1", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "finalizing admits no drain work")
	_, err = second.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrWrongState)

	rec, _ = read(t, first)
	assert.Equal(t, readiness.PhaseFinalizing, rec.Phase)
	assert.Empty(t, rec.Tokens, "the first runner's zero-token evidence must still hold")
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
	_, _, err = runner.AdmitDrain(ctx, "rb-cancelled", drain("drain-1", 1))
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
	op := admitDrain(t, drainer, "rb-1", "drain-1", 1)

	_, err := runner.Finalize(ctx, "rb-1", zeroTokensAt)
	require.ErrorIs(t, err, readiness.ErrChanged, "a token admitted after the zero-token read must force a fresh inventory")

	release(t, op)
	_, rev := read(t, runner)
	_, err = runner.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err)

	_, _, err = drainer.AdmitDrain(ctx, "rb-1", drain("drain-2", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "finalizing admits no drain work")
	_, _, err = drainer.Admit(ctx, launch("launch-1", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "finalizing admits no new work")
}

func TestControl_FrozenTokensSettleButCannotClaimOrHold(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	activate(t, c)
	launchOp := admit(t, c, "launch-1", 1)
	_, err := launchOp.Journal(ctx, journal(readiness.IntentClaim))
	require.NoError(t, err)

	_, _, err = c.AdmitDrain(ctx, "rb-1", drain("drain-early", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "drain admission needs the frozen rollback")
	freeze(t, c, "rb-1")

	for _, intent := range []readiness.Intent{readiness.IntentClaim, readiness.IntentHold} {
		_, err = launchOp.Journal(ctx, journal(intent))
		require.ErrorIs(t, err, readiness.ErrNotAdmitted, intent)
	}
	_, err = launchOp.Journal(ctx, journal(readiness.IntentSettle))
	require.NoError(t, err, "admitted work may still make its launch safe")

	_, _, err = c.AdmitDrain(ctx, "rb-other", drain("drain-1", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted)
	_, _, err = c.AdmitDrain(ctx, "rb-1", drain("drain-1", 2))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "drain admission is bound to its epoch")
	drainOp := admitDrain(t, c, "rb-1", "drain-1", 1)
	_, err = drainOp.Journal(ctx, journal(readiness.IntentHold))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "a drain token never adds a hold")
	_, err = drainOp.Journal(ctx, journal(readiness.IntentSettle))
	require.NoError(t, err)

	rec, rev := read(t, c)
	assert.Equal(t, []readiness.Journal{journal(readiness.IntentClaim), journal(readiness.IntentSettle)}, rec.Tokens["launch-1"].Journal)
	assert.Equal(t, "rb-1", rec.Tokens["drain-1"].Rollback)
	_, err = c.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)

	release(t, launchOp)
	release(t, drainOp)
	_, rev = read(t, c)
	_, err = c.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err)
}

func TestControl_FrozenSettlementThenReopenStartsCleanEpoch(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()
	activate(t, c)
	old := admit(t, c, "launch-old", 1)
	_, err := old.Journal(ctx, journal(readiness.IntentClaim))
	require.NoError(t, err)
	frozenAt := freeze(t, c, "rb-1")

	_, err = c.Reopen(ctx, 1, frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
	_, err = old.Journal(ctx, journal(readiness.IntentSettle))
	require.NoError(t, err, "recovery settles the outstanding launch while frozen")
	release(t, old)

	_, rev := read(t, c)
	rev, err = c.Reopen(ctx, 1, rev)
	require.NoError(t, err)
	_, err = c.Activate(ctx, 2, rev)
	require.NoError(t, err)

	_, _, err = c.Admit(ctx, launch("launch-stale", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "an epoch 1 authorization is spent")
	fresh := admit(t, c, "launch-new", 2)
	_, err = fresh.Journal(ctx, journal(readiness.IntentClaim))
	require.NoError(t, err)
}

func TestControl_ActivateRefusesPreparingRecordWithTokens(t *testing.T) {
	f := newFixture(t)
	rev, err := f.kv.Put(t.Context(), readiness.Key, []byte(
		`{"version":1,"epoch":3,"phase":"preparing","tokens":{"launch-1":{"id":"launch-1","kind":"launch","epoch":2}}}`))
	require.NoError(t, err)

	_, err = f.control().Activate(t.Context(), 3, rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
}

func TestControl_UnsupportedRecordFailsClosedWithoutRewrite(t *testing.T) {
	cases := map[string]string{
		"newer version":         `{"version":2,"epoch":1,"phase":"active"}`,
		"missing version":       `{"epoch":1,"phase":"active"}`,
		"unknown record field":  `{"version":1,"epoch":1,"phase":"active","lease":"x"}`,
		"unknown token field":   `{"version":1,"epoch":1,"phase":"active","tokens":{"t":{"id":"t","kind":"launch","epoch":1,"owner":"x"}}}`,
		"unknown phase":         `{"version":1,"epoch":1,"phase":"draining"}`,
		"unknown token kind":    `{"version":1,"epoch":1,"phase":"active","tokens":{"t":{"id":"t","kind":"evict","epoch":1}}}`,
		"unknown intent":        `{"version":1,"epoch":1,"phase":"active","tokens":{"t":{"id":"t","kind":"launch","epoch":1,"journal":[{"incarnation":"i","key":"k","expected_revision":1,"intent":"delete"}]}}}`,
		"token id mismatch":     `{"version":1,"epoch":1,"phase":"active","tokens":{"t":{"id":"u","kind":"launch","epoch":1}}}`,
		"undecodable record":    `not json`,
		"wrongly typed version": `{"version":"1","epoch":1,"phase":"active"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			owner := f.control()
			activate(t, owner)
			journalOp := admit(t, owner, "journal-op", 1)
			releaseOp := admit(t, owner, "release-op", 1)
			rev, err := f.kv.Put(ctx, readiness.Key, []byte(raw))
			require.NoError(t, err)

			_, _, err = f.control().Read(ctx)
			require.ErrorIs(t, err, readiness.ErrUnsupported)
			mutations := map[string]func(c *readiness.Control) error{
				"initialize": func(c *readiness.Control) error { _, err := c.Initialize(ctx); return err },
				"activate":   func(c *readiness.Control) error { _, err := c.Activate(ctx, 1, rev); return err },
				"freeze":     func(c *readiness.Control) error { _, err := c.Freeze(ctx, "rb-1", rev); return err },
				"finalize":   func(c *readiness.Control) error { _, err := c.Finalize(ctx, "rb-1", rev); return err },
				"reopen":     func(c *readiness.Control) error { _, err := c.Reopen(ctx, 1, rev); return err },
				"admit":      func(c *readiness.Control) error { _, _, err := c.Admit(ctx, launch("launch-1", 1)); return err },
				"drain":      func(c *readiness.Control) error { _, _, err := c.AdmitDrain(ctx, "rb-1", drain("drain-1", 1)); return err },
				"journal":    func(*readiness.Control) error { _, err := journalOp.Journal(ctx, journal(readiness.IntentSettle)); return err },
				"release":    func(*readiness.Control) error { _, err := releaseOp.Release(ctx); return err },
			}
			for op, mutate := range mutations {
				require.ErrorIs(t, mutate(f.control()), readiness.ErrUnsupported, op)
			}

			entry, err := f.kv.Get(ctx, readiness.Key)
			require.NoError(t, err)
			assert.Equal(t, rev, entry.Revision(), "an unsupported record must never be rewritten")
			assert.Equal(t, raw, string(entry.Value()))
		})
	}
}

func TestControl_UnknownAdmitOutcomeRetainsTokenAcrossRestart(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	activate(t, f.control())

	lossy := &faultKV{KV: f.kv}
	lossy.setFault(landThenLoseReply(f.kv))
	op, _, err := readiness.New(f.js, lossy).Admit(ctx, launch("launch-1", 1))
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
	assert.Nil(t, op, "an uncertain admission gives the owner no handle to start work with")

	restarted := f.reconnect(t)
	rec, _ := read(t, restarted)
	require.Contains(t, rec.Tokens, "launch-1", "a landed add whose reply was lost stays recorded")
	frozenAt := freeze(t, restarted, "rb-1")
	_, err = restarted.Finalize(ctx, "rb-1", frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "an uncertain token blocks rollback")
	_, err = restarted.Reopen(ctx, 1, frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "an uncertain token keeps the record frozen")
}

// ownedLaunch admits launch-1 through a Control whose next write can be faulted,
// then freezes the record for rb-1.
func ownedLaunch(t *testing.T, f fixture) (*faultKV, *readiness.Operation) {
	t.Helper()
	activate(t, f.control())
	lossy := &faultKV{KV: f.kv}
	op := admit(t, readiness.New(f.js, lossy), "launch-1", 1)
	freeze(t, f.control(), "rb-1")
	return lossy, op
}

func TestControl_ReleaseLandedWithLostReply(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	lossy, op := ownedLaunch(t, f)

	lossy.setFault(landThenLoseReply(f.kv))
	_, err := op.Release(ctx)
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

	fresh := f.reconnect(t)
	rec, rev := read(t, fresh)
	assert.NotContains(t, rec.Tokens, "launch-1", "an unknown release may have removed the token")
	_, err = fresh.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err, "a landed release lets rollback finish")

	_, err = op.Journal(ctx, journal(readiness.IntentSettle))
	require.ErrorIs(t, err, readiness.ErrTokenReleased, "the owner resumes no work after a release attempt")
}

func TestControl_ReleaseLostBeforeLanding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	lossy, op := ownedLaunch(t, f)

	lossy.setFault(loseBeforeLanding)
	_, err := op.Release(ctx)
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

	fresh := f.reconnect(t)
	rec, rev := read(t, fresh)
	require.Contains(t, rec.Tokens, "launch-1", "an unlanded release leaves the token recorded")
	_, err = fresh.Finalize(ctx, "rb-1", rev)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)

	_, err = op.Journal(ctx, journal(readiness.IntentSettle))
	require.ErrorIs(t, err, readiness.ErrTokenReleased, "the owner resumes no work after a release attempt")
	release(t, op)

	rec, rev = read(t, fresh)
	assert.NotContains(t, rec.Tokens, "launch-1", "only the release itself is retried")
	_, err = fresh.Finalize(ctx, "rb-1", rev)
	require.NoError(t, err)
}

func TestControl_AdmitRefusedUnlessActiveOrForAnExistingID(t *testing.T) {
	c := newFixture(t).control()
	ctx := t.Context()

	_, _, err := c.Admit(ctx, launch("launch-1", 1))
	require.ErrorIs(t, err, readiness.ErrAbsent, "an absent record refuses work")
	_, err = c.Initialize(ctx)
	require.NoError(t, err)
	_, _, err = c.Admit(ctx, launch("launch-1", 1))
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "preparing refuses work")
	_, _, err = c.Admit(ctx, drain("drain-1", 1))
	require.ErrorIs(t, err, readiness.ErrInvalid, "drain tokens use drain admission")
	_, _, err = c.Admit(ctx, launch("launch-2", 0))
	require.ErrorIs(t, err, readiness.ErrInvalid, "an admission names the epoch it was authorized for")

	_, rev := read(t, c)
	_, err = c.Activate(ctx, 1, rev)
	require.NoError(t, err)
	admit(t, c, "launch-1", 1)
	_, _, err = c.Admit(ctx, launch("launch-1", 1))
	require.ErrorIs(t, err, readiness.ErrTokenExists)
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
			_, _, errs[i] = controls[i].Admit(t.Context(), launch(fmt.Sprintf("launch-%d", i), 1))
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "worker %d", i)
	}
	rec, _ := read(t, f.control())
	assert.Len(t, rec.Tokens, workers)
}
