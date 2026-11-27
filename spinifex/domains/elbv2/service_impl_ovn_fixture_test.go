package elbv2

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	otherAccountID    = "210987654321"
	ovnDependencyKind = "ovn-load-balancer"
)

var (
	opKVWrites = []string{"Put", "PutString", "Create", "Update", "Delete", "Purge"}
	opKVLists  = []string{"Watch", "WatchAll", "WatchFiltered", "Keys", "ListKeys", "ListKeysFiltered", "History"}
)

// opKV records every call the code under test makes on a bucket, as "Op key",
// and can fail Gets.
type opKV struct {
	jetstream.KeyValue

	mu         sync.Mutex
	ops        []string
	failGet    error
	failPrefix string
	onUpdate   func(key string, value []byte, update func() (uint64, error)) (uint64, error)
	lagging    map[string]jetstream.KeyValueEntry
}

// lag makes ordinary Gets of key answer as a lagging replica would: with
// stale, or as absent when stale is nil. The stream keeps the committed value.
func (kv *opKV) lag(key string, stale jetstream.KeyValueEntry) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.lagging == nil {
		kv.lagging = map[string]jetstream.KeyValueEntry{}
	}
	kv.lagging[key] = stale
}

func (kv *opKV) lagged(key string) (jetstream.KeyValueEntry, bool) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	stale, ok := kv.lagging[key]
	return stale, ok
}

func (kv *opKV) record(op, key string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.ops = append(kv.ops, op+" "+key)
	if (op == "Get" || op == "LeaderGet") && strings.HasPrefix(key, kv.failPrefix) {
		return kv.failGet
	}
	return nil
}

func (kv *opKV) setFailGet(err error) { kv.setFailGetPrefix("", err) }

// setFailGetPrefix fails only the Gets of keys with prefix.
func (kv *opKV) setFailGetPrefix(prefix string, err error) {
	kv.mu.Lock()
	kv.failGet, kv.failPrefix = err, prefix
	kv.mu.Unlock()
}

// take returns the calls since the last take.
func (kv *opKV) take() []string {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	ops := kv.ops
	kv.ops = nil
	return ops
}

func opsOf(ops []string, names []string) []string {
	var out []string
	for _, op := range ops {
		if slices.Contains(names, strings.SplitN(op, " ", 2)[0]) {
			out = append(out, op)
		}
	}
	return out
}

func (kv *opKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if err := kv.record("Get", key); err != nil {
		return nil, err
	}
	if stale, ok := kv.lagged(key); ok {
		if stale == nil {
			return nil, jetstream.ErrKeyNotFound
		}
		return stale, nil
	}
	return kv.KeyValue.Get(ctx, key)
}

// leaderGet records a stream leader point read as "LeaderGet key" and fails it
// as a Get. The lag shim does not apply: the leader holds the committed value.
func (kv *opKV) leaderGet(js jetstream.JetStream) fixtureLeaderGet {
	get := leaderGetOf(js, kv.Bucket())
	return func(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
		if err := kv.record("LeaderGet", key); err != nil {
			return nil, err
		}
		return get(ctx, key)
	}
}

func (kv *opKV) GetRevision(ctx context.Context, key string, revision uint64) (jetstream.KeyValueEntry, error) {
	_ = kv.record("GetRevision", key)
	return kv.KeyValue.GetRevision(ctx, key, revision)
}

func (kv *opKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	_ = kv.record("Put", key)
	return kv.KeyValue.Put(ctx, key, value)
}

func (kv *opKV) PutString(ctx context.Context, key string, value string) (uint64, error) {
	_ = kv.record("PutString", key)
	return kv.KeyValue.PutString(ctx, key, value)
}

func (kv *opKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	_ = kv.record("Create", key)
	return kv.KeyValue.Create(ctx, key, value, opts...)
}

// interceptUpdate runs hook around the next Update of a key with prefix.
func (kv *opKV) interceptUpdate(prefix string, hook func(key string, value []byte, update func() (uint64, error)) (uint64, error)) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.onUpdate = func(key string, value []byte, update func() (uint64, error)) (uint64, error) {
		if !strings.HasPrefix(key, prefix) {
			return update()
		}
		kv.mu.Lock()
		kv.onUpdate = nil
		kv.mu.Unlock()
		return hook(key, value, update)
	}
}

