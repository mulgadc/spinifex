package readiness_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const endpointKey = "000000000000/new-model"

// endpointJS faults the next endpoint publish once and records, in call
// order, which bucket each publish was sent to.
type endpointJS struct {
	jetstream.JetStream
	mu    sync.Mutex
	fault publishFault
	order []string
}

func (e *endpointJS) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	e.mu.Lock()
	var fault publishFault
	if strings.HasPrefix(m.Subject, "$KV."+endpointBucket+".") {
		e.order = append(e.order, "endpoint")
		fault, e.fault = e.fault, nil
	} else {
		e.order = append(e.order, "control")
	}
	e.mu.Unlock()
	if fault != nil {
		return fault(ctx, e.JetStream, m, opts...)
	}
	return e.JetStream.PublishMsg(ctx, m, opts...)
}

func (e *endpointJS) setFault(fault publishFault) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fault = fault
}

// sent returns the publishes since the last call and starts a new count.
func (e *endpointJS) sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	got := e.order
	e.order = nil
	return got
}

type endpointEnv struct {
	js    jetstream.JetStream
	bound *readiness.Bound
	pub   *endpointJS
	c     *readiness.Control
	op    *readiness.Operation
}

// newEndpointEnv binds a deployment, activates it and admits launch-1 through a
// guarded Control whose endpoint publishes can be faulted.
func newEndpointEnv(t *testing.T) endpointEnv {
	t.Helper()
	js := initEnv(t)
	_, err := newInit(js).Initialize(t.Context(), firstRollout)
	require.NoError(t, err)
	b, err := newInit(js).Load(t.Context())
	require.NoError(t, err)
	pub := &endpointJS{JetStream: js}
	c, err := readiness.NewGuarded(pub, b)
	require.NoError(t, err)
	activate(t, c)
	op := admit(t, c, "launch-1", 1)
	pub.sent()
	return endpointEnv{js: js, bound: b, pub: pub, c: c, op: op}
}

func (e endpointEnv) raw() *readiness.Control { return readiness.New(e.js, e.bound.ControlKV) }

func (e endpointEnv) journal(t *testing.T, intent readiness.Intent, expected uint64) readiness.Journal {
	t.Helper()
	return readiness.Journal{Incarnation: e.bound.Anchor.Bound[0].Incarnation(), Key: endpointKey, ExpectedRevision: expected, Intent: intent}
}

// stored reads the endpoint key with no binding checks.
func (e endpointEnv) stored(t *testing.T) readiness.SubjectRevision {
	t.Helper()
	rev, err := readiness.ReadRevision(t.Context(), e.js, endpointBucket, endpointKey)
	require.NoError(t, err)
	return rev
}

// requireFenced checks that the Operation sends nothing more and that its
// token, with its journal, keeps rollback from finalizing.
func (e endpointEnv) requireFenced(t *testing.T, entries int) {
	t.Helper()
	ctx := t.Context()
	_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentSettle, []byte("again"), 0)
	require.ErrorIs(t, err, readiness.ErrTokenFenced)
	_, err = e.op.Journal(ctx, journal(readiness.IntentSettle))
	require.ErrorIs(t, err, readiness.ErrTokenFenced)
	_, err = e.op.Release(ctx)
	require.ErrorIs(t, err, readiness.ErrTokenFenced)
	assert.Empty(t, e.pub.sent(), "a fenced owner sends nothing more")
	requireFinalizeBlocked(t, e.raw(), "launch-1", entries)
}

func TestEndpoint_WriteJournalsBeforeOneFixedRevisionPublish(t *testing.T) {
	e := newEndpointEnv(t)
	ctx := t.Context()

	got, err := e.op.ReadEndpoint(ctx, endpointKey)
	require.NoError(t, err)
	require.Equal(t, readiness.SubjectRevision{}, got, "an absent key is revision zero")

	rev, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("v1"), 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "the journal lands before the one endpoint publish")
	assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: []byte("v1")}, e.stored(t))

	next, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentSettle, []byte("v2"), rev)
	require.NoError(t, err)
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
	got, err = e.op.ReadEndpoint(ctx, endpointKey)
	require.NoError(t, err)
	assert.Equal(t, readiness.SubjectRevision{Seq: next, Op: readiness.OpPut, Value: []byte("v2")}, got)

	rec, _ := read(t, e.raw())
	assert.Equal(t, []readiness.Journal{e.journal(t, readiness.IntentClaim, 0), e.journal(t, readiness.IntentSettle, rev)},
		rec.Tokens["launch-1"].Journal)
	assert.Contains(t, rec.Tokens["launch-1"].Journal[0].Incarnation, endpointBucket+"@")

	release(t, e.op)
	frozenAt := freeze(t, e.raw(), "rb-1")
	_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
	require.NoError(t, err)
}

