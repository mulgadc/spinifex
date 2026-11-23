package ochre

//test:in-package: builds real catalog bundle records with the package's
// unexported port assignment and key resolution.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var absentKey = readiness.SubjectRevision{}

// claimWrite is what launch-1 presents to claim the tenant key over prior.
func claimWrite(next []byte, prior readiness.SubjectRevision) readiness.EndpointWrite {
	return readiness.EndpointWrite{
		TokenID: "launch-1", Kind: readiness.TokenLaunch, Epoch: 1, Node: testNodeID, Target: drainKey(),
		Nonce: launchOwner.WorkerNonce, Key: drainKey(), Intent: readiness.IntentClaim, Prior: prior, Next: next,
	}
}

// globalUnheld is an observed READY bundle on the global account with no hold.
func globalUnheld(t *testing.T) EndpointRecord {
	t.Helper()
	rec := globalHeld(t)
	rec.HealthHold, rec.Generation = nil, rec.Generation-1
	return rec
}

func holdOf(rec EndpointRecord) EndpointRecord {
	rec.HealthHold = &HealthHold{Token: "sweep-2", Epoch: 1, ReservedAt: rec.LastActive().Add(time.Minute)}
	rec.Generation++
	return rec
}

// holdWrite is what sweep-2, led from another node, presents to hold old.
func holdWrite(old, next []byte) readiness.EndpointWrite {
	return readiness.EndpointWrite{
		TokenID: "sweep-2", Kind: readiness.TokenSweep, Epoch: 1, Node: "node-leader", Target: SweepTarget,
		Key: globalKey(), Intent: readiness.IntentHold, Prior: readiness.SubjectRevision{Seq: 7, Op: readiness.OpPut, Value: old}, Next: next,
	}
}

func TestEndpointWrite_ClaimAllowsABareOwnedRecordOnAnEmptyKey(t *testing.T) {
	claim := encode(t, bareClaimRecord())
	require.NoError(t, CheckEndpointWrite(claimWrite(claim, absentKey)), "a key never written")
	tomb := readiness.SubjectRevision{Seq: 9, Op: readiness.OpDel}
	require.NoError(t, CheckEndpointWrite(claimWrite(claim, tomb)), "a key with a delete marker")
	purged := readiness.SubjectRevision{Seq: 9, Op: readiness.OpPurge}
	require.NoError(t, CheckEndpointWrite(claimWrite(claim, purged)), "a key with a purge marker")

	pinned := bareClaimRecord()
	pinned.Pinned, pinned.OperatorReserved = true, true
	require.NoError(t, CheckEndpointWrite(claimWrite(encode(t, pinned), absentKey)), "a pinned operator reservation")
}

