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
	Worker   string `json:"worker"`
	Token    string `json:"token"`
	Instance string `json:"instance,omitempty"`
	State    string `json:"state"`
	Hold     bool   `json:"hold,omitempty"`
	Marked   bool   `json:"marked,omitempty"`
}

var (
	recStarting = []byte(`{"worker":"w1","token":"launch-1","state":"STARTING"}`)
	recReady    = []byte(`{"worker":"w1","token":"launch-1","state":"READY"}`)
)

func decodeRecord(data []byte) (testRecord, error) {
	var r testRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(&r)
	return r, err
}

// testSettles stands in for the endpoint domain rule. An owner may remove its
// own record or move it from STARTING to READY. A drain may only clear a READY
// record's hold or move a marked STARTING record to READY, changing nothing else.
func testSettles(s readiness.Settlement) error {
	o, err := decodeRecord(s.Old)
	if err != nil {
		return err
	}
	if s.Next == nil {
		if s.Kind == readiness.TokenDrain || !s.Owned {
			return errors.New("not the owner's removal")
		}
		return nil
	}
	n, err := decodeRecord(s.Next)
	if err != nil {
		return err
	}
	want := o
	switch {
	case s.Kind != readiness.TokenDrain && s.Owned && o.State == "STARTING":
		want.State = "READY"
	case s.Kind == readiness.TokenDrain && o.State == "READY" && o.Hold:
		want.Hold = false
	case s.Kind == readiness.TokenDrain && o.State == "STARTING" && o.Marked:
		want.State = "READY"
	default:
		return errors.New("no settlement applies")
	}
	if n != want {
		return errors.New("not the settling transition")
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

var (
	recHeld        = []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"READY","hold":true}`)
	recCleared     = []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"READY"}`)
	recMarked      = []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"STARTING","marked":true}`)
	recMarkedReady = []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"READY","marked":true}`)
)

// drainEnv stores value at endpointKey as a launch left it, releases the
// launch, freezes for rb-1 and admits drain-1. It records each Settlement the
// rule sees.
func drainEnv(t *testing.T, value []byte) (endpointEnv, *readiness.Operation, uint64, *[]readiness.Settlement) {
	t.Helper()
	ctx := t.Context()
	seen := &[]readiness.Settlement{}
	e := newEndpointEnvWith(t, func(s readiness.Settlement) error {
		*seen = append(*seen, s)
		return testSettles(s)
	})
	rev, err := e.bound.EndpointsKV.Put(ctx, endpointKey, value)
	require.NoError(t, err)
	release(t, e.op)
	freeze(t, e.raw(), "rb-1")
	drainOp, _, err := e.c.AdmitDrain(ctx, "rb-1", drain("drain-1", 1))
	require.NoError(t, err)
	e.pub.sent()
	return e, drainOp, rev, seen
}

func TestEndpoint_DrainRewritesHeldOrMarkedRecords(t *testing.T) {
	cases := map[string]struct{ old, next []byte }{
		"clear a hold":           {old: recHeld, next: recCleared},
		"finish a marked launch": {old: recMarked, next: recMarkedReady},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, drainOp, rev, seen := drainEnv(t, tc.old)
			ctx := t.Context()
			got, err := drainOp.ReadEndpoint(ctx, endpointKey)
			require.NoError(t, err)
			require.Equal(t, rev, got.Seq)

			next, err := drainOp.SettleEndpoint(ctx, endpointKey, tc.next, got.Seq)
			require.NoError(t, err)
			assert.Equal(t, readiness.SubjectRevision{Seq: next, Op: readiness.OpPut, Value: tc.next}, e.stored(t))
			assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent())
			require.Len(t, *seen, 1)
			assert.Equal(t, readiness.Settlement{
				TokenID: "drain-1", Kind: readiness.TokenDrain, Epoch: 1, Rollback: "rb-1", Node: "node-1",
				Key: endpointKey, Revision: rev, Owned: false, Old: tc.old, Next: tc.next,
			}, (*seen)[0], "the rule sees the token, its kind and the observed identity")

			rec, _ := read(t, e.raw())
			assert.Equal(t, readiness.IntentSettle, rec.Tokens["drain-1"].Journal[0].Intent)
			_, frozenAt := read(t, e.raw())
			_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
			require.ErrorIs(t, err, readiness.ErrTokensOutstanding)
			release(t, drainOp)
			_, frozenAt = read(t, e.raw())
			_, err = e.raw().Finalize(ctx, "rb-1", frozenAt)
			require.NoError(t, err)
		})
	}
}

