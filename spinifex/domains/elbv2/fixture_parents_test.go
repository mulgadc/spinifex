package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fenceHoldWait         = 10 * time.Second
	fenceRouteTableBucket = "spinifex-vpc-route-tables"
)

// fenceHold stops the first matching KV call, before or after it runs, until
// the test frees it.
type fenceHold struct {
	op      string
	after   bool
	match   func(key string) bool
	reached chan struct{}
	release chan struct{}
}

func (h *fenceHold) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(fenceHoldWait):
		t.Fatalf("no %s call reached the hold", h.op)
	}
}

func (h *fenceHold) free() { close(h.release) }

// fenceKV passes every call through. A test can hold a call, run a hook
// before it, or fail a read.
type fenceKV struct {
	jetstream.KeyValue

	mu      sync.Mutex
	holds   []*fenceHold
	before  func(op, key string)
	failGet func(key string) error
}

func (kv *fenceKV) arm(op string, after bool, match func(string) bool) *fenceHold {
	h := &fenceHold{op: op, after: after, match: match, reached: make(chan struct{}), release: make(chan struct{})}
	kv.mu.Lock()
	kv.holds = append(kv.holds, h)
	kv.mu.Unlock()
	return h
}

func (kv *fenceKV) setBefore(hook func(op, key string)) {
	kv.mu.Lock()
	kv.before = hook
	kv.mu.Unlock()
}

func (kv *fenceKV) setFailGet(fail func(key string) error) {
	kv.mu.Lock()
	kv.failGet = fail
	kv.mu.Unlock()
}