func TestEndpointWrite_ClaimRefusesOtherOperationsAndRecords(t *testing.T) {
	const otherModel = "acme.not-a-catalog-model-v1:0"
	cases := map[string]func(*readiness.EndpointWrite, *EndpointRecord){
		"drain kind":       func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Kind = readiness.TokenDrain },
		"sweep kind":       func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Kind = readiness.TokenSweep },
		"no nonce":         func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Nonce = "" },
		"no node":          func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Node = "" },
		"no epoch":         func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Epoch = 0 },
		"other nonce":      func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Nonce = "nonce-2" },
		"other token":      func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.TokenID = "launch-2" },
		"other epoch":      func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Epoch = 2 },
		"other node":       func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Node = "node-b" },
		"sweep target":     func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Target = SweepTarget },
		"target elsewhere": func(w *readiness.EndpointWrite, _ *EndpointRecord) { w.Target = globalKey() },
		"live prior": func(w *readiness.EndpointWrite, _ *EndpointRecord) {
			w.Prior = readiness.SubjectRevision{Seq: 9, Op: readiness.OpPut, Value: []byte(`{}`)}
		},
		"other account":  func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.AccountID = "000000000002" },
		"no account":     func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.AccountID = "" },
		"global account": func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.AccountID = awsidentifiers.GlobalAccountID },
		"other model":    func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.ModelID = "amazon.titan-embed-text-v2:0" },
		"no owner":       func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.LaunchOwner = OwnerField{} },
		"owner token": func(_ *readiness.EndpointWrite, r *EndpointRecord) {
			r.LaunchOwner = Owned(LaunchOwner{WorkerNonce: "nonce-1", TokenID: "launch-2", Epoch: 1})
		},
		"owner epoch": func(_ *readiness.EndpointWrite, r *EndpointRecord) {
			r.LaunchOwner = Owned(LaunchOwner{WorkerNonce: "nonce-1", TokenID: "launch-1", Epoch: 2})
		},
		"owner nonce": func(_ *readiness.EndpointWrite, r *EndpointRecord) {
			r.LaunchOwner = Owned(LaunchOwner{WorkerNonce: "nonce-2", TokenID: "launch-1", Epoch: 1})
		},
		"record node":     func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.NodeID = "node-b" },
		"READY":           func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.State = StateReady },
		"generation 2":    func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.Generation = 2 },
		"generation 0":    func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.Generation = 0 },
		"no created time": func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.CreatedAt = time.Time{} },
		"instance":        func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.InstanceID = "i-0123456789abcdef0" },
		"address":         func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.PrivateIP = recoveryIP },
		"base URL":        func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.BaseURL = "http://" + recoveryIP + ":8080" },
		"activity":        func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.LastActiveAt = r.CreatedAt },
		"in flight":       func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.InFlight = 1 },
		"successes":       func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.SuccessTotal = 1 },
		"scrape failures": func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.ScrapeFailures = 1 },
		"park reason":     func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.ParkedReason = ParkReadinessTimedOut },
		"park time":       func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.ParkedAt = r.CreatedAt },
		"hold": func(_ *readiness.EndpointWrite, r *EndpointRecord) {
			r.HealthHold = &HealthHold{Token: "launch-1", Epoch: 1, ReservedAt: r.CreatedAt}
		},
		"ready time":        func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.ReadyAt = r.CreatedAt },
		"reserved unpinned": func(_ *readiness.EndpointWrite, r *EndpointRecord) { r.OperatorReserved = true },
		"not a catalog model": func(w *readiness.EndpointWrite, r *EndpointRecord) {
			r.ModelID = otherModel
			w.Key, w.Target = resolveKey(recoveryTenant, otherModel), resolveKey(recoveryTenant, otherModel)
		},
	}
	require.NoError(t, CheckEndpointWrite(claimWrite(encode(t, bareClaimRecord()), absentKey)), "the base claim is allowed")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := bareClaimRecord()
			w := claimWrite(nil, absentKey)
			mutate(&w, &rec)
			w.Next = encode(t, rec)
			require.ErrorIs(t, CheckEndpointWrite(w), errEndpointWrite)
		})
	}
}

func TestEndpointWrite_RefusesAmbiguousOrUnknownWrites(t *testing.T) {
	claim := string(encode(t, bareClaimRecord()))
	cases := map[string]readiness.EndpointWrite{
		"settle intent": func() readiness.EndpointWrite {
			w := claimWrite([]byte(claim), absentKey)
			w.Intent = readiness.IntentSettle
			return w
		}(),
		"no value":        claimWrite(nil, absentKey),
		"trailing data":   claimWrite([]byte(claim+" {}"), absentKey),
		"duplicate key":   claimWrite([]byte(strings.Replace(claim, "{", `{"state":"READY",`, 1)), absentKey),
		"unknown owner":   claimWrite([]byte(strings.Replace(claim, `"worker_nonce"`, `"worker_nonce_v2":"x","worker_nonce"`, 1)), absentKey),
		"held stored bad": holdWrite([]byte(`{"state":"READY"} {}`), encode(t, holdOf(globalUnheld(t)))),
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, CheckEndpointWrite(w), errEndpointWrite)
		})
	}
}

func TestEndpointWrite_HoldAddsOnlyTheSweepsHold(t *testing.T) {
	old := globalUnheld(t)
	require.NoError(t, CheckEndpointWrite(holdWrite(encode(t, old), encode(t, holdOf(old)))))

	idle := old
	idle.InFlight, idle.SuccessTotal, idle.LastActiveAt = 0, 0, time.Time{}
	require.NoError(t, CheckEndpointWrite(holdWrite(encode(t, idle), encode(t, holdOf(idle)))), "an endpoint never observed")

	partial := old
	partial.Members, partial.WeightsVolumeID = nil, ""
	_, err := catalogBundle(partial)
	require.Error(t, err, "the bundle is not complete")
	require.NoError(t, CheckEndpointWrite(holdWrite(encode(t, partial), encode(t, holdOf(partial)))), "a bundle of doubtful identity")
}

