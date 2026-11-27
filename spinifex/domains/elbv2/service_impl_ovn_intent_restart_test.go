package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// intentDeadline bounds each service call; no call may wait for a consumer.
const intentDeadline = 5 * time.Second

func (h *ovnCreateHarness) bounded() (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.t.Context(), intentDeadline)
}

// recoverOwner is a fresh reader of the restarted fixture: it lists the
// account's owner keys and point-reads the one complete snapshot.
func (h *ovnCreateHarness) recoverOwner(name string) fixtureOwner {
	h.t.Helper()
	ctx := h.t.Context()
	kv := h.bucket(KVBucketELBv2)
	lister, err := kv.ListKeysFiltered(ctx, KeyPrefixLBName+testAccountID+".>")
	require.NoError(h.t, err)
	defer func() { _ = lister.Stop() }()
	var keys []string
	for k := range lister.Keys() {
		keys = append(keys, k)
	}
	nameKey := LBNameKey(name, testAccountID)
	require.Equal(h.t, []string{nameKey}, keys, "one owner key, and no other snapshot")
	e, err := kv.Get(ctx, nameKey)
	require.NoError(h.t, err)
	owner, err := decodeFixtureOwner(e.Value())
	require.NoError(h.t, err)
	return owner
}

// requireHeld asserts the restarted fixture still refuses the explicit deletes
// and still holds its VPC and subnet as dependencies.
func (h *ovnCreateHarness) requireHeld(owner fixtureOwner) {
	h.t.Helper()
	ctx := h.t.Context()
	if owner.Listener != nil {
		_, err := h.svc.DeleteListener(ctx, &elbv2.DeleteListenerInput{ListenerArn: aws.String(owner.Listener.ListenerArn)}, testAccountID)
		requireFenceCode(h.t, err, awserrors.ErrorOperationNotPermitted)
	}
	_, err := h.svc.DeleteLoadBalancer(ctx, &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(owner.Root.LoadBalancerArn)}, testAccountID)
	requireFenceCode(h.t, err, awserrors.ErrorOperationNotPermitted)
	vpc, subnet := h.parentsBlocked()
	assert.True(h.t, vpc, "the VPC stays a dependency")
	assert.True(h.t, subnet, "the subnet stays a dependency")
}