func (kv *opKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	_ = kv.record("Update", key)
	update := func() (uint64, error) { return kv.KeyValue.Update(ctx, key, value, last) }
	kv.mu.Lock()
	hook := kv.onUpdate
	kv.mu.Unlock()
	if hook != nil {
		return hook(key, value, update)
	}
	return update()
}

func (kv *opKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	_ = kv.record("Delete", key)
	return kv.KeyValue.Delete(ctx, key, opts...)
}

func (kv *opKV) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	_ = kv.record("Purge", key)
	return kv.KeyValue.Purge(ctx, key, opts...)
}

func (kv *opKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	_ = kv.record("Watch", keys)
	return kv.KeyValue.Watch(ctx, keys, opts...)
}

func (kv *opKV) WatchAll(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	_ = kv.record("WatchAll", "*")
	return kv.KeyValue.WatchAll(ctx, opts...)
}

func (kv *opKV) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	_ = kv.record("WatchFiltered", strings.Join(keys, ","))
	return kv.KeyValue.WatchFiltered(ctx, keys, opts...)
}

func (kv *opKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	_ = kv.record("Keys", "*")
	return kv.KeyValue.Keys(ctx, opts...)
}

func (kv *opKV) ListKeys(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	_ = kv.record("ListKeys", "*")
	return kv.KeyValue.ListKeys(ctx, opts...)
}

func (kv *opKV) ListKeysFiltered(ctx context.Context, filters ...string) (jetstream.KeyLister, error) {
	_ = kv.record("ListKeysFiltered", strings.Join(filters, ","))
	return kv.KeyValue.ListKeysFiltered(ctx, filters...)
}

func (kv *opKV) History(ctx context.Context, key string, opts ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	_ = kv.record("History", key)
	return kv.KeyValue.History(ctx, key, opts...)
}

// ovnCreateHarness runs the OVN create path of a real ELBv2 service over a
// real VPC service. restart replaces the ELBv2 service over the same server.
type ovnCreateHarness struct {
	t        *testing.T
	nc       *nats.Conn
	js       jetstream.JetStream
	clock    *testClock
	vpcs     *ec2vpc.VPCServiceImpl
	launcher *mockSystemInstanceLauncher
	svc      *ELBv2ServiceImpl
	instance *fakeInstances

	elbKV, vpcKV, subnetKV, sgKV *opKV

	vpcID, subnetID, subnet2ID, az string
	groups                         []string
	foreignVPCGroup                string
	otherSubnet, otherGroup        string
}

func newOVNCreateHarness(t *testing.T) *ovnCreateHarness {
	t.Helper()
	ctx := t.Context()
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	vpcs, err := ec2vpc.NewVPCServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	h := &ovnCreateHarness{
		t: t, nc: nc, js: js, vpcs: vpcs,
		clock:    &testClock{t: time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)},
		launcher: &mockSystemInstanceLauncher{},
	}

	h.vpcID, h.subnetID = h.network(testAccountID, "10.81")
	h.subnet2ID = h.addSubnet(testAccountID, h.vpcID, "10.81.2.0/24")
	for _, name := range []string{"nlb-a", "nlb-b", "nlb-c"} {
		h.groups = append(h.groups, h.group(testAccountID, h.vpcID, name))
	}
	otherVPC, _ := h.network(testAccountID, "10.82")
	h.foreignVPCGroup = h.group(testAccountID, otherVPC, "other-vpc")
	foreignVPC, foreignSubnet := h.network(otherAccountID, "10.81")
	h.otherSubnet = foreignSubnet
	h.otherGroup = h.group(otherAccountID, foreignVPC, "other-account")

	sub, _, err := ec2vpc.NewParents(h.bucket(ec2vpc.KVBucketVPCs), h.bucket(ec2vpc.KVBucketSubnets)).Subnet(ctx, testAccountID, h.subnetID)
	require.NoError(t, err)
	require.NotNil(t, sub)
	h.az = sub.AvailabilityZone
	h.restart()
	return h
}