func TestEndpointWrite_HoldRefusesOtherScopesAndChanges(t *testing.T) {
	type mutation func(w *readiness.EndpointWrite, old, next *EndpointRecord)
	cases := map[string]mutation{
		"launch kind":   func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Kind = readiness.TokenLaunch },
		"drain kind":    func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Kind = readiness.TokenDrain },
		"no token":      func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.TokenID = "" },
		"no epoch":      func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Epoch = 0 },
		"no node":       func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Node = "" },
		"key as target": func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Target = globalKey() },
		"absent key":    func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Prior = absentKey },
		"delete marker": func(w *readiness.EndpointWrite, _, _ *EndpointRecord) {
			w.Prior = readiness.SubjectRevision{Seq: 9, Op: readiness.OpDel}
		},
		"other key": func(w *readiness.EndpointWrite, _, _ *EndpointRecord) { w.Key = drainKey() },
		"tenant record": func(w *readiness.EndpointWrite, o, n *EndpointRecord) {
			o.AccountID, n.AccountID, w.Key = recoveryTenant, recoveryTenant, drainKey()
		},
		"already held": func(_ *readiness.EndpointWrite, o, _ *EndpointRecord) {
			o.HealthHold = &HealthHold{Token: "sweep-1", Epoch: 1, ReservedAt: o.ReadyAt}
		},
		"held by itself": func(_ *readiness.EndpointWrite, o, n *EndpointRecord) { o.HealthHold = n.HealthHold },
		"STARTING": func(_ *readiness.EndpointWrite, o, n *EndpointRecord) {
			o.State, n.State = StateStarting, StateStarting
		},
		"DRAINING": func(_ *readiness.EndpointWrite, o, n *EndpointRecord) {
			o.State, n.State = StateDraining, StateDraining
		},
		"no hold":            func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.HealthHold = nil },
		"no reservation":     func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.HealthHold.ReservedAt = time.Time{} },
		"other token hold":   func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.HealthHold.Token = "sweep-1" },
		"other epoch hold":   func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.HealthHold.Epoch = 2 },
		"same generation":    func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.Generation-- },
		"skipped generation": func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.Generation++ },
		"state change":       func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.State = StateDraining },
		"observation":        func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.InFlight++ },
		"moved instance":     func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.InstanceID = "i-0fedcba9876543210" },
		"new owner":          func(_ *readiness.EndpointWrite, _, n *EndpointRecord) { n.LaunchOwner = Owned(launchOwner) },
		"park marker": func(_ *readiness.EndpointWrite, _, n *EndpointRecord) {
			n.ParkedReason, n.ParkedAt = ParkReadinessTimedOut, n.ReadyAt
		},
	}
	base := globalUnheld(t)
	require.NoError(t, CheckEndpointWrite(holdWrite(encode(t, base), encode(t, holdOf(base)))), "the base hold is allowed")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			old := globalUnheld(t)
			next := holdOf(old)
			w := holdWrite(nil, nil)
			mutate(&w, &old, &next)
			if w.Prior.Live() {
				w.Prior.Value = encode(t, old)
			}
			w.Next = encode(t, next)
			require.ErrorIs(t, CheckEndpointWrite(w), errEndpointWrite)
		})
	}
}

func TestEndpointWrite_GuardedClaimOverAnAbsentOrDeletedKey(t *testing.T) {
	ctx := t.Context()
	e, op := newLaunchEnv(t)
	claim := encode(t, claimFor(op))
	rev, err := op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, claim, 0)
	require.NoError(t, err)
	entry, err := e.kv.Get(ctx, drainKey())
	require.NoError(t, err)
	assert.Equal(t, rev, entry.Revision())
	assert.Equal(t, string(claim), string(entry.Value()))
	_, err = op.Release(ctx)
	require.NoError(t, err)

	require.NoError(t, e.kv.Delete(ctx, drainKey()))
	history, err := e.kv.History(ctx, drainKey())
	require.NoError(t, err)
	tomb := history[len(history)-1]
	require.Equal(t, jetstream.KeyValueDelete, tomb.Operation())

	next := admitLaunch(t, e.c, "launch-2")
	again := encode(t, claimFor(next))
	_, err = next.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, again, 0)
	require.ErrorIs(t, err, readiness.ErrChanged, "a delete marker has its own revision")
	written, err := next.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, again, tomb.Revision())
	require.NoError(t, err)
	entry, err = e.kv.Get(ctx, drainKey())
	require.NoError(t, err)
	assert.Equal(t, written, entry.Revision())
	assert.Equal(t, string(again), string(entry.Value()))
}

func TestEndpointWrite_GuardedClaimRefusesALiveKey(t *testing.T) {
	ctx := t.Context()
	e, op := newLaunchEnv(t)
	rev, err := e.kv.Put(ctx, drainKey(), encode(t, bareClaimRecord()))
	require.NoError(t, err)
	_, err = op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claimFor(op)), rev)
	require.ErrorIs(t, err, readiness.ErrNotWrite, "another worker's claim stays")
	entry, err := e.kv.Get(ctx, drainKey())
	require.NoError(t, err)
	assert.Equal(t, rev, entry.Revision())
	rec, _, err := e.c.Read(ctx)
	require.NoError(t, err)
	assert.Empty(t, rec.Tokens["launch-1"].Journal, "nothing was journaled")
}

