package readiness_test

import (
	"context"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardEnv is a bound deployment with an active control record written
// through a guarded Control whose publishes can be faulted.
type guardEnv struct {
	js    jetstream.JetStream
	bound *readiness.Bound
	lossy *faultJS
	c     *readiness.Control
}

func newGuardEnv(t *testing.T) guardEnv {
	t.Helper()
	js := initEnv(t)
	_, err := newInit(js).Initialize(t.Context(), firstRollout)
	require.NoError(t, err)
	b, err := newInit(js).Load(t.Context())
	require.NoError(t, err)
	lossy := &faultJS{JetStream: js}
	c, err := readiness.NewGuarded(lossy, b, readiness.Rules{})
	require.NoError(t, err)
	activate(t, c)
	return guardEnv{js: js, bound: b, lossy: lossy, c: c}
}

// raw reads and writes the control record with no binding checks, as an
// operator inspecting the store would.
func (g guardEnv) raw() *readiness.Control { return readiness.New(g.js, g.bound.ControlKV) }

// controlSeq is the control stream's last sequence, which moves on any write.
func controlSeq(t *testing.T, js jetstream.JetStream) uint64 {
	t.Helper()
	st, err := js.Stream(t.Context(), "KV_"+readiness.KVBucket)
	require.NoError(t, err)
	info, err := st.Info(t.Context())
	require.NoError(t, err)
	return info.State.LastSeq
}

// requireUnconfirmed checks an error that must stop the owner: it carries the
// binding failure and is never a conflict the update loop would retry.
func requireUnconfirmed(t *testing.T, err, binding error) {
	t.Helper()
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
	require.ErrorIs(t, err, binding)
	require.NotErrorIs(t, err, readiness.ErrChanged)
}

func TestGuarded_HealthyOperationSendsEachWriteOnce(t *testing.T) {
	g := newGuardEnv(t)
	ctx := t.Context()
	op := admit(t, g.c, "launch-1", 1)
	_, err := op.Journal(ctx, journal(readiness.IntentClaim))
	require.NoError(t, err)
	release(t, op)
	frozenAt := freeze(t, g.c, "rb-1")
	_, err = g.c.Finalize(ctx, "rb-1", frozenAt)
	require.NoError(t, err)

	assert.Equal(t, 7, g.lossy.publishes(), "initialize, activate, admit, journal, release, freeze and finalize, once each")
	rec, _ := read(t, g.raw())
	assert.Equal(t, readiness.PhaseFinalizing, rec.Phase)
	assert.Empty(t, rec.Tokens)
}

func TestGuarded_BindingLostDuringAReadIsNeverInterpreted(t *testing.T) {
	cases := map[string]struct {
		disturb func(t *testing.T, js jetstream.JetStream)
		// before disturbs and latches the quarantine before the read is sent.
		before bool
		want   error
	}{
		"endpoint stream replaced": {disturb: func(t *testing.T, js jetstream.JetStream) { recreate(t, js, endpointBucket) }, want: readiness.ErrStreamReplaced},
		"control stream replaced":  {disturb: func(t *testing.T, js jetstream.JetStream) { recreate(t, js, readiness.KVBucket) }, want: readiness.ErrStreamReplaced},
		"endpoint stream deleted": {disturb: func(t *testing.T, js jetstream.JetStream) {
			require.NoError(t, js.DeleteKeyValue(t.Context(), endpointBucket))
		}, want: readiness.ErrStreamMissing},
		"quarantined before the read": {disturb: func(t *testing.T, js jetstream.JetStream) { recreate(t, js, endpointBucket) }, before: true, want: readiness.ErrStreamReplaced},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGuardEnv(t)
			ctx := t.Context()
			reads := 0
			var once sync.Once
			readiness.SetAfterReadHook(g.c, func() {
				reads++
				once.Do(func() { tc.disturb(t, g.js) })
			})
			if tc.before {
				once.Do(func() { tc.disturb(t, g.js) })
				require.ErrorIs(t, g.bound.Endpoints.Verify(ctx), tc.want)
			}
			sent := g.lossy.publishes()

			op, _, err := g.c.Admit(ctx, launch("launch-1", 1))
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, readiness.ErrChanged)
			assert.Nil(t, op, "a read that cannot be trusted admits no work")
			assert.Equal(t, sent, g.lossy.publishes(), "no write follows an untrusted read")
			if tc.before {
				assert.Zero(t, reads, "a quarantined binding is not read")
			} else {
				assert.Equal(t, 1, reads)
			}

			_, _, err = g.c.Read(ctx)
			require.ErrorIs(t, err, tc.want, "the handle stays quarantined")
			_, err = g.c.Initialize(ctx)
			require.ErrorIs(t, err, tc.want, "a replaced control stream is never initialized again")
			assert.Equal(t, sent, g.lossy.publishes())
		})
	}
}

