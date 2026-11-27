package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vipIPAMKV is the IPAM bucket handle of the reservation under test. It counts
// the writes and runs hook around the next one.
type vipIPAMKV struct {
	jetstream.KeyValue

	mu     sync.Mutex
	writes int
	hook   func(write func() (uint64, error)) (uint64, error)
	// read runs once, at the start of the next allocator leader read.
	read func()
}

func (kv *vipIPAMKV) beforeRead() {
	kv.mu.Lock()
	read := kv.read
	kv.read = nil
	kv.mu.Unlock()
	if read != nil {
		read()
	}
}

func (kv *vipIPAMKV) write(write func() (uint64, error)) (uint64, error) {
	kv.mu.Lock()
	kv.writes++
	hook := kv.hook
	kv.hook = nil
	kv.mu.Unlock()
	if hook != nil {
		return hook(write)
	}
	return write()
}

func (kv *vipIPAMKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	return kv.write(func() (uint64, error) { return kv.KeyValue.Create(ctx, key, value, opts...) })
}

func (kv *vipIPAMKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	return kv.write(func() (uint64, error) { return kv.KeyValue.Update(ctx, key, value, last) })
}

func (kv *vipIPAMKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	return kv.write(func() (uint64, error) { return kv.KeyValue.Put(ctx, key, value) })
}

func (kv *vipIPAMKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	_, err := kv.write(func() (uint64, error) { return 0, kv.KeyValue.Delete(ctx, key, opts...) })
	return err
}

func (kv *vipIPAMKV) writeCount() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.writes
}

// vipJS hands the IPAM store the counted handle; leader reads use the real
// client after the handle's read hook.
type vipJS struct {
	jetstream.JetStream
	kv *vipIPAMKV
}

func (j vipJS) Options() jetstream.JetStreamOptions {
	j.kv.beforeRead()
	return j.JetStream.Options()
}

func (j vipJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if bucket == ec2vpc.KVBucketIPAM {
		return j.kv, nil
	}
	return j.JetStream.KeyValue(ctx, bucket)
}

// openVIPs gives the current service a newly opened IPAM over the real bucket,
// as a restarted daemon would open it, and returns its counted handle.
func (h *ovnCreateHarness) openVIPs() *vipIPAMKV {
	h.t.Helper()
	_, err := ec2vpc.NewIPAM(h.t.Context(), h.js)
	require.NoError(h.t, err)
	kv := &vipIPAMKV{KeyValue: h.bucket(ec2vpc.KVBucketIPAM)}
	vips, err := ec2vpc.NewIPAM(h.t.Context(), vipJS{JetStream: h.js, kv: kv})
	require.NoError(h.t, err)
	h.svc.ovn.vips = vips
	return kv
}

// ipamRow is the committed IPAM row of subnetID and its revision.
func (h *ovnCreateHarness) ipamRow(subnetID string) (ec2vpc.IPAMRecord, uint64) {
	h.t.Helper()
	e, err := h.bucket(ec2vpc.KVBucketIPAM).Get(h.t.Context(), subnetID)
	require.NoError(h.t, err)
	var row ec2vpc.IPAMRecord
	require.NoError(h.t, json.Unmarshal(e.Value(), &row))
	return row, e.Revision()
}

type vipResult struct {
	ip  string
	err error
}

// vipPass is the test Reconciler: each elected pass waits for the test to hand
// it the current service, then reserves the address of one named load balancer.
type vipPass struct {
	arn, vpcID string
	service    chan *ELBv2ServiceImpl
	results    chan vipResult
	calls      atomic.Int32
}

var _ reconcile.Reconciler = (*vipPass)(nil)

func newVIPPass(arn, vpcID string) *vipPass {
	return &vipPass{arn: arn, vpcID: vpcID, service: make(chan *ELBv2ServiceImpl), results: make(chan vipResult, 4)}
}

func (p *vipPass) Reconcile(ctx context.Context, intent reconcile.IntentState) error {
	p.calls.Add(1)
	var svc *ELBv2ServiceImpl
	select {
	case svc = <-p.service:
	case <-ctx.Done():
		return ctx.Err()
	}
	if _, ok := intent.VPCs[p.vpcID]; !ok {
		err := errors.New("the pass intent does not hold the fixture VPC")
		p.results <- vipResult{err: err}
		return err
	}
	ip, err := svc.reserveOVNFixtureVIP(ctx, p.arn, testAccountID)
	p.results <- vipResult{ip: ip, err: err}
	return err
}

func (p *vipPass) ReconcileApplyOnly(context.Context, reconcile.IntentState) error { return nil }