func (h *ovnCreateHarness) network(accountID, prefix string) (string, string) {
	out, err := h.vpcs.CreateVpc(h.t.Context(), &ec2.CreateVpcInput{CidrBlock: aws.String(prefix + ".0.0/16")}, accountID)
	require.NoError(h.t, err)
	vpcID := aws.StringValue(out.Vpc.VpcId)
	return vpcID, h.addSubnet(accountID, vpcID, prefix+".1.0/24")
}

func (h *ovnCreateHarness) addSubnet(accountID, vpcID, cidr string) string {
	out, err := h.vpcs.CreateSubnet(h.t.Context(), &ec2.CreateSubnetInput{VpcId: aws.String(vpcID), CidrBlock: aws.String(cidr)}, accountID)
	require.NoError(h.t, err)
	return aws.StringValue(out.Subnet.SubnetId)
}

func (h *ovnCreateHarness) group(accountID, vpcID, name string) string {
	out, err := h.vpcs.CreateSecurityGroup(h.t.Context(), &ec2.CreateSecurityGroupInput{
		GroupName: aws.String(name), Description: aws.String(name), VpcId: aws.String(vpcID),
	}, accountID)
	require.NoError(h.t, err)
	return aws.StringValue(out.GroupId)
}

func (h *ovnCreateHarness) bucket(name string) jetstream.KeyValue {
	kv, err := h.js.KeyValue(h.t.Context(), name)
	require.NoError(h.t, err)
	return kv
}

// restart builds a new ELBv2 service with the OVN create path over the same
// JetStream server, as a daemon restart would.
func (h *ovnCreateHarness) restart() {
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(h.t, err)
	svc, err := NewELBv2ServiceImplWithNATS(nil, h.nc, masterKey)
	require.NoError(h.t, err)
	h.t.Cleanup(svc.Close)
	svc.region = fixtureRegion
	svc.VPCService = h.vpcs
	svc.InstanceLauncher = h.launcher

	h.elbKV = &opKV{KeyValue: h.bucket(KVBucketELBv2)}
	h.vpcKV = &opKV{KeyValue: h.bucket(ec2vpc.KVBucketVPCs)}
	h.subnetKV = &opKV{KeyValue: h.bucket(ec2vpc.KVBucketSubnets)}
	h.sgKV = &opKV{KeyValue: h.bucket(ec2vpc.KVBucketSecurityGroups)}
	svc.store.kv = h.elbKV
	parents := ec2vpc.NewParents(h.vpcKV, h.subnetKV)
	owners := &fixtureOwnerStore{kv: h.elbKV, leaderGet: h.elbKV.leaderGet(h.js), parents: parents, now: h.clock.now, claimTTL: lbNameClaimTTL}
	if h.instance == nil {
		h.instance = newFakeInstances()
	}
	svc.ovn = &ovnFixture{owners: owners, parents: parents, groups: ec2vpc.NewGroupLookup(h.sgKV), instances: h.instance}
	h.vpcs.SetDependencyCheck(ovnDependencyKind, owners.vpcBlocksDeletion)
	h.vpcs.SetSubnetDependencyCheck(ovnDependencyKind, owners.subnetBlocksDeletion)
	h.svc = svc
}

func (h *ovnCreateHarness) input(name, subnetID string, groups ...string) *elbv2.CreateLoadBalancerInput {
	return &elbv2.CreateLoadBalancerInput{
		Name:           aws.String(name),
		Type:           aws.String(LoadBalancerTypeNetwork),
		Scheme:         aws.String(SchemeInternal),
		Subnets:        []*string{aws.String(subnetID)},
		SecurityGroups: aws.StringSlice(groups),
	}
}

func (h *ovnCreateHarness) create(input *elbv2.CreateLoadBalancerInput) (*elbv2.LoadBalancer, error) {
	out, err := h.svc.CreateLoadBalancer(h.t.Context(), input, testAccountID)
	if err != nil {
		return nil, err
	}
	require.Len(h.t, out.LoadBalancers, 1)
	return out.LoadBalancers[0], nil
}