func TestEndpoint_TombstoneRevisionMustBeNamed(t *testing.T) {
	for _, op := range []string{readiness.OpDel, readiness.OpPurge} {
		t.Run(op, func(t *testing.T) {
			e := newEndpointEnv(t)
			ctx := t.Context()
			_, err := e.bound.EndpointsKV.Put(ctx, endpointKey, []byte("old"))
			require.NoError(t, err)
			if op == readiness.OpDel {
				require.NoError(t, e.bound.EndpointsKV.Delete(ctx, endpointKey))
			} else {
				require.NoError(t, e.bound.EndpointsKV.Purge(ctx, endpointKey))
			}
			tomb, err := e.op.ReadEndpoint(ctx, endpointKey)
			require.NoError(t, err)
			require.NotZero(t, tomb.Seq)
			require.Equal(t, op, tomb.Op)

			_, err = e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), 0)
			require.ErrorIs(t, err, readiness.ErrChanged, "zero is not a create over a tombstone")
			require.NotErrorIs(t, err, readiness.ErrOutcomeUnknown)
			assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "one attempt, no hidden retry")
			assert.Equal(t, tomb, e.stored(t))

			rev, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), tomb.Seq)
			require.NoError(t, err, "a definite conflict does not fence the owner")
			assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: []byte("new")}, e.stored(t))
			rec, _ := read(t, e.raw())
			assert.Equal(t, []readiness.Journal{e.journal(t, readiness.IntentClaim, 0), e.journal(t, readiness.IntentClaim, tomb.Seq)},
				rec.Tokens["launch-1"].Journal)
			release(t, e.op)
		})
	}
}

func TestEndpoint_BindingLostDuringReadIsNeverInterpreted(t *testing.T) {
	for _, bucket := range []string{endpointBucket, readiness.KVBucket} {
		t.Run(bucket, func(t *testing.T) {
			e := newEndpointEnv(t)
			ctx := t.Context()
			_, err := e.bound.EndpointsKV.Put(ctx, endpointKey, []byte("old"))
			require.NoError(t, err)
			var once sync.Once
			readiness.SetAfterReadHook(e.c, func() { once.Do(func() { recreate(t, e.js, bucket) }) })

			got, err := e.op.ReadEndpoint(ctx, endpointKey)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced)
			assert.Equal(t, readiness.SubjectRevision{}, got, "no revision is returned")

			_, err = e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), 0)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced)
			_, err = e.op.Release(ctx)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced, "a lost binding is not proof of release")
			assert.Empty(t, e.pub.sent(), "nothing is written under a lost binding")
			if bucket == endpointBucket {
				requireFinalizeBlocked(t, e.raw(), "launch-1", 0)
			}
		})
	}
}

func TestEndpoint_ReplacementAfterAnAnswerFencesTheOwner(t *testing.T) {
	cases := map[string]bool{"acknowledged": false, "reported conflict": true}
	for name, conflict := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEndpointEnv(t)
			var competing func()
			if conflict {
				competing = func() {
					_, err := e.bound.EndpointsKV.Put(t.Context(), endpointKey, []byte("competitor"))
					require.NoError(t, err)
				}
			}
			e.pub.setFault(publishThenReplace(t, endpointBucket, competing))

			_, err := e.op.WriteEndpoint(t.Context(), endpointKey, readiness.IntentClaim, []byte("new"), 0)
			requireUnconfirmed(t, err, readiness.ErrStreamReplaced)
			assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "the write is not retried")
			e.requireFenced(t, 1)
		})
	}
}

func TestEndpoint_LostAcknowledgementFencesTheOwner(t *testing.T) {
	cases := map[string]struct {
		fault  publishFault
		landed bool
	}{
		"landed":     {fault: landThenLoseReply, landed: true},
		"not landed": {fault: loseBeforeLanding},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEndpointEnv(t)
			e.pub.setFault(tc.fault)

			_, err := e.op.WriteEndpoint(t.Context(), endpointKey, readiness.IntentClaim, []byte("new"), 0)
			require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
			assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
			assert.Equal(t, tc.landed, e.stored(t).Live())
			e.requireFenced(t, 1)
		})
	}
}