func (kv *fenceKV) pass(ctx context.Context, op, key string, after bool) error {
	kv.mu.Lock()
	var h *fenceHold
	for i, c := range kv.holds {
		if c.op == op && c.after == after && c.match(key) {
			h = c
			kv.holds = append(kv.holds[:i], kv.holds[i+1:]...)
			break
		}
	}
	before := kv.before
	failGet := kv.failGet
	kv.mu.Unlock()
	if !after && before != nil {
		before(op, key)
	}
	if op == "Get" && failGet != nil {
		if err := failGet(key); err != nil {
			return err
		}
	}
	if h == nil {
		return nil
	}
	close(h.reached)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (kv *fenceKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if err := kv.pass(ctx, "Get", key, false); err != nil {
		return nil, err
	}
	return kv.KeyValue.Get(ctx, key)
}

func (kv *fenceKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if err := kv.pass(ctx, "Create", key, false); err != nil {
		return 0, err
	}
	return kv.KeyValue.Create(ctx, key, value, opts...)
}

func (kv *fenceKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if err := kv.pass(ctx, "Update", key, false); err != nil {
		return 0, err
	}
	rev, err := kv.KeyValue.Update(ctx, key, value, last)
	if perr := kv.pass(ctx, "Update", key, true); perr != nil {
		return rev, perr
	}
	return rev, err
}

func (kv *fenceKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if err := kv.pass(ctx, "Delete", key, false); err != nil {
		return err
	}
	return kv.KeyValue.Delete(ctx, key, opts...)
}

// fenceCheck holds a dependency check after it answers, once armed, so the
// delete stops between its check and its marked write.
type fenceCheck struct {
	mu    sync.Mutex
	ran   int
	next  *fenceHold
	check ec2vpc.DependencyCheck
}

func (c *fenceCheck) arm() *fenceHold {
	h := &fenceHold{op: "dependency check", reached: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.next = h
	c.mu.Unlock()
	return h
}

func (c *fenceCheck) set(check ec2vpc.DependencyCheck) {
	c.mu.Lock()
	c.check = check
	c.mu.Unlock()
}

func (c *fenceCheck) runs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ran
}

func (c *fenceCheck) call(ctx context.Context, accountID, id string) (bool, error) {
	c.mu.Lock()
	check := c.check
	c.ran++
	c.mu.Unlock()
	blocks, err := check(ctx, accountID, id)
	c.mu.Lock()
	h := c.next
	c.next = nil
	c.mu.Unlock()
	if h != nil {
		close(h.reached)
		select {
		case <-h.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return blocks, err
}

// fenceFixture runs fixture admission and the parent deletes as separate
// services over one JetStream server. The delete side owns the dependency
// checks; admission sees the buckets through its own hooks.
type fenceFixture struct {
	t     *testing.T
	nc    *nats.Conn
	js    jetstream.JetStream
	clock *testClock
	vpcs  *ec2vpc.VPCServiceImpl
	elbKV jetstream.KeyValue

	admit         *fixtureOwnerStore
	admitKV       *fenceKV
	admitVPCKV    *fenceKV
	admitSubnetKV *fenceKV
	del           *fixtureOwnerStore
	delKV         *fenceKV
	vpcCheck      *fenceCheck
	subCheck      *fenceCheck

	vpcID, subnetID string
}

func newFenceFixture(t *testing.T) *fenceFixture {
	t.Helper()
	ctx := t.Context()
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	vpcs, err := ec2vpc.NewVPCServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	store, err := NewStore(ctx, nc)
	require.NoError(t, err)
	vpcKV, err := js.KeyValue(ctx, ec2vpc.KVBucketVPCs)
	require.NoError(t, err)
	subnetKV, err := js.KeyValue(ctx, ec2vpc.KVBucketSubnets)
	require.NoError(t, err)

	f := &fenceFixture{
		t: t, nc: nc, js: js, vpcs: vpcs, elbKV: store.kv,
		clock:         &testClock{t: time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)},
		admitKV:       &fenceKV{KeyValue: store.kv},
		admitVPCKV:    &fenceKV{KeyValue: vpcKV},
		admitSubnetKV: &fenceKV{KeyValue: subnetKV},
		delKV:         &fenceKV{KeyValue: store.kv},
		vpcCheck:      &fenceCheck{},
		subCheck:      &fenceCheck{},
	}
	f.admit = f.owners(f.admitKV, ec2vpc.NewParents(f.admitVPCKV, f.admitSubnetKV))
	f.restartDeleteSide()
	vpcs.SetDependencyCheck("ovn-load-balancer", f.vpcCheck.call)
	vpcs.SetSubnetDependencyCheck("ovn-load-balancer", f.subCheck.call)

	vpcOut, err := vpcs.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.80.0.0/16")}, testAccountID)
	require.NoError(t, err)
	f.vpcID = aws.StringValue(vpcOut.Vpc.VpcId)
	subOut, err := vpcs.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpcOut.Vpc.VpcId, CidrBlock: aws.String("10.80.1.0/24")}, testAccountID)
	require.NoError(t, err)
	f.subnetID = aws.StringValue(subOut.Subnet.SubnetId)
	return f
}

func (f *fenceFixture) owners(kv jetstream.KeyValue, parents *ec2vpc.Parents) *fixtureOwnerStore {
	return &fixtureOwnerStore{kv: kv, parents: parents, now: f.clock.now, claimTTL: lbNameClaimTTL}
}

// restartDeleteSide replaces the delete side's owner store, as a daemon
// restart would, keeping its KV hooks.
func (f *fenceFixture) restartDeleteSide() {
	vpcKV, err := f.js.KeyValue(f.t.Context(), ec2vpc.KVBucketVPCs)
	require.NoError(f.t, err)
	subnetKV, err := f.js.KeyValue(f.t.Context(), ec2vpc.KVBucketSubnets)
	require.NoError(f.t, err)
	f.del = f.owners(f.delKV, ec2vpc.NewParents(vpcKV, subnetKV))
	f.vpcCheck.set(f.del.vpcBlocksDeletion)
	f.subCheck.set(f.del.subnetBlocksDeletion)
}

func (f *fenceFixture) root(id string) *LoadBalancerRecord {
	root := newFixtureRoot(id, "ovn-nlb")
	root.VpcId = f.vpcID
	root.Subnets = []string{f.subnetID}
	return root
}