func (p *vipPass) ReconcileHostEIPs(context.Context) error { return nil }

// contenderStore reports each lease read with the bytes it saw, so a test
// knows a contender's election ran, and counts its lease writes.
type contenderStore struct {
	kvsync.KVStore
	loads  chan []byte
	writes atomic.Int32
}

func (s *contenderStore) Load(ctx context.Context, key string) ([]byte, uint64, error) {
	v, rev, err := s.KVStore.Load(ctx, key)
	select {
	case s.loads <- v:
	default:
	}
	return v, rev, err
}

func (s *contenderStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	s.writes.Add(1)
	return s.KVStore.CompareAndSwap(ctx, key, value, rev)
}

// drift runs the real drift loop for rec under lease and returns its trigger
// and a stop that waits for the loop to return.
func (h *ovnCreateHarness) drift(rec reconcile.Reconciler, lease *kvsync.Lease) (chan<- struct{}, func()) {
	vpc, _, err := h.svc.ovn.parents.VPC(h.t.Context(), testAccountID, h.vpcID)
	require.NoError(h.t, err)
	require.NotNil(h.t, vpc)
	ctx, cancel := context.WithCancel(h.t.Context())
	trigger := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcile.DriftLoop(ctx, rec, h.nc, lease, vpc.AZ, nil, trigger)
	}()
	stop := sync.OnceFunc(func() { cancel(); <-done })
	h.t.Cleanup(stop)
	return trigger, stop
}

// runPass triggers one pass of p, hands it the current service and returns
// what the reservation answered.
func (h *ovnCreateHarness) runPass(trigger chan<- struct{}, p *vipPass) vipResult {
	h.t.Helper()
	trigger <- struct{}{}
	select {
	case p.service <- h.svc:
	case <-time.After(intentDeadline):
		h.t.Fatal("the triggered pass did not reach the reconciler")
	}
	return h.awaitPass(p)
}

func (h *ovnCreateHarness) awaitPass(p *vipPass) vipResult {
	h.t.Helper()
	select {
	case r := <-p.results:
		return r
	case <-time.After(intentDeadline):
		h.t.Fatal("the pass did not finish its reservation")
		return vipResult{}
	}
}

// requireNoEndpoint asserts a reply still provisions and carries no DNS name
// and no address.
func requireNoEndpoint(t *testing.T, lb *elbv2.LoadBalancer, arn string) {
	t.Helper()
	assert.Equal(t, arn, aws.StringValue(lb.LoadBalancerArn))
	assert.Equal(t, StateProvisioning, aws.StringValue(lb.State.Code))
	assert.Nil(t, lb.DNSName)
	require.NotEmpty(t, lb.AvailabilityZones)
	for _, az := range lb.AvailabilityZones {
		assert.Empty(t, az.LoadBalancerAddresses)
	}
}

func (h *ovnCreateHarness) requireAPIOmitsAddress(arn string) {
	h.t.Helper()
	ctx, cancel := h.bounded()
	defer cancel()
	out, err := h.svc.DescribeLoadBalancers(ctx, byARN(arn), testAccountID)
	require.NoError(h.t, err, "Describe does not wait for the pass")
	require.Len(h.t, out.LoadBalancers, 1)
	requireNoEndpoint(h.t, out.LoadBalancers[0], arn)
	replay, err := h.svc.CreateLoadBalancer(ctx, h.input("ovn-nlb", h.subnetID, h.groups[0]), testAccountID)
	require.NoError(h.t, err, "an equal Create does not wait for the pass")
	require.Len(h.t, replay.LoadBalancers, 1)
	requireNoEndpoint(h.t, replay.LoadBalancers[0], arn)
}