// keySet is the sorted keys of a bucket state, without revisions.
func keySet(state map[string]uint64) []string {
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// liveRecords is the EC2 live instance record space, as the daemon creates it.
func (h *ovnCreateHarness) liveRecords() jetstream.KeyValue {
	kv, err := h.js.CreateKeyValue(h.t.Context(), jetstream.KeyValueConfig{Bucket: reconcile.InstanceRecordBucket, History: 1})
	require.NoError(h.t, err)
	return kv
}

func (h *ovnCreateHarness) putLive(kv jetstream.KeyValue, v *vm.VM) {
	raw, err := json.Marshal(v.Record())
	require.NoError(h.t, err)
	_, err = kv.Put(h.t.Context(), reconcile.InstanceRecordPrefix+v.ID, raw)
	require.NoError(h.t, err)
}

// runOVNFixtureIntentHeldReader checks that the three operations answer within
// their deadlines while a reader of the owner key holds its updates, and that
// the named reader newReader builds admits and refuses by the live record.
func runOVNFixtureIntentHeldReader(t *testing.T, newReader func(jetstream.JetStream) projection.NamedInstanceLookup) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	nameKey := LBNameKey("ovn-nlb", testAccountID)

	records := h.liveRecords()
	a := h.running(1)
	h.putLive(records, h.instance.vms[a])
	h.svc.ovn.instances = newReader(h.js)
	sideBefore := h.sideEffects()
	keysBefore := keySet(h.state(KVBucketELBv2))

	held, err := h.bucket(KVBucketELBv2).Watch(t.Context(), nameKey)
	require.NoError(t, err)
	defer func() { _ = held.Stop() }()
	for _, initial := range []bool{true, false} {
		select {
		case e := <-held.Updates():
			assert.Equal(t, initial, e != nil, "the sealed owner, then the end of the initial values")
		case <-time.After(intentDeadline):
			t.Fatal("the reader did not start")
		}
	}

	ctx, cancel := h.bounded()
	_, err = h.svc.CreateListener(ctx, listenerInput(lbArn, tgArn, 80), testAccountID)
	cancel()
	require.NoError(t, err)
	ctx, cancel = h.bounded()
	_, err = h.svc.RegisterTargets(ctx, &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(a, 0), tuple(a, 9000))}, testAccountID)
	cancel()
	require.NoError(t, err, "the named reader admits a running instance in the VPC")
	ctx, cancel = h.bounded()
	_, err = h.svc.DeregisterTargets(ctx, &elbv2.DeregisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(a, 9000))}, testAccountID)
	cancel()
	require.NoError(t, err)

	h.instance.set(a, func(v *vm.VM) { v.Status = vm.StateStopped })
	h.putLive(records, h.instance.vms[a])
	err = h.register(tgArn, tuple(a, 9001))
	requireFenceCode(t, err, awserrors.ErrorELBv2InvalidTarget)

	want := fixtureOwner{}
	var seen []uint64
	for len(seen) < 3 {
		select {
		case e := <-held.Updates():
			require.NotNil(t, e)
			seen = append(seen, e.Revision())
			want, err = decodeFixtureOwner(e.Value())
			require.NoError(t, err)
		case <-time.After(intentDeadline):
			t.Fatalf("the held reader saw %d of 3 owner writes", len(seen))
		}
	}
	assert.True(t, slices.IsSorted(seen), "the reader observes the writes in order")
	assert.Equal(t, uint64(4), want.OwnerRevision, "one revision per committed edit, none for the refusal")
	assert.Equal(t, []fixtureTarget{{a, 8080}}, want.Listener.Targets)
	assert.Equal(t, keysBefore, keySet(h.state(KVBucketELBv2)), "no orphan key")
	assert.Equal(t, sideBefore, h.sideEffects())
	assert.Zero(t, h.launches())

	h.restart()
	got := h.recoverOwner("ovn-nlb")
	assert.Equal(t, want, got, "the fresh reader recovers the last observed snapshot")
	h.requireHeld(got)
}