// takeOps returns the calls on every wrapped bucket since the last take.
func (h *ovnCreateHarness) takeOps() map[string][]string {
	return map[string][]string{
		KVBucketELBv2: h.elbKV.take(), ec2vpc.KVBucketVPCs: h.vpcKV.take(),
		ec2vpc.KVBucketSubnets: h.subnetKV.take(), ec2vpc.KVBucketSecurityGroups: h.sgKV.take(),
	}
}

// state reads every key and revision of a bucket, outside the wrapped KVs.
func (h *ovnCreateHarness) state(bucket string) map[string]uint64 {
	ctx := h.t.Context()
	kv, err := h.js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return map[string]uint64{}
	}
	require.NoError(h.t, err)
	keys, err := kv.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return map[string]uint64{}
	}
	require.NoError(h.t, err)
	out := make(map[string]uint64, len(keys))
	for _, k := range keys {
		e, err := kv.Get(ctx, k)
		require.NoError(h.t, err)
		out[k] = e.Revision()
	}
	return out
}

// sideEffectBuckets are the buckets an OVN create must not change: ENIs,
// security groups (no managed group), route tables and IP allocations.
var sideEffectBuckets = []string{ec2vpc.KVBucketENIs, ec2vpc.KVBucketSecurityGroups, fenceRouteTableBucket, ec2vpc.KVBucketIPAM}

func (h *ovnCreateHarness) sideEffects() map[string]map[string]uint64 {
	out := map[string]map[string]uint64{}
	for _, b := range sideEffectBuckets {
		out[b] = h.state(b)
	}
	return out
}

func (h *ovnCreateHarness) launches() int {
	h.launcher.mu.Lock()
	defer h.launcher.mu.Unlock()
	return len(h.launcher.launchCalls)
}

func (h *ovnCreateHarness) slots(subnetID string) []string {
	return []string{vpcSlotKey(testAccountID, h.vpcID), subnetSlotKey(testAccountID, subnetID)}
}