// publishThenReplace sends the publish for real, then optionally runs a
// competing write first, and replaces bucket before the answer is checked.
func publishThenReplace(t *testing.T, bucket string, competing func()) publishFault {
	return func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		if competing != nil {
			competing()
		}
		ack, err := js.PublishMsg(ctx, m, opts...)
		recreate(t, js, bucket)
		return ack, err
	}
}

func TestGuarded_ReplacementAfterAnAnswerLeavesTheWriteUnconfirmed(t *testing.T) {
	t.Run("acknowledged admit", func(t *testing.T) {
		g := newGuardEnv(t)
		sent := g.lossy.publishes()
		g.lossy.setFault(publishThenReplace(t, endpointBucket, nil))

		op, _, err := g.c.Admit(t.Context(), launch("launch-1", 1))
		requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
		assert.Nil(t, op, "an unconfirmed admission gives the owner no handle")
		assert.Equal(t, sent+1, g.lossy.publishes())
		rec, _ := read(t, g.raw())
		assert.Contains(t, rec.Tokens, "launch-1", "the write landed, so its token stays")

		_, _, err = g.c.Admit(t.Context(), launch("launch-2", 1))
		require.ErrorIs(t, err, readiness.ErrStreamReplaced)
		assert.Equal(t, sent+1, g.lossy.publishes(), "no follow-on write")
	})

	t.Run("acknowledged admit, control stream replaced", func(t *testing.T) {
		g := newGuardEnv(t)
		sent := g.lossy.publishes()
		g.lossy.setFault(publishThenReplace(t, readiness.KVBucket, nil))

		op, _, err := g.c.Admit(t.Context(), launch("launch-1", 1))
		requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
		assert.Nil(t, op)
		_, err = g.c.Initialize(t.Context())
		require.ErrorIs(t, err, readiness.ErrStreamReplaced, "the empty replacement is never initialized")
		assert.Equal(t, sent+1, g.lossy.publishes())
	})

	t.Run("reported conflict", func(t *testing.T) {
		g := newGuardEnv(t)
		sent := g.lossy.publishes()
		g.lossy.setFault(publishThenReplace(t, endpointBucket, func() { admit(t, g.raw(), "other", 1) }))

		op, _, err := g.c.Admit(t.Context(), launch("launch-1", 1))
		requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
		assert.Nil(t, op)
		assert.Equal(t, sent+1, g.lossy.publishes(), "a conflict under a changed binding is not retried")
		rec, _ := read(t, g.raw())
		assert.Contains(t, rec.Tokens, "other")
		assert.NotContains(t, rec.Tokens, "launch-1")
	})

	t.Run("acknowledged journal", func(t *testing.T) {
		g := newGuardEnv(t)
		ctx := t.Context()
		op := admit(t, g.c, "launch-1", 1)
		sent := g.lossy.publishes()
		g.lossy.setFault(publishThenReplace(t, endpointBucket, nil))

		_, err := op.Journal(ctx, journal(readiness.IntentClaim))
		requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
		_, err = op.Journal(ctx, journal(readiness.IntentSettle))
		require.ErrorIs(t, err, readiness.ErrTokenFenced)
		_, err = op.Release(ctx)
		require.ErrorIs(t, err, readiness.ErrTokenFenced)
		assert.Equal(t, sent+1, g.lossy.publishes(), "a fenced owner sends nothing more")
		requireFinalizeBlocked(t, g.raw(), "launch-1", 1)
	})

	t.Run("acknowledged release", func(t *testing.T) {
		g := newGuardEnv(t)
		ctx := t.Context()
		op := admit(t, g.c, "launch-1", 1)
		sent := g.lossy.publishes()
		g.lossy.setFault(publishThenReplace(t, endpointBucket, nil))

		_, err := op.Release(ctx)
		requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
		_, err = op.Journal(ctx, journal(readiness.IntentSettle))
		require.ErrorIs(t, err, readiness.ErrTokenReleased)
		assert.Equal(t, sent+1, g.lossy.publishes())
		rec, _ := read(t, g.raw())
		assert.NotContains(t, rec.Tokens, "launch-1", "the release landed")
	})
}