func TestEndpoint_DrainRefusesAnUnsafeOrChangedTransition(t *testing.T) {
	cases := map[string]struct {
		old  []byte
		call func(op *readiness.Operation, rev uint64) error
	}{
		"clear a hold and change the instance": {old: recHeld, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, []byte(`{"worker":"w1","token":"launch-1","instance":"i-2","state":"READY"}`), rev)
			return err
		}},
		"clear a hold and strip the token": {old: recHeld, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, []byte(`{"worker":"w1","instance":"i-1","state":"READY"}`), rev)
			return err
		}},
		"add a hold": {old: recCleared, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, recHeld, rev)
			return err
		}},
		"finish an unmarked launch": {old: recStarting, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, recReady, rev)
			return err
		}},
		"move READY back to STARTING": {old: recMarkedReady, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, recMarked, rev)
			return err
		}},
		"remove a realized record": {old: recHeld, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.DeleteEndpoint(t.Context(), endpointKey, rev)
			return err
		}},
		"rewrite at a revision no longer current": {old: recHeld, call: func(op *readiness.Operation, rev uint64) error {
			_, err := op.SettleEndpoint(t.Context(), endpointKey, recCleared, rev-1)
			return err
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, drainOp, rev, _ := drainEnv(t, tc.old)
			require.ErrorIs(t, tc.call(drainOp, rev), readiness.ErrNotSettlement)
			assert.Empty(t, e.pub.sent(), "a refused settle sends neither a journal nor an endpoint write")
			assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: tc.old}, e.stored(t))
			release(t, drainOp)
		})
	}
}