func TestOVNFixtureCreate_ReturnsProvisioningRootAndRetriesAfterRestart(t *testing.T) {
	h := newOVNCreateHarness(t)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	h.takeOps()

	lb, err := h.create(h.input("ovn-nlb", h.subnetID, h.groups[0]))
	require.NoError(t, err)
	ops := h.takeOps()

	assert.Equal(t, StateProvisioning, aws.StringValue(lb.State.Code))
	assert.Nil(t, lb.DNSName, "an OVN root has no endpoint while it provisions")
	assert.Equal(t, LoadBalancerTypeNetwork, aws.StringValue(lb.Type))
	assert.Equal(t, SchemeInternal, aws.StringValue(lb.Scheme))
	assert.Equal(t, IPAddressTypeIPv4, aws.StringValue(lb.IpAddressType))
	assert.Equal(t, h.vpcID, aws.StringValue(lb.VpcId))
	assert.Equal(t, []string{h.groups[0]}, aws.StringValueSlice(lb.SecurityGroups))
	require.Len(t, lb.AvailabilityZones, 1)
	assert.Equal(t, h.subnetID, aws.StringValue(lb.AvailabilityZones[0].SubnetId))
	assert.Equal(t, h.az, aws.StringValue(lb.AvailabilityZones[0].ZoneName))
	assert.Empty(t, lb.AvailabilityZones[0].LoadBalancerAddresses)
	parsed, ok := resourcearn.ParseELBv2(aws.StringValue(lb.LoadBalancerArn))
	require.True(t, ok)
	assert.Equal(t, fixtureRegion, parsed.Region)
	assert.Equal(t, testAccountID, parsed.AccountID)

	assert.Equal(t, []string{"Get " + kvstore.AccountKey(testAccountID, h.groups[0])}, ops[ec2vpc.KVBucketSecurityGroups],
		"one supplied group costs one point read")
	for bucket, calls := range ops {
		assert.Empty(t, opsOf(calls, opKVLists), "bucket-wide read on %s", bucket)
	}

	elbAfter := h.state(KVBucketELBv2)
	var added []string
	for k, rev := range elbAfter {
		if old, ok := elbBefore[k]; !ok {
			added = append(added, k)
		} else {
			assert.Equal(t, old, rev, "create changed existing key %s", k)
		}
	}
	assert.ElementsMatch(t, append([]string{LBNameKey("ovn-nlb", testAccountID)}, h.slots(h.subnetID)...), added,
		"create writes only the claim and its two parent slots: no lb., lbhealth. or target group record")
	assert.Equal(t, sideBefore, h.sideEffects(), "no ENI, managed group or route table write")
	assert.Zero(t, h.launches(), "no appliance launch")

	h.restart()
	again, err := h.create(h.input("ovn-nlb", h.subnetID, h.groups[0]))
	require.NoError(t, err)
	assert.Equal(t, aws.StringValue(lb.LoadBalancerArn), aws.StringValue(again.LoadBalancerArn))
	assert.Equal(t, aws.TimeValue(lb.CreatedTime), aws.TimeValue(again.CreatedTime))
	assert.Nil(t, again.DNSName)
	ops = h.takeOps()
	assert.Equal(t, []string{"LeaderGet " + LBNameKey("ovn-nlb", testAccountID)}, ops[KVBucketELBv2],
		"an equal retry is one point read of the sealed claim")
	assert.Empty(t, ops[ec2vpc.KVBucketSubnets], "an equal retry does not re-validate parents")
	assert.Empty(t, ops[ec2vpc.KVBucketSecurityGroups])
	assert.Equal(t, elbAfter, h.state(KVBucketELBv2))

	_, err = h.create(h.input("ovn-nlb", h.subnetID, h.groups[1]))
	requireFenceCode(t, err, awserrors.ErrorELBv2DuplicateLoadBalancer)
	assert.Equal(t, elbAfter, h.state(KVBucketELBv2))

	h.takeOps()
	_, err = h.vpcs.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnetID)}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorDependencyViolation, "the restarted service keeps the subnet slot binding")
	assert.Contains(t, h.takeOps()[KVBucketELBv2], "LeaderGet "+subnetSlotKey(testAccountID, h.subnetID))
	sub, _, err := ec2vpc.NewParents(h.bucket(ec2vpc.KVBucketVPCs), h.bucket(ec2vpc.KVBucketSubnets)).Subnet(t.Context(), testAccountID, h.subnetID)
	require.NoError(t, err)
	require.NotNil(t, sub, "the subnet survives")
	assert.False(t, sub.Deleting, "a refused delete leaves no mark")
	assert.Equal(t, elbAfter, h.state(KVBucketELBv2), "the claim and both slots survive")

	_, err = h.vpcs.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnet2ID)}, testAccountID)
	require.NoError(t, err, "a subnet with no slot still deletes")
}

func TestOVNFixtureCreate_SubnetMappingAndGroupPointReads(t *testing.T) {
	h := newOVNCreateHarness(t)
	input := h.input("ovn-mapped", h.subnetID, h.groups...)
	input.Subnets = nil
	input.SubnetMappings = []*elbv2.SubnetMapping{{SubnetId: aws.String(h.subnetID)}}
	h.takeOps()

	lb, err := h.create(input)
	require.NoError(t, err)
	assert.Equal(t, h.groups, aws.StringValueSlice(lb.SecurityGroups))
	ops := h.takeOps()
	var want []string
	for _, g := range h.groups {
		want = append(want, "Get "+kvstore.AccountKey(testAccountID, g))
	}
	assert.Equal(t, want, ops[ec2vpc.KVBucketSecurityGroups], "each supplied group costs exactly one point read")
	require.NotEmpty(t, ops[ec2vpc.KVBucketSubnets])
	assert.Equal(t, "Get "+kvstore.AccountKey(testAccountID, h.subnetID), ops[ec2vpc.KVBucketSubnets][0], "the mapped subnet is the one validated")
}