// begin claims the name for root and returns the pending claim.
func (f *fenceFixture) begin(owners *fixtureOwnerStore, root *LoadBalancerRecord) *fixtureClaim {
	f.t.Helper()
	claim, sealed, err := owners.Begin(f.t.Context(), root)
	require.NoError(f.t, err)
	require.Nil(f.t, sealed)
	return claim
}

func (f *fenceFixture) createAsync(owners *fixtureOwnerStore, claim *fixtureClaim) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := owners.admitAndSeal(f.t.Context(), claim)
		done <- err
	}()
	return done
}

func (f *fenceFixture) result(ch <-chan error) error {
	f.t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(fenceHoldWait):
		f.t.Fatal("call did not return")
		return nil
	}
}

func (f *fenceFixture) deleteVPC() error {
	_, err := f.vpcs.DeleteVpc(f.t.Context(), &ec2.DeleteVpcInput{VpcId: aws.String(f.vpcID)}, testAccountID)
	return err
}

func (f *fenceFixture) deleteSubnet() error {
	return f.deleteSubnetID(f.subnetID)
}

func (f *fenceFixture) deleteSubnetID(id string) error {
	_, err := f.vpcs.DeleteSubnet(f.t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(id)}, testAccountID)
	return err
}

func (f *fenceFixture) addSubnet(cidr string) string {
	f.t.Helper()
	out, err := f.vpcs.CreateSubnet(f.t.Context(), &ec2.CreateSubnetInput{VpcId: aws.String(f.vpcID), CidrBlock: aws.String(cidr)}, testAccountID)
	require.NoError(f.t, err)
	return aws.StringValue(out.Subnet.SubnetId)
}

func fenceAsync(call func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return done
}

func (f *fenceFixture) vpcSlot() string    { return vpcSlotKey(testAccountID, f.vpcID) }
func (f *fenceFixture) subnetSlot() string { return subnetSlotKey(testAccountID, f.subnetID) }

func (f *fenceFixture) isVPC(key string) bool {
	return key == kvstore.AccountKey(testAccountID, f.vpcID)
}

func isSlotKey(key string) bool {
	return strings.HasPrefix(key, keyPrefixOVNVPCSlot) || strings.HasPrefix(key, keyPrefixOVNSubnetSlot)
}

// slotOwner returns the owner named at a slot key, or "" when the slot is absent.
func (f *fenceFixture) slotOwner(key string) string {
	f.t.Helper()
	entry, err := f.elbKV.Get(f.t.Context(), key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ""
	}
	require.NoError(f.t, err)
	var slot fixtureSlot
	require.NoError(f.t, json.Unmarshal(entry.Value(), &slot))
	return slot.OwnerID
}

func (f *fenceFixture) claimPresent(key string) bool {
	f.t.Helper()
	_, err := f.elbKV.Get(f.t.Context(), key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false
	}
	require.NoError(f.t, err)
	return true
}

func (f *fenceFixture) parentEntry(bucket, id string) jetstream.KeyValueEntry {
	f.t.Helper()
	kv, err := f.js.KeyValue(f.t.Context(), bucket)
	require.NoError(f.t, err)
	entry, err := kv.Get(f.t.Context(), kvstore.AccountKey(testAccountID, id))
	require.NoError(f.t, err)
	return entry
}

func (f *fenceFixture) parentMarked(bucket, id string) bool {
	f.t.Helper()
	var rec struct {
		Deleting bool `json:"deleting"`
	}
	require.NoError(f.t, json.Unmarshal(f.parentEntry(bucket, id).Value(), &rec))
	return rec.Deleting
}

