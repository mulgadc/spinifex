package readiness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// testRecord stands in for an endpoint record under testSettles.
type testRecord struct {
	Worker string `json:"worker"`
	Token  string `json:"token"`
	State  string `json:"state"`
	Hold   bool   `json:"hold,omitempty"`
}

var (
	recStarting = []byte(`{"worker":"w1","token":"launch-1","state":"STARTING"}`)
	recReady    = []byte(`{"worker":"w1","token":"launch-1","state":"READY"}`)
)

// testSettles allows only removal or STARTING to READY with the same worker,
// token and hold, as a domain rule would.
func testSettles(_ string, old, next []byte) error {
	var o, n testRecord
	if err := json.Unmarshal(old, &o); err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(next))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return err
	}
	if n.Worker != o.Worker || n.Token != o.Token || n.Hold != o.Hold || o.State != "STARTING" || n.State != "READY" {
		return errors.New("not a finishing transition")
	}
	return nil
}

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
	return newEndpointEnvWith(t, testSettles)
}

func newEndpointEnvWith(t *testing.T, settles readiness.SettleCheck) endpointEnv {
	t.Helper()
	js := initEnv(t)
	_, err := newInit(js).Initialize(t.Context(), firstRollout)
	require.NoError(t, err)
	b, err := newInit(js).Load(t.Context())
	require.NoError(t, err)
	pub := &endpointJS{JetStream: js}
	c, err := readiness.NewGuarded(pub, b, settles)
	require.NoError(t, err)
	activate(t, c)
	op := admit(t, c, "launch-1", 1)
	pub.sent()
	return endpointEnv{js: js, bound: b, pub: pub, c: c, op: op}
}

func (e endpointEnv) raw() *readiness.Control { return readiness.New(e.js, e.bound.ControlKV) }