func TestOVNFixtureCreate_FailedParentReadWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   func(h *ovnCreateHarness) *opKV
	}{
		{"security group", func(h *ovnCreateHarness) *opKV { return h.sgKV }},
		{"subnet", func(h *ovnCreateHarness) *opKV { return h.subnetKV }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			elbBefore := h.state(KVBucketELBv2)
			tc.kv(h).setFailGet(errors.New("simulated read failure"))
			h.takeOps()

			_, err := h.create(h.input("ovn-nlb", h.subnetID, h.groups[0]))
			requireFenceCode(t, err, awserrors.ErrorServerInternal)
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, opsOf(calls, opKVWrites), "write on %s after a failed read", bucket)
				assert.Empty(t, opsOf(calls, opKVLists), "bucket-wide read on %s", bucket)
			}
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "no claim or slot after a failed read")
		})
	}
}

func TestOVNFixtureCreate_RejectsUnsupportedShapeBeforeAnyRead(t *testing.T) {
	h := newOVNCreateHarness(t)
	g := h.groups[0]
	mutate := func(f func(in *elbv2.CreateLoadBalancerInput)) *elbv2.CreateLoadBalancerInput {
		in := h.input("ovn-nlb", h.subnetID, g)
		f(in)
		return in
	}
	mapping := func(m *elbv2.SubnetMapping) *elbv2.CreateLoadBalancerInput {
		return mutate(func(in *elbv2.CreateLoadBalancerInput) {
			in.Subnets = nil
			m.SubnetId = aws.String(h.subnetID)
			in.SubnetMappings = []*elbv2.SubnetMapping{m}
		})
	}
	six := []string{g, "sg-2", "sg-3", "sg-4", "sg-5", "sg-6"}
	for name, tc := range map[string]struct {
		input *elbv2.CreateLoadBalancerInput
		code  string
	}{
		"missing name":     {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = nil }), awserrors.ErrorMissingParameter},
		"name too long":    {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = aws.String(strings.Repeat("a", 33)) }), awserrors.ErrorValidationError},
		"leading hyphen":   {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = aws.String("-nlb") }), awserrors.ErrorValidationError},
		"trailing hyphen":  {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = aws.String("nlb-") }), awserrors.ErrorValidationError},
		"bad character":    {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = aws.String("nlb_a") }), awserrors.ErrorValidationError},
		"internal prefix":  {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Name = aws.String("internal-nlb") }), awserrors.ErrorValidationError},
		"empty tag key":    {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Tags = []*elbv2.Tag{{Key: aws.String("")}} }), awserrors.ErrorValidationError},
		"application type": {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Type = aws.String(LoadBalancerTypeApplication) }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"default type":     {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Type = nil }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"internet-facing":  {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Scheme = aws.String(SchemeInternetFacing) }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"dualstack":        {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.IpAddressType = aws.String("dualstack") }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"customer pool":    {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.CustomerOwnedIpv4Pool = aws.String("ipv4pool-coip-1") }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"no subnet":        {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Subnets = nil }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"empty subnet":     {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.Subnets = []*string{aws.String("")} }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"two subnets": {mutate(func(in *elbv2.CreateLoadBalancerInput) {
			in.Subnets = aws.StringSlice([]string{h.subnetID, h.subnet2ID})
		}), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"subnets and mapping": {mutate(func(in *elbv2.CreateLoadBalancerInput) {
			in.SubnetMappings = []*elbv2.SubnetMapping{{SubnetId: aws.String(h.subnetID)}}
		}), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"pinned address":  {mapping(&elbv2.SubnetMapping{PrivateIPv4Address: aws.String("10.81.1.10")}), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"allocation ID":   {mapping(&elbv2.SubnetMapping{AllocationId: aws.String("eipalloc-1")}), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"IPv6 address":    {mapping(&elbv2.SubnetMapping{IPv6Address: aws.String("fd00::10")}), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"no groups":       {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.SecurityGroups = nil }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"six groups":      {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.SecurityGroups = aws.StringSlice(six) }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"duplicate group": {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.SecurityGroups = aws.StringSlice([]string{g, g}) }), awserrors.ErrorELBv2InvalidConfigurationRequest},
		"empty group":     {mutate(func(in *elbv2.CreateLoadBalancerInput) { in.SecurityGroups = aws.StringSlice([]string{""}) }), awserrors.ErrorELBv2InvalidConfigurationRequest},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.create(tc.input)
			requireFenceCode(t, err, tc.code)
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, calls, "shape check touched %s", bucket)
			}
		})
	}
}