func (f *fenceFixture) mainRouteTables() int {
	f.t.Helper()
	kv, err := f.js.KeyValue(f.t.Context(), fenceRouteTableBucket)
	require.NoError(f.t, err)
	all, err := kvstore.Keys(f.t.Context(), kv)
	require.NoError(f.t, err)
	n := 0
	for _, key := range all {
		if key == kvstore.VersionKey {
			continue
		}
		entry, err := kv.Get(f.t.Context(), key)
		require.NoError(f.t, err)
		var rec struct {
			VpcID string `json:"vpc_id"`
		}
		require.NoError(f.t, json.Unmarshal(entry.Value(), &rec))
		if rec.VpcID == f.vpcID {
			n++
		}
	}
	return n
}

// assertVPCKept checks that a refused DeleteVpc left the VPC, its default
// security group and its main route table, with no mark.
func (f *fenceFixture) assertVPCKept() {
	f.t.Helper()
	assert.False(f.t, f.parentMarked(ec2vpc.KVBucketVPCs, f.vpcID), "a refused delete writes no mark")
	sg, err := f.vpcs.FindDefaultSGForVPC(testAccountID, f.vpcID)
	require.NoError(f.t, err)
	assert.NotEmpty(f.t, sg, "the default security group is not cascaded")
	assert.Equal(f.t, 1, f.mainRouteTables(), "the main route table is not reaped")
}

func requireFenceCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	require.Equal(t, code, got, "error: %v", err)
}

// holdBeforeSlots holds admission after it read both parents and before it
// publishes its first slot.
func (f *fenceFixture) holdBeforeSlots(claim *fixtureClaim) (<-chan error, *fenceHold) {
	f.t.Helper()
	slot := f.admitKV.arm("Create", false, isSlotKey)
	admission := f.createAsync(f.admit, claim)
	slot.wait(f.t)
	return admission, slot
}

func TestFixtureFence_Subnet(t *testing.T) {
	t.Parallel()

	t.Run("slots before the delete's check", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		touch := f.admitVPCKV.arm("Update", false, f.isVPC)
		admission := f.createAsync(f.admit, claim)
		touch.wait(t)

		requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
		assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
		touch.free()
		require.NoError(t, f.result(admission))

		f.clock.advance(10 * lbNameClaimTTL)
		requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
		blocked, err := f.del.vpcBlocksDeletion(t.Context(), testAccountID, f.vpcID)
		require.NoError(t, err)
		assert.True(t, blocked, "a sealed owner keeps its VPC dependency past the TTL")
		assert.Equal(t, claim.Owner.OwnerID, f.slotOwner(f.vpcSlot()))
		assert.Equal(t, claim.Owner.OwnerID, f.slotOwner(f.subnetSlot()))
	})

	t.Run("admission touches between the check and the marked write", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		check := f.subCheck.arm()
		deletion := fenceAsync(f.deleteSubnet)
		check.wait(t)
		require.NoError(t, f.result(f.createAsync(f.admit, claim)))

		check.free()
		requireFenceCode(t, f.result(deletion), awserrors.ErrorDependencyViolation)
		assert.Equal(t, 2, f.subCheck.runs(), "the lost mark re-runs the check")
		assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
		got, err := f.admit.GetSealedByArn(t.Context(), claim.Owner.Root.LoadBalancerArn, fixtureRegion, testAccountID)
		require.NoError(t, err)
		require.NotNil(t, got)
	})

	t.Run("marked write before admission's touch", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		admission, slot := f.holdBeforeSlots(claim)
		require.NoError(t, f.deleteSubnet())

		slot.free()
		require.ErrorIs(t, f.result(admission), errFixtureSubnetUnavailable)
		assert.Empty(t, f.slotOwner(f.vpcSlot()), "the refused admission clears its VPC slot")
		assert.Empty(t, f.slotOwner(f.subnetSlot()), "no slot remains for the deleted subnet")
		assert.False(t, f.claimPresent(claim.Key))
		_, err := f.admit.Seal(t.Context(), claim)
		require.ErrorIs(t, err, errFixtureOwnerLost)
	})
}