func (e endpointEnv) journal(t *testing.T, intent readiness.Intent, expected uint64) readiness.Journal {
	t.Helper()
	incarnation, err := e.bound.Anchor.Bound[0].Incarnation()
	require.NoError(t, err)
	return readiness.Journal{Incarnation: incarnation, Key: endpointKey, ExpectedRevision: expected, Intent: intent}
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
	_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("again"), 0)
	require.ErrorIs(t, err, readiness.ErrTokenFenced)
	_, err = e.op.ReadEndpoint(ctx, endpointKey)
	require.ErrorIs(t, err, readiness.ErrTokenFenced, "a fenced owner cannot read to start later work")
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

	rev, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "the journal lands before the one endpoint publish")
	assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: recStarting}, e.stored(t))

	next, err := e.op.SettleEndpoint(ctx, endpointKey, recReady, rev)
	require.NoError(t, err)
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
	got, err = e.op.ReadEndpoint(ctx, endpointKey)
	require.NoError(t, err)
	assert.Equal(t, readiness.SubjectRevision{Seq: next, Op: readiness.OpPut, Value: recReady}, got)

	rec, _ := read(t, e.raw())
	assert.Equal(t, []readiness.Journal{e.journal(t, readiness.IntentClaim, 0), e.journal(t, readiness.IntentSettle, rev)},
		rec.Tokens["launch-1"].Journal)

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
		_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, []byte("new"), 0)
		require.ErrorIs(t, err, readiness.ErrTokenReleased)
		_, err = e.op.ReadEndpoint(ctx, endpointKey)
		require.ErrorIs(t, err, readiness.ErrTokenReleased, "a released owner cannot read to start later work")
		assert.Empty(t, e.pub.sent())
	})
	t.Run("invalid", func(t *testing.T) {
		e := newEndpointEnv(t)
		for _, key := range []string{"", "a b", readiness.MarkerPrefix + "x"} {
			_, err := e.op.WriteEndpoint(ctx, key, readiness.IntentClaim, nil, 0)
			require.ErrorIs(t, err, readiness.ErrInvalid, key)
		}
		for _, intent := range []readiness.Intent{"evict", readiness.IntentSettle} {
			_, err := e.op.WriteEndpoint(ctx, endpointKey, intent, nil, 0)
			require.ErrorIs(t, err, readiness.ErrInvalid, "%s is not a claim or hold", intent)
		}
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
		_, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
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

	_, err = e.op.SettleEndpoint(ctx, endpointKey, recReady, rev.Seq)
	require.NoError(t, err, "admitted work may settle its own write while frozen")
	release(t, e.op)
	_, frozenAt = read(t, e.raw())
	_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
	require.NoError(t, err)
}

// In any phase, a settle may only finish this token's own confirmed write or
// remove a live value; it never creates, resurrects or takes over a record.
func TestEndpoint_SettleNeverStartsNewWork(t *testing.T) {
	ctx := t.Context()
	const foreignKey = "000000000000/foreign"
	// Each case sets up its store, then returns the settle to refuse.
	cases := map[string]func(t *testing.T, e endpointEnv, own uint64) func() error{
		"rewrite a new key at zero": func(t *testing.T, e endpointEnv, _ uint64) func() error {
			return func() error {
				_, err := e.op.SettleEndpoint(ctx, "000000000000/brand-new", []byte("x"), 0)
				return err
			}
		},
		"rewrite another writer's record": func(t *testing.T, e endpointEnv, _ uint64) func() error {
			rev, err := e.bound.EndpointsKV.Put(ctx, foreignKey, []byte("theirs"))
			require.NoError(t, err)
			return func() error {
				_, err := e.op.SettleEndpoint(ctx, foreignKey, []byte("x"), rev)
				return err
			}
		},
		"rewrite own record moved on by another writer": func(t *testing.T, e endpointEnv, _ uint64) func() error {
			rev, err := e.bound.EndpointsKV.Put(ctx, endpointKey, []byte("theirs"))
			require.NoError(t, err)
			return func() error {
				_, err := e.op.SettleEndpoint(ctx, endpointKey, []byte("x"), rev)
				return err
			}
		},
		"delete an absent key": func(t *testing.T, e endpointEnv, _ uint64) func() error {
			return func() error {
				_, err := e.op.DeleteEndpoint(ctx, "000000000000/brand-new", 0)
				return err
			}
		},
		"delete at a stale revision": func(t *testing.T, e endpointEnv, own uint64) func() error {
			return func() error {
				_, err := e.op.DeleteEndpoint(ctx, endpointKey, own-1)
				return err
			}
		},
		"rewrite over its own delete": func(t *testing.T, e endpointEnv, own uint64) func() error {
			tomb, err := e.op.DeleteEndpoint(ctx, endpointKey, own)
			require.NoError(t, err)
			return func() error {
				_, err := e.op.SettleEndpoint(ctx, endpointKey, []byte("x"), tomb)
				return err
			}
		},
		"delete a tombstone": func(t *testing.T, e endpointEnv, own uint64) func() error {
			tomb, err := e.op.DeleteEndpoint(ctx, endpointKey, own)
			require.NoError(t, err)
			return func() error {
				_, err := e.op.DeleteEndpoint(ctx, endpointKey, tomb)
				return err
			}
		},
	}
	for _, frozen := range []bool{false, true} {
		for name, settle := range cases {
			t.Run(fmt.Sprintf("frozen=%t/%s", frozen, name), func(t *testing.T) {
				refuseSettle(t, frozen, settle)
			})
		}
	}
}

// refuseSettle claims endpointKey, optionally freezes, runs the case's setup
// and checks its settle is refused with nothing sent or changed.
func refuseSettle(t *testing.T, frozen bool, settle func(t *testing.T, e endpointEnv, own uint64) func() error) {
	t.Helper()
	ctx := t.Context()
	e := newEndpointEnv(t)
	_, err := e.bound.EndpointsKV.Put(ctx, "000000000000/filler", []byte("x"))
	require.NoError(t, err)
	own, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
	require.NoError(t, err)
	if frozen {
		freeze(t, e.raw(), "rb-1")
	}
	call := settle(t, e, own)
	e.pub.sent()
	before := keyRevisions(t, e)
	rec, _ := read(t, e.raw())
	entries := len(rec.Tokens["launch-1"].Journal)

	require.ErrorIs(t, call(), readiness.ErrNotSettlement)
	assert.Empty(t, e.pub.sent(), "a refused settle sends neither a journal nor an endpoint write")
	assert.Equal(t, before, keyRevisions(t, e), "no record is created or changed")
	requireFinalizeBlocked(t, e.raw(), "launch-1", entries)
}

// keyRevisions reads the test keys' raw revisions with no binding checks.
func keyRevisions(t *testing.T, e endpointEnv) map[string]readiness.SubjectRevision {
	t.Helper()
	got := map[string]readiness.SubjectRevision{}
	for _, key := range []string{endpointKey, "000000000000/brand-new", "000000000000/foreign"} {
		rev, err := readiness.ReadRevision(t.Context(), e.js, endpointBucket, key)
		require.NoError(t, err)
		got[key] = rev
	}
	return got
}

func TestEndpoint_DrainRemovesALiveRecordWhileFrozen(t *testing.T) {
	e := newEndpointEnv(t)
	ctx := t.Context()
	rev, err := e.bound.EndpointsKV.Put(ctx, endpointKey, recStarting)
	require.NoError(t, err)
	release(t, e.op)
	freeze(t, e.raw(), "rb-1")
	drainOp, _, err := e.c.AdmitDrain(ctx, "rb-1", drain("drain-1", 1))
	require.NoError(t, err)
	e.pub.sent()

	_, err = drainOp.WriteEndpoint(ctx, "000000000000/other-model", readiness.IntentHold, []byte("x"), 0)
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "a drain token may not add a hold")
	tomb, err := drainOp.DeleteEndpoint(ctx, endpointKey, rev)
	require.NoError(t, err)
	assert.Equal(t, readiness.SubjectRevision{Seq: tomb, Op: readiness.OpDel}, e.stored(t))
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
	_, err = drainOp.SettleEndpoint(ctx, endpointKey, []byte("x"), tomb)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "a removal is not ownership")

	rec, _ := read(t, e.raw())
	assert.Equal(t, readiness.IntentSettle, rec.Tokens["drain-1"].Journal[0].Intent)
	release(t, drainOp)
	_, frozenAt := read(t, e.raw())
	_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
	require.NoError(t, err)
}