// A competing write between the drain's check and its publish wins: the
// drain gets a definite conflict, is not fenced and may settle the new value.
func TestEndpoint_DrainLosesACompetingCompareAndSet(t *testing.T) {
	e, drainOp, rev, _ := drainEnv(t, recHeld)
	ctx := t.Context()
	competitor := []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"READY","hold":true,"marked":true}`)
	e.pub.setFault(func(ctx context.Context, js jetstream.JetStream, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		_, err := e.bound.EndpointsKV.Put(ctx, endpointKey, competitor)
		require.NoError(t, err)
		return js.PublishMsg(ctx, m, opts...)
	})

	_, err := drainOp.SettleEndpoint(ctx, endpointKey, recCleared, rev)
	require.ErrorIs(t, err, readiness.ErrChanged)
	require.NotErrorIs(t, err, readiness.ErrOutcomeUnknown)
	assert.Equal(t, []string{"control", "endpoint"}, e.pub.sent(), "one attempt, no retry")
	got := e.stored(t)
	assert.Equal(t, competitor, got.Value, "the competing write survives")

	_, err = drainOp.SettleEndpoint(ctx, endpointKey, recCleared, got.Seq)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the old replacement does not settle the changed record")
	cleared := []byte(`{"worker":"w1","token":"launch-1","instance":"i-1","state":"READY","marked":true}`)
	_, err = drainOp.SettleEndpoint(ctx, endpointKey, cleared, got.Seq)
	require.NoError(t, err, "a conflict does not fence the drain")
	assert.Equal(t, cleared, e.stored(t).Value)
	release(t, drainOp)
}

// The bytes sent are the bytes the rule approved: a caller that changes its
// buffer while the journal is in flight, or a rule that writes into the
// values it was given, cannot change what lands.
func TestEndpoint_OnlyTheValidatedBytesLand(t *testing.T) {
	t.Run("caller changes its buffer after validation", func(t *testing.T) {
		e, drainOp, rev, _ := drainEnv(t, recHeld)
		p := newPause("journal")
		readiness.SetBeforeWriteHook(e.c, p.hook)
		buf := bytes.Clone(recCleared)
		wrote := make(chan error, 1)
		go func() {
			_, err := drainOp.SettleEndpoint(t.Context(), endpointKey, buf, rev)
			wrote <- err
		}()
		<-p.reached
		copy(buf, recHeld)
		close(p.release)
		require.NoError(t, <-wrote)
		assert.Equal(t, recCleared, e.stored(t).Value)
	})
	t.Run("rule writes into its values", func(t *testing.T) {
		e, drainOp, rev, _ := drainEnv(t, recHeld)
		readiness.SetSettleCheck(e.c, func(s readiness.Settlement) error {
			err := testSettles(s)
			copy(s.Next, recHeld)
			copy(s.Old, "XXXX")
			return err
		})
		_, err := drainOp.SettleEndpoint(t.Context(), endpointKey, bytes.Clone(recCleared), rev)
		require.NoError(t, err)
		assert.Equal(t, recCleared, e.stored(t).Value)
	})
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

// The rule sees the admitted token's own node and target, so it can check the
// key and worker against what the token was admitted for.
func TestEndpoint_SettleCheckSeesTheAdmittedTarget(t *testing.T) {
	ctx := t.Context()
	var seen []readiness.Settlement
	e := newEndpointEnvWith(t, func(s readiness.Settlement) error {
		seen = append(seen, s)
		if s.Target != "acct/model" || s.Node != "node-1" {
			return errors.New("token not admitted for this endpoint")
		}
		return testSettles(s)
	})
	own, err := e.op.WriteEndpoint(ctx, endpointKey, readiness.IntentClaim, recStarting, 0)
	require.NoError(t, err)
	_, err = e.op.SettleEndpoint(ctx, endpointKey, recReady, own)
	require.NoError(t, err)
	require.Len(t, seen, 1)
	assert.Equal(t, "launch-1", seen[0].TokenID)
	assert.Equal(t, readiness.TokenLaunch, seen[0].Kind)
	assert.Equal(t, "node-1", seen[0].Node)
	assert.Equal(t, "acct/model", seen[0].Target)
	assert.True(t, seen[0].Owned)
	release(t, e.op)

	rev, err := e.bound.EndpointsKV.Put(ctx, endpointKey, recHeld)
	require.NoError(t, err)
	freeze(t, e.raw(), "rb-1")
	for _, tok := range []readiness.Token{
		{ID: "drain-other", Kind: readiness.TokenDrain, Epoch: 1, Node: "node-1", Target: "acct/other"},
		{ID: "drain-node", Kind: readiness.TokenDrain, Epoch: 1, Node: "node-2", Target: "acct/model"},
	} {
		op, _, err := e.c.AdmitDrain(ctx, "rb-1", tok)
		require.NoError(t, err)
		e.pub.sent()
		_, err = op.SettleEndpoint(ctx, endpointKey, recCleared, rev)
		require.ErrorIs(t, err, readiness.ErrNotSettlement, tok.ID)
		assert.Empty(t, e.pub.sent())
		got := seen[len(seen)-1]
		assert.Equal(t, []string{tok.Node, tok.Target}, []string{got.Node, got.Target})
		release(t, op)
	}
	op, _, err := e.c.AdmitDrain(ctx, "rb-1", readiness.Token{ID: "drain-1", Kind: readiness.TokenDrain, Epoch: 1, Node: "node-1", Target: "acct/model"})
	require.NoError(t, err)
	_, err = op.SettleEndpoint(ctx, endpointKey, recCleared, rev)
	require.NoError(t, err)
	assert.Equal(t, recCleared, e.stored(t).Value)
	release(t, op)
}

func TestEndpoint_SweepReachesTheRuleForAnUnownedRevision(t *testing.T) {
	ctx := t.Context()
	var seen []readiness.Settlement
	e := newEndpointEnvWith(t, func(s readiness.Settlement) error {
		seen = append(seen, s)
		return nil
	})
	keys := []string{"000000000000/held-a", "000000000001/parked-b"}
	revs := map[string]uint64{}
	for _, k := range keys {
		rev, err := e.bound.EndpointsKV.Put(ctx, k, []byte("written by an earlier sweep"))
		require.NoError(t, err)
		revs[k] = rev
	}

	_, err := e.op.SettleEndpoint(ctx, keys[0], []byte("x"), revs[keys[0]])
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "a launch token still needs its own revision")
	require.Empty(t, seen, "the launch token never reaches the rule")

	sweep, _, err := e.c.Admit(ctx, readiness.Token{ID: "sweep-2", Kind: readiness.TokenSweep, Epoch: 1, Node: "node-2", Target: "sweep"})
	require.NoError(t, err)
	e.pub.sent()
	for _, k := range keys {
		_, err := sweep.SettleEndpoint(ctx, k, []byte("settled"), revs[k])
		require.NoError(t, err)
	}
	require.Len(t, seen, 2)
	for i, s := range seen {
		assert.Equal(t, readiness.TokenSweep, s.Kind)
		assert.False(t, s.Owned, "the rule is told the sweep did not write this revision")
		assert.Equal(t, keys[i], s.Key)
		assert.Equal(t, "node-2", s.Node)
	}
	rec, _ := read(t, e.raw())
	assert.Equal(t, []readiness.Journal{
		{Incarnation: e.journal(t, readiness.IntentSettle, 0).Incarnation, Key: keys[0], ExpectedRevision: revs[keys[0]], Intent: readiness.IntentSettle},
		{Incarnation: e.journal(t, readiness.IntentSettle, 0).Incarnation, Key: keys[1], ExpectedRevision: revs[keys[1]], Intent: readiness.IntentSettle},
	}, rec.Tokens["sweep-2"].Journal, "one token covers the sweep and journals each key")
}