func TestFixtureFence_Vpc(t *testing.T) {
	t.Parallel()

	t.Run("slots before the delete's check", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		admission, slot := f.holdBeforeSlots(claim)
		require.NoError(t, f.deleteSubnet())
		touched := f.admitVPCKV.arm("Update", true, f.isVPC)
		slot.free()
		touched.wait(t)

		requireFenceCode(t, f.deleteVPC(), awserrors.ErrorDependencyViolation)
		assert.Equal(t, 1, f.vpcCheck.runs(), "DeleteVpc reached the dependency check")
		f.assertVPCKept()
		touched.free()
		require.ErrorIs(t, f.result(admission), errFixtureSubnetUnavailable)
		assert.Empty(t, f.slotOwner(f.vpcSlot()))
		assert.Empty(t, f.slotOwner(f.subnetSlot()))
		_, err := f.admit.Seal(t.Context(), claim)
		require.ErrorIs(t, err, errFixtureOwnerLost)
		require.NoError(t, f.deleteVPC(), "the cleared slot no longer blocks")
	})

	t.Run("admission touches between the check and the marked write", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		admission, slot := f.holdBeforeSlots(claim)
		require.NoError(t, f.deleteSubnet())
		check := f.vpcCheck.arm()
		deletion := fenceAsync(f.deleteVPC)
		check.wait(t)
		touched := f.admitVPCKV.arm("Update", true, f.isVPC)
		slot.free()
		touched.wait(t)

		check.free()
		requireFenceCode(t, f.result(deletion), awserrors.ErrorDependencyViolation)
		assert.Equal(t, 2, f.vpcCheck.runs(), "the touch won the CAS, so the check ran again")
		f.assertVPCKept()
		touched.free()
		require.ErrorIs(t, f.result(admission), errFixtureSubnetUnavailable)
		assert.Empty(t, f.slotOwner(f.vpcSlot()))
		assert.Empty(t, f.slotOwner(f.subnetSlot()))
		_, err := f.admit.Seal(t.Context(), claim)
		require.ErrorIs(t, err, errFixtureOwnerLost)
	})

	t.Run("marked write before admission's touch", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		admission, slot := f.holdBeforeSlots(claim)
		require.NoError(t, f.deleteSubnet())
		// The cascade fails after the mark, so the marked record stays.
		testutil.OverrideVpcdStubResponse(f.nc, "vpc.delete-sg", []byte(`{"success":false,"error":"held"}`))
		require.Error(t, f.deleteVPC())
		require.True(t, f.parentMarked(ec2vpc.KVBucketVPCs, f.vpcID), "the mark won the CAS")

		slot.free()
		require.ErrorIs(t, f.result(admission), errFixtureVPCUnavailable)
		assert.Empty(t, f.slotOwner(f.vpcSlot()))
		assert.Empty(t, f.slotOwner(f.subnetSlot()))
		assert.False(t, f.claimPresent(claim.Key))
	})
}

// A failed dependency point read after the parent touch fails the delete
// closed and leaves the parent record unchanged.
func TestFixtureFence_DependencyReadFailsAfterTouch(t *testing.T) {
	t.Parallel()
	readFail := errors.New("injected read failure")

	for name, failKey := range map[string]func(f *fenceFixture, claim *fixtureClaim) string{
		"slot read":  func(f *fenceFixture, _ *fixtureClaim) string { return f.subnetSlot() },
		"claim read": func(_ *fenceFixture, claim *fixtureClaim) string { return claim.Key },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFenceFixture(t)
			claim := f.begin(f.admit, f.root("aaaa000000000001"))
			require.NoError(t, f.result(f.createAsync(f.admit, claim)))
			before := f.parentEntry(ec2vpc.KVBucketSubnets, f.subnetID)

			key := failKey(f, claim)
			f.delKV.setFailGet(func(k string) error {
				if k == key {
					return readFail
				}
				return nil
			})
			requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorServerInternal)
			after := f.parentEntry(ec2vpc.KVBucketSubnets, f.subnetID)
			assert.Equal(t, before.Revision(), after.Revision(), "the subnet record is not rewritten")
			assert.Equal(t, before.Value(), after.Value())
			assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))

			_, err := f.del.vpcBlocksDeletion(t.Context(), testAccountID, f.vpcID)
			if key == claim.Key {
				require.ErrorIs(t, err, readFail, "the VPC check fails closed too")
			}
		})
	}
}