// An elected drift pass reserves the sealed owner's address while the API
// answers, a competing contender does no work, a lost write reply recovers the
// same address after both stores reopen, and listener edits keep it.
func TestOVNFixtureVIP_ElectedPassReservesAndRecoversOneAddress(t *testing.T) {
	h := newOVNCreateHarness(t)
	lb := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(lb.LoadBalancerArn)
	root := h.owner("ovn-nlb").Root
	ipamKV := h.openVIPs()
	elbBefore := h.state(KVBucketELBv2)
	parentsBefore := []map[string]uint64{h.state(ec2vpc.KVBucketVPCs), h.state(ec2vpc.KVBucketSubnets)}

	syncBucket := kvstore.OpenSyncBucket(h.js)
	leaseA := reconcile.NewLeaderLease(syncBucket, reconcile.KeyVPCDReconcile, "vpcd-a")
	contender := &contenderStore{KVStore: syncBucket, loads: make(chan []byte, 1)}
	leaseB := reconcile.NewLeaderLease(contender, reconcile.KeyVPCDReconcile, "vpcd-b")
	passA, passB := newVIPPass(arn, h.vpcID), newVIPPass(arn, h.vpcID)
	triggerA, stopA := h.drift(passA, leaseA)
	triggerB, stopB := h.drift(passB, leaseB)

	writing, held := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	ipamKV.hook = func(write func() (uint64, error)) (uint64, error) {
		close(writing)
		<-held
		if _, err := write(); err != nil {
			return 0, err
		}
		return 0, context.DeadlineExceeded
	}
	triggerA <- struct{}{}
	select {
	case passA.service <- h.svc:
	case <-time.After(intentDeadline):
		t.Fatal("the elected pass did not reach the reconciler")
	}
	select {
	case <-writing:
	case <-time.After(intentDeadline):
		t.Fatal("the reservation did not reach its IPAM write")
	}

	h.requireAPIOmitsAddress(arn)
	triggerB <- struct{}{}
	select {
	case seen := <-contender.loads:
		assert.Contains(t, string(seen), "vpcd-a", "the contender read the elected holder's live lease")
	case <-time.After(intentDeadline):
		t.Fatal("the contender did not run its election")
	}

	release()
	lost := h.awaitPass(passA)
	require.ErrorIs(t, lost.err, context.DeadlineExceeded, "an unknown write result is not success")
	stopA()
	stopB()
	assert.Zero(t, contender.writes.Load(), "the contender never wrote the lease")
	assert.Zero(t, passB.calls.Load(), "the contender lost the lease and did no work")
	row, rev := h.ipamRow(h.subnetID)
	assert.Equal(t, ec2vpc.IPAMRecord{SubnetId: h.subnetID, CidrBlock: "10.81.1.0/24", Allocated: []ec2vpc.IPEntry{
		{IP: "10.81.1.4", Purpose: ec2vpc.PurposeLBVIP, OwnerID: root.LoadBalancerID},
	}}, row, "the write committed one entry from the subnet's own CIDR under the load balancer ID")
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "the owner and both slots are not written")
	assert.Equal(t, parentsBefore, []map[string]uint64{h.state(ec2vpc.KVBucketVPCs), h.state(ec2vpc.KVBucketSubnets)}, "the parents are not touched")

	h.restart()
	ipamKV = h.openVIPs()
	passA = newVIPPass(arn, h.vpcID)
	triggerA, stopA = h.drift(passA, leaseA)
	got := h.runPass(triggerA, passA)
	stopA()
	require.NoError(t, got.err)
	assert.Equal(t, "10.81.1.4", got.ip, "the replay recovers the committed address")
	assert.Zero(t, ipamKV.writeCount(), "recovery writes nothing")
	_, after := h.ipamRow(h.subnetID)
	assert.Equal(t, rev, after)
	h.requireAPIOmitsAddress(arn)

	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	_, err := h.listener(listenerInput(arn, tgArn, 80))
	require.NoError(t, err)
	require.NoError(t, h.register(tgArn, tuple(h.running(1), 0)))
	require.Equal(t, uint64(3), h.owner("ovn-nlb").OwnerRevision)
	passA = newVIPPass(arn, h.vpcID)
	triggerA, _ = h.drift(passA, leaseA)
	got = h.runPass(triggerA, passA)
	require.NoError(t, got.err)
	assert.Equal(t, "10.81.1.4", got.ip, "a listener and target edit keeps the mapping and its address")
	assert.Zero(t, ipamKV.writeCount())
	_, after = h.ipamRow(h.subnetID)
	assert.Equal(t, rev, after)
	h.requireAPIOmitsAddress(arn)
}

// rewrite replaces the JSON object at key in bucket after edit changes it.
func (h *ovnCreateHarness) rewrite(bucket, key string, edit func(map[string]any)) {
	h.t.Helper()
	kv := h.bucket(bucket)
	e, err := kv.Get(h.t.Context(), key)
	require.NoError(h.t, err)
	var v map[string]any
	require.NoError(h.t, json.Unmarshal(e.Value(), &v))
	edit(v)
	raw, err := json.Marshal(v)
	require.NoError(h.t, err)
	_, err = kv.Put(h.t.Context(), key, raw)
	require.NoError(h.t, err)
}

// rewriteRoot edits the stored sealed root of name in place.
func (h *ovnCreateHarness) rewriteRoot(name string, edit func(root map[string]any)) {
	h.rewrite(KVBucketELBv2, LBNameKey(name, testAccountID), func(v map[string]any) { edit(v["root"].(map[string]any)) })
}