func TestOVNFixtureCreate_RejectsMissingOrForeignParents(t *testing.T) {
	h := newOVNCreateHarness(t)
	for name, tc := range map[string]struct {
		subnet, group, code string
	}{
		"missing subnet":        {"subnet-00000000000000000", h.groups[0], awserrors.ErrorELBv2SubnetNotFound},
		"other account subnet":  {h.otherSubnet, h.groups[0], awserrors.ErrorELBv2SubnetNotFound},
		"missing group":         {h.subnetID, "sg-00000000000000000", awserrors.ErrorELBv2InvalidSecurityGroup},
		"group in other VPC":    {h.subnetID, h.foreignVPCGroup, awserrors.ErrorELBv2InvalidSecurityGroup},
		"other account group":   {h.subnetID, h.otherGroup, awserrors.ErrorELBv2InvalidSecurityGroup},
		"second group is wrong": {h.subnetID, "", awserrors.ErrorELBv2InvalidSecurityGroup},
	} {
		t.Run(name, func(t *testing.T) {
			groups := []string{tc.group}
			if tc.group == "" {
				groups = []string{h.groups[0], h.foreignVPCGroup}
			}
			elbBefore := h.state(KVBucketELBv2)
			h.takeOps()
			_, err := h.create(h.input("ovn-nlb", tc.subnet, groups...))
			requireFenceCode(t, err, tc.code)
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, opsOf(calls, opKVWrites), "write on %s for a rejected parent", bucket)
			}
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2))
		})
	}
}

func TestOVNFixtureCreate_OccupiedParentRejectsAnotherLoadBalancer(t *testing.T) {
	h := newOVNCreateHarness(t)
	first, err := h.create(h.input("ovn-first", h.subnetID, h.groups[0]))
	require.NoError(t, err)
	held := h.state(KVBucketELBv2)

	for name, subnetID := range map[string]string{"same subnet": h.subnetID, "same VPC": h.subnet2ID} {
		t.Run(name, func(t *testing.T) {
			_, err := h.create(h.input("ovn-second", subnetID, h.groups[0]))
			requireFenceCode(t, err, awserrors.ErrorELBv2InvalidConfigurationRequest)
			after := h.state(KVBucketELBv2)
			assert.NotContains(t, after, LBNameKey("ovn-second", testAccountID), "a refused create keeps no claim")
			assert.NotContains(t, after, subnetSlotKey(testAccountID, h.subnet2ID))
			for _, k := range h.slots(h.subnetID) {
				assert.Equal(t, held[k], after[k], "the sealed owner keeps slot %s", k)
			}
		})
	}

	again, err := h.create(h.input("ovn-first", h.subnetID, h.groups[0]))
	require.NoError(t, err)
	assert.Equal(t, aws.StringValue(first.LoadBalancerArn), aws.StringValue(again.LoadBalancerArn))
}

func TestOVNFixtureCreate_DefaultServiceKeepsAppliancePath(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)
	require.Nil(t, svc.ovn)
	mock := &mockSystemInstanceLauncher{launchResult: &SystemInstanceOutput{InstanceID: "i-default", PrivateIP: "10.0.1.9"}}
	svc.InstanceLauncher = mock

	out, err := svc.CreateLoadBalancer(t.Context(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("appliance-nlb"),
		Type:    aws.String(LoadBalancerTypeNetwork),
		Scheme:  aws.String(SchemeInternal),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	require.Len(t, out.LoadBalancers, 1)
	assert.NotNil(t, out.LoadBalancers[0].DNSName, "the appliance path still assigns a DNS name")
	require.Len(t, mock.launchCalls, 1, "the appliance path still launches its VM")
	keys, err := svc.store.kv.Keys(t.Context())
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, KeyPrefixLB) }), "appliance record under lb.")
	assert.False(t, slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "ovnslot.") }), "no OVN parent slot")
}