// After a restart, a delete invalidates an expired pending owner that never
// returned, clears only that owner's slots, and the old creator cannot seal.
func TestFixtureFence_DeleteInvalidatesExpiredPendingOwner(t *testing.T) {
	t.Parallel()
	f := newFenceFixture(t)
	ctx := t.Context()
	claim := f.begin(f.admit, f.root("aaaa000000000001"))
	require.NoError(t, f.admit.admitParents(ctx, claim))

	requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
	assert.True(t, f.claimPresent(claim.Key), "an unexpired pending owner is not invalidated")

	f.clock.advance(lbNameClaimTTL + time.Second)
	f.restartDeleteSide()
	require.NoError(t, f.deleteSubnet())
	assert.False(t, f.claimPresent(claim.Key), "the delete invalidated the expired owner")
	assert.Empty(t, f.slotOwner(f.vpcSlot()), "the owner's VPC slot is cleared with it")
	assert.Empty(t, f.slotOwner(f.subnetSlot()))

	_, err := f.admit.Seal(ctx, claim)
	require.ErrorIs(t, err, errFixtureOwnerLost, "the old creator cannot seal on resume")
	require.NoError(t, f.deleteVPC())
}

// A seal that lands between the delete's claim read and its invalidation
// wins, and the parent dependencies stay.
func TestFixtureFence_ConcurrentSealBlocksInvalidation(t *testing.T) {
	t.Parallel()
	f := newFenceFixture(t)
	ctx := t.Context()
	claim := f.begin(f.admit, f.root("aaaa000000000001"))
	require.NoError(t, f.admit.admitParents(ctx, claim))
	f.clock.advance(lbNameClaimTTL + time.Second)

	sealed := false
	f.delKV.setBefore(func(op, key string) {
		if op != "Delete" || key != claim.Key || sealed {
			return
		}
		sealed = true
		_, err := f.admit.Seal(ctx, claim)
		require.NoError(t, err)
	})
	requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
	require.True(t, sealed, "the delete reached its invalidation")
	assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
	assert.Equal(t, claim.Owner.OwnerID, f.slotOwner(f.vpcSlot()))
	assert.Equal(t, claim.Owner.OwnerID, f.slotOwner(f.subnetSlot()))
	got, err := f.admit.GetSealedByArn(ctx, claim.Owner.Root.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
}

// An expired admission that resumes after a replacement took its name and
// slots cannot seal, and its cleanup leaves the replacement's slots.
func TestFixtureFence_LateWriterKeepsReplacementSlots(t *testing.T) {
	t.Parallel()
	f := newFenceFixture(t)
	ctx := t.Context()
	oldClaim := f.begin(f.admit, f.root("aaaa000000000001"))
	touch := f.admitVPCKV.arm("Update", false, f.isVPC)
	late := f.createAsync(f.admit, oldClaim)
	touch.wait(t)
	assert.Equal(t, oldClaim.Owner.OwnerID, f.slotOwner(f.subnetSlot()))

	f.clock.advance(lbNameClaimTTL + time.Second)
	newClaim := f.begin(f.del, f.root("bbbb000000000002"))
	_, err := f.del.admitAndSeal(ctx, newClaim)
	require.NoError(t, err)
	assert.Equal(t, newClaim.Owner.OwnerID, f.slotOwner(f.vpcSlot()), "the replacement took the stale slot")

	touch.free()
	require.ErrorIs(t, f.result(late), errFixtureOwnerLost)
	assert.Equal(t, newClaim.Owner.OwnerID, f.slotOwner(f.vpcSlot()))
	assert.Equal(t, newClaim.Owner.OwnerID, f.slotOwner(f.subnetSlot()))
	require.NoError(t, f.admit.abandon(ctx, oldClaim))
	assert.Equal(t, newClaim.Owner.OwnerID, f.slotOwner(f.subnetSlot()), "a second late cleanup changes nothing")
	requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
}

// A failed admission that cannot take a live owner's slot refuses and does
// not free that slot.
func TestFixtureFence_FailedAdmissionKeepsLiveSlot(t *testing.T) {
	t.Parallel()
	f := newFenceFixture(t)
	ctx := t.Context()
	first := f.begin(f.admit, f.root("aaaa000000000001"))
	_, err := f.admit.admitAndSeal(ctx, first)
	require.NoError(t, err)

	other := f.root("bbbb000000000002")
	other.Name = "ovn-nlb-other"
	second := f.begin(f.del, other)
	_, err = f.del.admitAndSeal(ctx, second)
	require.ErrorIs(t, err, errFixtureParentBusy)
	assert.Equal(t, first.Owner.OwnerID, f.slotOwner(f.vpcSlot()))
	assert.Equal(t, first.Owner.OwnerID, f.slotOwner(f.subnetSlot()))
	assert.False(t, f.claimPresent(second.Key), "the refused create invalidated its own claim")
}

// The fence, the seal and the cleanup all use the persisted pending snapshot.
// A change to the decoded claim after Begin cannot move the dependency.
func TestFixtureFence_ParentsComeFromPendingSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("seal keeps the saved subnet", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		other := f.addSubnet("10.80.2.0/24")
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		claim.Owner.Root.Subnets[0] = other

		root, err := f.admit.admitAndSeal(t.Context(), claim)
		require.NoError(t, err)
		assert.Equal(t, []string{f.subnetID}, root.Subnets)
		assert.Equal(t, claim.Owner.OwnerID, f.slotOwner(f.subnetSlot()), "the saved subnet holds the dependency")
		assert.Empty(t, f.slotOwner(subnetSlotKey(testAccountID, other)), "the changed subnet is unowned")
		requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
		require.NoError(t, f.deleteSubnetID(other))
	})

	t.Run("cleanup clears the saved subnet", func(t *testing.T) {
		t.Parallel()
		f := newFenceFixture(t)
		other := f.addSubnet("10.80.2.0/24")
		claim := f.begin(f.admit, f.root("aaaa000000000001"))
		require.NoError(t, f.admit.admitParents(t.Context(), claim))
		claim.Owner.Root.Subnets[0] = other
		claim.Owner.OwnerID = "bbbb000000000002"

		require.NoError(t, f.admit.abandon(t.Context(), claim))
		assert.False(t, f.claimPresent(claim.Key))
		assert.Empty(t, f.slotOwner(f.vpcSlot()))
		assert.Empty(t, f.slotOwner(f.subnetSlot()), "the saved subnet's slot is cleared")
		require.NoError(t, f.deleteSubnet())
	})
}