func TestEndpointWrite_GuardedHoldNeedsTheObservedRevision(t *testing.T) {
	ctx := t.Context()
	old := globalUnheld(t)
	e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, old))
	op := e.admit(t, "sweep-2", SweepTarget)

	served := old
	served.LastActiveAt = old.LastActive().Add(time.Minute)
	served.Generation++
	competing, err := e.kv.Update(ctx, globalKey(), encode(t, served), rev)
	require.NoError(t, err)

	_, err = op.WriteEndpoint(ctx, globalKey(), readiness.IntentHold, encode(t, holdOf(old)), rev)
	require.ErrorIs(t, err, readiness.ErrChanged, "the probe saw an older revision")
	got, gotRev := e.stored(t, globalKey())
	assert.Equal(t, competing, gotRev)
	assert.Equal(t, string(encode(t, served)), got)
	rec, _, err := e.c.Read(ctx)
	require.NoError(t, err)
	assert.Empty(t, rec.Tokens["sweep-2"].Journal, "nothing was journaled")

	_, err = op.WriteEndpoint(ctx, globalKey(), readiness.IntentHold, encode(t, holdOf(old)), competing)
	require.ErrorIs(t, err, readiness.ErrNotWrite, "the hold must start from the record now stored")
	held := encode(t, holdOf(served))
	written, err := op.WriteEndpoint(ctx, globalKey(), readiness.IntentHold, held, competing)
	require.NoError(t, err)
	got, gotRev = e.stored(t, globalKey())
	assert.Equal(t, written, gotRev)
	assert.Equal(t, string(held), got)
}

func TestEndpointWrite_GuardedWritesStopAtFreeze(t *testing.T) {
	ctx := t.Context()
	old := globalUnheld(t)
	e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, old))
	sweep := e.admit(t, "sweep-2", SweepTarget)
	launch := admitLaunch(t, e.c, "launch-1")
	_, controlRev, err := e.c.Read(ctx)
	require.NoError(t, err)
	_, err = e.c.Freeze(ctx, "rb-1", controlRev)
	require.NoError(t, err)

	_, err = sweep.WriteEndpoint(ctx, globalKey(), readiness.IntentHold, encode(t, holdOf(old)), rev)
	require.ErrorIs(t, err, readiness.ErrNotAdmitted)
	_, err = launch.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claimFor(launch)), 0)
	require.ErrorIs(t, err, readiness.ErrNotAdmitted)
	_, gotRev := e.stored(t, globalKey())
	assert.Equal(t, rev, gotRev)
	_, err = e.kv.Get(ctx, drainKey())
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

// loseEndpointAck drops the reply of the first allowed endpoint publish,
// after the value lands.
type loseEndpointAck struct {
	jetstream.JetStream
	lost atomic.Bool
}

func (l *loseEndpointAck) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := l.JetStream.PublishMsg(ctx, m, opts...)
	if err == nil && strings.HasPrefix(m.Subject, "$KV."+KVBucketEndpoints+".") && l.lost.CompareAndSwap(false, true) {
		return nil, nats.ErrTimeout
	}
	return ack, err
}

func TestEndpointWrite_UncertainClaimFencesTheOperation(t *testing.T) {
	ctx := t.Context()
	lose := &loseEndpointAck{}
	c, kv, _ := newGuardedControlOver(t, CheckLaunchSettlement, EndpointKey(recoveryTenant, "seed"), []byte(`{"state":"READY"}`),
		func(js jetstream.JetStream) jetstream.JetStream { lose.JetStream = js; return lose })
	op := admitLaunch(t, c, "launch-1")
	claim := claimFor(op)

	_, err := op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claim), 0)
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
	entry, err := kv.Get(ctx, drainKey())
	require.NoError(t, err, "the claim landed")

	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, realizedOf(t, claim)), entry.Revision())
	require.ErrorIs(t, err, readiness.ErrTokenFenced, "the worker cannot go on from an unknown outcome")
	_, err = op.Release(ctx)
	require.ErrorIs(t, err, readiness.ErrTokenFenced)
	rec, _, err := c.Read(ctx)
	require.NoError(t, err)
	assert.Len(t, rec.Tokens["launch-1"].Journal, 1, "the journal keeps the claim for recovery")
}