func TestEndpoint_ReleaseWaitsForAnInFlightWrite(t *testing.T) {
	cases := map[string]struct {
		land       publishFault
		releasedOK bool
	}{
		"write confirmed": {land: func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
			return js.PublishMsg(ctx, m, opts...)
		}, releasedOK: true},
		"write unconfirmed": {land: landThenLoseReply},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEndpointEnv(t)
			ctx := t.Context()
			reached, proceed := make(chan struct{}), make(chan struct{})
			e.pub.setFault(func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
				close(reached)
				<-proceed
				return tc.land(ctx, js, m, opts...)
			})

			wrote := make(chan error, 1)
			go func() {
				_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), 0)
				wrote <- err
			}()
			<-reached
			released := make(chan error, 1)
			go func() {
				_, err := e.op.Release(ctx)
				released <- err
			}()
			assert.Never(t, func() bool { return len(released) > 0 }, 100*time.Millisecond, 5*time.Millisecond,
				"release must wait for the in-flight endpoint write")
			close(proceed)

			writeErr, releaseErr := <-wrote, <-released
			rec, _ := read(t, e.raw())
			if tc.releasedOK {
				require.NoError(t, writeErr)
				require.NoError(t, releaseErr)
				assert.Equal(t, []string{"control", "endpoint", "control"}, e.pub.sent(), "release is sent after the write returns")
				assert.NotContains(t, rec.Tokens, "launch-1")
			} else {
				require.ErrorIs(t, writeErr, readiness.ErrOutcomeUnknown)
				require.ErrorIs(t, releaseErr, readiness.ErrTokenFenced)
				assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "an unconfirmed write is never released")
				assert.Contains(t, rec.Tokens, "launch-1")
			}
			assert.True(t, e.stored(t).Live())
		})
	}
}

func TestEndpoint_RefusedBeforeAnyEndpointPublish(t *testing.T) {
	ctx := t.Context()
	t.Run("frozen claim", func(t *testing.T) {
		e := newEndpointEnv(t)
		freeze(t, e.raw(), "rb-1")
		_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), 0)
		require.ErrorIs(t, err, readiness.ErrNotAdmitted)
		assert.Empty(t, e.pub.sent())
	})
	t.Run("released", func(t *testing.T) {
		e := newEndpointEnv(t)
		release(t, e.op)
		e.pub.sent()
		_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentSettle, []byte("new"), 0)
		require.ErrorIs(t, err, readiness.ErrTokenReleased)
		assert.Empty(t, e.pub.sent())
	})
	t.Run("invalid", func(t *testing.T) {
		e := newEndpointEnv(t)
		for _, key := range []string{"", "a b", readiness.MarkerPrefix + "x"} {
			_, err := e.op.WriteEndpoint(ctx, key, readiness.IntentClaim, nil, 0)
			require.ErrorIs(t, err, readiness.ErrInvalid, key)
		}
		_, err := e.op.WriteEndpoint(ctx, endpointKey, "evict", nil, 0)
		require.ErrorIs(t, err, readiness.ErrInvalid)
		assert.Empty(t, e.pub.sent())
	})
	t.Run("unguarded control", func(t *testing.T) {
		e := newEndpointEnv(t)
		op := admit(t, e.raw(), "launch-2", 1)
		_, err := op.ReadEndpoint(ctx, endpointKey)
		require.ErrorIs(t, err, readiness.ErrInvalid)
		_, err = op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, nil, 0)
		require.ErrorIs(t, err, readiness.ErrInvalid)
		assert.False(t, e.stored(t).Live())
	})
}

// A claim journaled before a freeze is admitted work: its publish still lands
// under the outstanding token, which keeps rollback from finalizing.
func TestEndpoint_ClaimJournaledBeforeFreezeMayFinish(t *testing.T) {
	e := newEndpointEnv(t)
	ctx := t.Context()
	reached, proceed := make(chan struct{}), make(chan struct{})
	e.pub.setFault(func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		close(reached)
		<-proceed
		return js.PublishMsg(ctx, m, opts...)
	})
	wrote := make(chan error, 1)
	go func() {
		_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("v1"), 0)
		wrote <- err
	}()
	<-reached
	frozenAt := freeze(t, e.raw(), "rb-1")
	close(proceed)
	require.NoError(t, <-wrote, "the claim journaled before the freeze finishes")
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
	rev := e.stored(t)
	require.True(t, rev.Live())

	_, err := e.raw().Finalize(ctx, "rb-1", frozenAt)
	require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "the admitted claim has not settled")
	for _, intent := range []readiness.Intent{readiness.IntentClaim, readiness.IntentHold} {
		_, err = e.op.WriteEndpoint(ctx, "000000000000/other-model", intent, []byte("x"), 0)
		require.ErrorIs(t, err, readiness.ErrNotAdmitted, "no new %s after the freeze", intent)
	}
	assert.Empty(t, e.pub.sent(), "a refused journal sends no endpoint write")

	_, err = e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentSettle, []byte("v2"), rev.Seq)
	require.NoError(t, err, "admitted work may settle while frozen")
	release(t, e.op)
	_, frozenAt = read(t, e.raw())
	_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
	require.NoError(t, err)
}