// requireFinalizeBlocked checks the token is still recorded with journal
// entries and keeps rollback from finalizing or reopening.
func requireFinalizeBlocked(t *testing.T, raw *readiness.Control, id string, entries int) {
	t.Helper()
	rec, _ := read(t, raw)
	require.Contains(t, rec.Tokens, id)
	assert.Len(t, rec.Tokens[id].Journal, entries)
	frozenAt := freeze(t, raw, "rb-1")
	_, err := raw.Finalize(t.Context(), "rb-1", frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
	_, err = raw.Reopen(t.Context(), 1, frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
}

func TestGuarded_UnknownJournalFencesTheOperation(t *testing.T) {
	cases := map[string]struct {
		fault   publishFault
		entries int
	}{
		"landed":     {fault: landThenLoseReply, entries: 1},
		"not landed": {fault: loseBeforeLanding, entries: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGuardEnv(t)
			ctx := t.Context()
			op := admit(t, g.c, "launch-1", 1)
			sent := g.lossy.publishes()
			g.lossy.setFault(tc.fault)

			_, err := op.Journal(ctx, journal(readiness.IntentClaim))
			require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
			_, err = op.Journal(ctx, journal(readiness.IntentSettle))
			require.ErrorIs(t, err, readiness.ErrTokenFenced, "the earlier publish may still be in flight")
			_, err = op.Release(ctx)
			require.ErrorIs(t, err, readiness.ErrTokenFenced, "a fenced token is never released by its owner")
			assert.Equal(t, sent+1, g.lossy.publishes())

			requireFinalizeBlocked(t, g.raw(), "launch-1", tc.entries)
			_, err = op.Release(ctx)
			require.ErrorIs(t, err, readiness.ErrTokenFenced, "the fence outlasts a later freeze")
		})
	}
}

func TestGuarded_ConstructorRefusesAMismatchedContext(t *testing.T) {
	ctx := t.Context()
	bind := func() (jetstream.JetStream, *readiness.Bound) {
		js := initEnv(t)
		_, err := newInit(js).Initialize(ctx, firstRollout)
		require.NoError(t, err)
		b, err := newInit(js).Load(ctx)
		require.NoError(t, err)
		return js, b
	}
	jsA, bA := bind()
	jsB, bB := bind()
	otherDomain, err := jetstream.NewWithDomain(jsA.Conn(), "elsewhere")
	require.NoError(t, err)
	seqA, seqB := controlSeq(t, jsA), controlSeq(t, jsB)

	cases := map[string]struct {
		js jetstream.JetStream
		b  *readiness.Bound
	}{
		"writer on the other broker":         {js: jsB, b: bA},
		"wrapped writer on the other broker": {js: &faultJS{JetStream: jsB}, b: bA},
		"streams on the other broker":        {js: jsA, b: bB},
		"streams split across brokers": {js: jsB, b: &readiness.Bound{
			Endpoints: bA.Endpoints, EndpointsKV: bA.EndpointsKV, Control: bB.Control, ControlKV: bB.ControlKV}},
		"same connection, other domain": {js: otherDomain, b: bA},
		"endpoint stream as control": {js: jsA, b: &readiness.Bound{
			Endpoints: bA.Endpoints, EndpointsKV: bA.EndpointsKV, Control: bA.Endpoints, ControlKV: bA.EndpointsKV}},
		"control stream as endpoints": {js: jsA, b: &readiness.Bound{
			Endpoints: bA.Control, EndpointsKV: bA.ControlKV, Control: bA.Control, ControlKV: bA.ControlKV}},
		"no binding": {js: jsA},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := readiness.NewGuarded(tc.js, tc.b, readiness.Rules{})
			require.ErrorIs(t, err, readiness.ErrInvalid)
			assert.Nil(t, c)
		})
	}
	assert.Equal(t, seqA, controlSeq(t, jsA), "nothing is written on either broker")
	assert.Equal(t, seqB, controlSeq(t, jsB))

	c, err := readiness.NewGuarded(&faultJS{JetStream: jsA}, bA, readiness.Rules{})
	require.NoError(t, err, "a wrapper over the binding's own context is accepted")
	_, err = c.Initialize(ctx)
	require.NoError(t, err)
}