// Missing, unsealed, foreign, mismatched or unreadable evidence stops before
// IPAM: no address, no allocator write and no other change.
func TestOVNFixtureVIP_RefusesBeforeAllocating(t *testing.T) {
	injected := errors.New("injected read failure")
	cases := map[string]struct {
		setup func(h *ovnCreateHarness, root *LoadBalancerRecord) (arn, accountID string)
		want  error
	}{
		"no allocator": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.svc.ovn.vips = nil
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"unknown load balancer ID": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			return resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, root.Name, "0123456789abcdef", LoadBalancerTypeNetwork), testAccountID
		}, errOVNVIPRefused},
		"another Region": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			return resourcearn.FormatELBv2LoadBalancer("us-east-1", testAccountID, root.Name, root.LoadBalancerID, LoadBalancerTypeNetwork), testAccountID
		}, errOVNVIPRefused},
		"another account": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			return root.LoadBalancerArn, otherAccountID
		}, errOVNVIPRefused},
		"unsealed owner": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(KVBucketELBv2, LBNameKey(root.Name, testAccountID), func(v map[string]any) { delete(v, "sealed") })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"absent realization": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { delete(r, "realization") })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"another realization": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { r["realization"] = "vm" })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"application type": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { r["type"] = LoadBalancerTypeApplication })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"internet-facing scheme": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { r["scheme"] = SchemeInternetFacing })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"dualstack address type": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { r["ip_address_type"] = "dualstack" })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"absent address type": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewriteRoot(root.Name, func(r map[string]any) { delete(r, "ip_address_type") })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"missing subnet slot": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			require.NoError(h.t, h.bucket(KVBucketELBv2).Delete(h.t.Context(), subnetSlotKey(testAccountID, h.subnetID)))
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"VPC slot of another owner": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(KVBucketELBv2, vpcSlotKey(testAccountID, h.vpcID), func(v map[string]any) { v["ownerId"] = "lb-0123456789abcdef" })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"VPC slot naming another subnet": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(KVBucketELBv2, vpcSlotKey(testAccountID, h.vpcID), func(v map[string]any) { v["subnetId"] = h.subnet2ID })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"VPC being deleted": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(ec2vpc.KVBucketVPCs, kvstore.AccountKey(testAccountID, h.vpcID), func(v map[string]any) { v["deleting"] = true })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"subnet being deleted": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(ec2vpc.KVBucketSubnets, kvstore.AccountKey(testAccountID, h.subnetID), func(v map[string]any) { v["deleting"] = true })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"subnet in another VPC": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.rewrite(ec2vpc.KVBucketSubnets, kvstore.AccountKey(testAccountID, h.subnetID), func(v map[string]any) { v["vpc_id"] = "vpc-0123456789abcdef0" })
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"missing subnet": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			require.NoError(h.t, h.bucket(ec2vpc.KVBucketSubnets).Delete(h.t.Context(), kvstore.AccountKey(testAccountID, h.subnetID)))
			return root.LoadBalancerArn, testAccountID
		}, errOVNVIPRefused},
		"owner read fails": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.elbKV.setFailGetPrefix(KeyPrefixLBName, injected)
			return root.LoadBalancerArn, testAccountID
		}, injected},
		"slot read fails": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.elbKV.setFailGetPrefix(keyPrefixOVNSubnetSlot, injected)
			return root.LoadBalancerArn, testAccountID
		}, injected},
		"subnet read fails": {func(h *ovnCreateHarness, root *LoadBalancerRecord) (string, string) {
			h.subnetKV.setFailGetPrefix(kvstore.AccountKey(testAccountID, h.subnetID), injected)
			return root.LoadBalancerArn, testAccountID
		}, injected},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			h.sealedOVN("ovn-nlb")
			root := h.owner("ovn-nlb").Root
			ipamKV := h.openVIPs()
			arn, accountID := tc.setup(h, root)
			elbBefore := h.state(KVBucketELBv2)
			sideBefore := h.sideEffects()
			parentsBefore := []map[string]uint64{h.state(ec2vpc.KVBucketVPCs), h.state(ec2vpc.KVBucketSubnets)}

			ip, err := h.svc.reserveOVNFixtureVIP(t.Context(), arn, accountID)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, ip)
			assert.Zero(t, ipamKV.writeCount(), "no allocator write")
			assert.Equal(t, sideBefore, h.sideEffects(), "no IPAM row or other side effect")
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2))
			assert.Equal(t, parentsBefore, []map[string]uint64{h.state(ec2vpc.KVBucketVPCs), h.state(ec2vpc.KVBucketSubnets)})
		})
	}
}