// An update refused or interrupted before commit leaves the prior snapshot, and
// a lost acknowledgement after commit is adopted as the one next revision. A
// fresh reader after restart recovers that exact revision and no orphan key.
func TestOVNFixtureIntent_FaultsAroundCommitRecoverOneRevision(t *testing.T) {
	before := map[string]func(update func() (uint64, error)) (uint64, error){
		"refused before commit": func(func() (uint64, error)) (uint64, error) {
			return 0, errors.New("injected refusal")
		},
		"interrupted before commit": func(func() (uint64, error)) (uint64, error) {
			return 0, context.DeadlineExceeded
		},
	}
	lostAck := func(update func() (uint64, error)) (uint64, error) {
		if _, err := update(); err != nil {
			return 0, err
		}
		return 0, context.DeadlineExceeded
	}
	type intentCase struct {
		setup func(h *ovnCreateHarness) (op func(context.Context) error, want func(prior fixtureOwner) *fixtureListener)
	}
	cases := map[string]intentCase{
		"CreateListener": {func(h *ovnCreateHarness) (func(context.Context) error, func(fixtureOwner) *fixtureListener) {
			lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
			tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
			var listenerArn string
			op := func(ctx context.Context) error {
				out, err := h.svc.CreateListener(ctx, listenerInput(lbArn, tgArn, 80), testAccountID)
				if err == nil {
					listenerArn = aws.StringValue(out.Listeners[0].ListenerArn)
				}
				return err
			}
			return op, func(fixtureOwner) *fixtureListener {
				return &fixtureListener{ListenerArn: listenerArn, Port: 80, TargetGroupArn: tgArn, TargetPort: 8080}
			}
		}},
		"RegisterTargets": {func(h *ovnCreateHarness) (func(context.Context) error, func(fixtureOwner) *fixtureListener) {
			tgArn := h.bound()
			a, b := h.running(1), h.running(2)
			require.NoError(h.t, h.register(tgArn, tuple(a, 0)))
			op := func(ctx context.Context) error {
				_, err := h.svc.RegisterTargets(ctx, &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(b, 0))}, testAccountID)
				return err
			}
			return op, func(prior fixtureOwner) *fixtureListener {
				l := *prior.Listener
				l.Targets = []fixtureTarget{{a, 8080}, {b, 8080}}
				return &l
			}
		}},
		"DeregisterTargets": {func(h *ovnCreateHarness) (func(context.Context) error, func(fixtureOwner) *fixtureListener) {
			tgArn := h.bound()
			a, b := h.running(1), h.running(2)
			require.NoError(h.t, h.register(tgArn, tuple(a, 0), tuple(b, 0)))
			op := func(ctx context.Context) error {
				_, err := h.svc.DeregisterTargets(ctx, &elbv2.DeregisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(a, 0))}, testAccountID)
				return err
			}
			return op, func(prior fixtureOwner) *fixtureListener {
				l := *prior.Listener
				l.Targets = []fixtureTarget{{b, 8080}}
				return &l
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			op, want := tc.setup(h)
			nameKey := LBNameKey("ovn-nlb", testAccountID)
			prior := *h.owner("ovn-nlb")
			elbBefore := h.state(KVBucketELBv2)
			sideBefore := h.sideEffects()

			for fault, hook := range before {
				h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
					return hook(update)
				})
				ctx, cancel := h.bounded()
				err := op(ctx)
				cancel()
				requireFenceCode(t, err, awserrors.ErrorServerInternal)
				assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "%s: no write and no orphan key", fault)
				assert.Equal(t, prior, *h.owner("ovn-nlb"), fault)
			}

			h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
				return lostAck(update)
			})
			ctx, cancel := h.bounded()
			require.NoError(t, op(ctx), "the committed snapshot is adopted after a lost acknowledgement")
			cancel()
			after := h.state(KVBucketELBv2)
			assert.Equal(t, keySet(elbBefore), keySet(after), "no orphan key")
			for k, rev := range elbBefore {
				if k != nameKey {
					assert.Equal(t, rev, after[k], "only the owner key changes: %s", k)
				}
			}
			assert.Greater(t, after[nameKey], elbBefore[nameKey])
			assert.Equal(t, sideBefore, h.sideEffects())
			assert.Zero(t, h.launches())

			h.restart()
			got := h.recoverOwner("ovn-nlb")
			assert.Equal(t, prior.OwnerRevision+1, got.OwnerRevision, "exactly one next revision")
			assert.Equal(t, want(prior), got.Listener)
			assert.Equal(t, prior.Root, got.Root)
			assert.Equal(t, &got, h.owner("ovn-nlb"), "the restarted service reads the same snapshot")
			h.requireHeld(got)

			recovered := h.state(KVBucketELBv2)
			ctx, cancel = h.bounded()
			require.NoError(t, op(ctx), "a caller retry after restart is a no-op")
			cancel()
			assert.Equal(t, recovered, h.state(KVBucketELBv2))
		})
	}
}

// A registration that loses its CAS to an interleaved deregistration commits
// on the retry, and the restarted fixture recovers that exact set and holds.
func TestOVNFixtureIntent_RecoversAfterInterleavedLosingWrite(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a, c := h.running(1), h.running(3)
	require.NoError(t, h.register(tgArn, tuple(a, 0)))
	var lost error
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
		require.NoError(t, h.deregister(tgArn, tuple(a, 0)))
		rev, err := update()
		lost = err
		return rev, err
	})

	ctx, cancel := h.bounded()
	_, err := h.svc.RegisterTargets(ctx, &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(c, 0))}, testAccountID)
	cancel()
	require.NoError(t, err)
	require.True(t, definiteKVConflict(lost), "the first update lost its CAS: %v", lost)
	vpc, subnet := h.parentsBlocked()
	assert.True(t, vpc && subnet, "a read after the losing write still sees the dependency")

	h.restart()
	got := h.recoverOwner("ovn-nlb")
	assert.Equal(t, uint64(5), got.OwnerRevision, "listener, register, deregister, register")
	assert.Equal(t, []fixtureTarget{{c, 8080}}, got.Listener.Targets)
	h.requireHeld(got)
}