// A settle authorized and journaled while active may publish after a freeze:
// it can only finish the token's own write, and the token blocks finalizing.
func TestEndpoint_SettleJournaledBeforeFreezeMayFinish(t *testing.T) {
	for _, removal := range []bool{false, true} {
		t.Run(fmt.Sprintf("removal=%t", removal), func(t *testing.T) {
			e := newEndpointEnv(t)
			ctx := t.Context()
			own, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
			require.NoError(t, err)
			reached, proceed := make(chan struct{}), make(chan struct{})
			e.pub.setFault(func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
				close(reached)
				<-proceed
				return js.PublishMsg(ctx, m, opts...)
			})
			wrote := make(chan error, 1)
			go func() {
				var err error
				if removal {
					_, err = e.op.DeleteEndpoint(ctx, endpointKey, own)
				} else {
					_, err = e.op.SettleEndpoint(ctx, endpointKey, recReady, own)
				}
				wrote <- err
			}()
			<-reached
			frozenAt := freeze(t, e.raw(), "rb-1")
			_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
			require.ErrorIs(t, err, readiness.ErrTokensOutstanding, "the in-flight settle keeps its token")
			close(proceed)
			require.NoError(t, <-wrote)
			assert.Equal(t, !removal, e.stored(t).Live())

			release(t, e.op)
			_, frozenAt = read(t, e.raw())
			_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
			require.NoError(t, err)
		})
	}
}

// Owning the key and revision is not a settlement: the bytes must pass the
// domain rule, and without one no settle is sent at all.
func TestEndpoint_SameKeySettleNeedsTheDomainRule(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T, settles readiness.SettleCheck) (endpointEnv, uint64) {
		e := newEndpointEnvWith(t, settles)
		own, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
		require.NoError(t, err)
		freeze(t, e.raw(), "rb-1")
		e.pub.sent()
		return e, own
	}
	requireRefused := func(t *testing.T, e endpointEnv, err error) {
		require.ErrorIs(t, err, readiness.ErrNotSettlement)
		assert.Empty(t, e.pub.sent())
		assert.Equal(t, recStarting, e.stored(t).Value, "the owned record is unchanged")
		requireFinalizeBlocked(t, e.raw(), "launch-1", 1)
	}

	t.Run("no rule", func(t *testing.T) {
		e, own := setup(t, nil)
		_, err := e.op.SettleEndpoint(ctx, endpointKey, recReady, own)
		requireRefused(t, e, err)
		_, err = e.op.DeleteEndpoint(ctx, endpointKey, own)
		requireRefused(t, e, err)
	})

	refused := map[string]string{
		"adds a hold":         `{"worker":"w1","token":"launch-1","state":"READY","hold":true}`,
		"strips the worker":   `{"token":"launch-1","state":"READY"}`,
		"strips the token":    `{"worker":"w1","state":"READY"}`,
		"changes the worker":  `{"worker":"w2","token":"launch-1","state":"READY"}`,
		"unknown field":       `{"worker":"w1","token":"launch-1","state":"READY","target":"x"}`,
		"not a record":        `not json`,
		"no state transition": string(recStarting),
	}
	for name, next := range refused {
		t.Run(name, func(t *testing.T) {
			e, own := setup(t, testSettles)
			_, err := e.op.SettleEndpoint(ctx, endpointKey, []byte(next), own)
			requireRefused(t, e, err)
		})
	}

	t.Run("finishing transition", func(t *testing.T) {
		e, own := setup(t, testSettles)
		rev, err := e.op.SettleEndpoint(ctx, endpointKey, recReady, own)
		require.NoError(t, err)
		assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: recReady}, e.stored(t))
		assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
		release(t, e.op)
	})
}