// A slot that is not exactly the owner's scoped VPC and subnet pair is a store
// error. The delete refuses before any invalidation or slot deletion.
// validSlotJSON is the slot admission would write for claim.
func validSlotJSON(f *fenceFixture, claim *fixtureClaim) string {
	return `{"claimKey":"` + claim.Key + `","ownerId":"` + claim.Owner.OwnerID + `","vpcId":"` + f.vpcID + `","subnetId":"` + f.subnetID + `"}`
}

func TestFixtureFence_MalformedSlotFailsClosed(t *testing.T) {
	t.Parallel()

	cases := map[string]func(f *fenceFixture, claim *fixtureClaim) string{
		"extra key list": func(f *fenceFixture, claim *fixtureClaim) string {
			return `{"claimKey":"` + claim.Key + `","ownerId":"` + claim.Owner.OwnerID + `","vpcId":"` + f.vpcID +
				`","subnetId":"` + f.subnetID + `","keys":["` + f.vpcSlot() + `","` + f.subnetSlot() + `","ovnslot.subnet.x.y"]}`
		},
		"pair without the read key": func(f *fenceFixture, claim *fixtureClaim) string {
			return `{"claimKey":"` + claim.Key + `","ownerId":"` + claim.Owner.OwnerID + `","vpcId":"` + f.vpcID + `","subnetId":"subnet-elsewhere"}`
		},
		"claim in another account": func(f *fenceFixture, claim *fixtureClaim) string {
			return `{"claimKey":"` + LBNameKey("ovn-nlb", "210987654321") + `","ownerId":"` + claim.Owner.OwnerID +
				`","vpcId":"` + f.vpcID + `","subnetId":"` + f.subnetID + `"}`
		},
		"pair differs from the claim root": func(f *fenceFixture, claim *fixtureClaim) string {
			return `{"claimKey":"` + claim.Key + `","ownerId":"` + claim.Owner.OwnerID + `","vpcId":"vpc-elsewhere","subnetId":"` + f.subnetID + `"}`
		},
		"valid slot then trailing data": func(f *fenceFixture, claim *fixtureClaim) string {
			return validSlotJSON(f, claim) + `junk`
		},
		"valid slot then a second value": func(f *fenceFixture, claim *fixtureClaim) string {
			return validSlotJSON(f, claim) + "\n" + validSlotJSON(f, claim)
		},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFenceFixture(t)
			ctx := t.Context()
			root := f.root("aaaa000000000001")
			if name == "pair without the read key" {
				// The claim agrees with the slot, so only the read-key check refuses.
				root.Subnets = []string{"subnet-elsewhere"}
			}
			claim := f.begin(f.admit, root)
			f.clock.advance(lbNameClaimTTL + time.Second)
			raw := []byte(value(f, claim))
			rev, err := f.elbKV.Put(ctx, f.subnetSlot(), raw)
			require.NoError(t, err)
			before := f.parentEntry(ec2vpc.KVBucketSubnets, f.subnetID)

			requireFenceCode(t, f.deleteSubnet(), awserrors.ErrorServerInternal)
			after := f.parentEntry(ec2vpc.KVBucketSubnets, f.subnetID)
			assert.Equal(t, before.Revision(), after.Revision(), "the subnet record is not rewritten")
			assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
			slot, err := f.elbKV.Get(ctx, f.subnetSlot())
			require.NoError(t, err)
			assert.Equal(t, rev, slot.Revision(), "the slot is not deleted or rewritten")
			assert.Equal(t, raw, slot.Value())
			assert.True(t, f.claimPresent(claim.Key), "the expired claim is not invalidated")
		})
	}
}

// A slot delete that succeeds on the last allowed attempt completes the
// cleanup; only conflicts are retried.
func TestFixtureFence_ClearSucceedsOnLastAttempt(t *testing.T) {
	t.Parallel()
	f := newFenceFixture(t)
	ctx := t.Context()
	claim := f.begin(f.admit, f.root("aaaa000000000001"))
	require.NoError(t, f.admit.admitParents(ctx, claim))
	entry, err := f.elbKV.Get(ctx, f.subnetSlot())
	require.NoError(t, err)
	value := entry.Value()

	bumps := 0
	f.delKV.setBefore(func(op, key string) {
		if op != "Delete" || key != f.subnetSlot() || bumps == fixtureParentAttempts-1 {
			return
		}
		bumps++
		_, err := f.elbKV.Put(ctx, key, value)
		require.NoError(t, err)
	})
	require.NoError(t, f.del.clearOwnerSlots(ctx, testAccountID, claim.Owner.OwnerID, []string{f.subnetSlot()}))
	assert.Equal(t, fixtureParentAttempts-1, bumps, "every earlier attempt lost its CAS")
	assert.Empty(t, f.slotOwner(f.subnetSlot()))
}
