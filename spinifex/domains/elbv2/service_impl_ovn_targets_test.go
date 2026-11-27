package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstances serves the EC2 named projection over in-memory instances, so
// the fixture sees EC2's visibility and primary interface rules unchanged.
type fakeInstances struct {
	mu     sync.Mutex
	vms    map[string]*vm.VM
	calls  []string
	fail   error
	onRead func(id string)
}

var _ projection.NamedInstanceLookup = (*fakeInstances)(nil)

func newFakeInstances() *fakeInstances { return &fakeInstances{vms: map[string]*vm.VM{}} }

func (f *fakeInstances) NamedInstance(_ context.Context, accountID, id string) (*projection.NamedInstance, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	hook, fail := f.onRead, f.fail
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if fail != nil {
		return nil, fail
	}
	f.mu.Lock()
	v := f.vms[id]
	f.mu.Unlock()
	if v == nil {
		return nil, nil
	}
	return instance.ProjectNamedInstance(accountID, id, v), nil
}

func (f *fakeInstances) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func (f *fakeInstances) set(id string, edit func(*vm.VM)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.vms[id])
}

func targetID(n int) string { return fmt.Sprintf("i-%017x", n) }

func instanceNIC(eni, vpc, subnet string, index int64) *ec2.InstanceNetworkInterface {
	return &ec2.InstanceNetworkInterface{
		NetworkInterfaceId: aws.String(eni), VpcId: aws.String(vpc), SubnetId: aws.String(subnet),
		Attachment: &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(index)},
	}
}

// running records a running instance of the test account with its primary
// interface in the fixture VPC, and returns its ID.
func (h *ovnCreateHarness) running(n int) string {
	id := targetID(n)
	eni := fmt.Sprintf("eni-%017x", n)
	h.instance.mu.Lock()
	h.instance.vms[id] = &vm.VM{
		ID: id, AccountID: testAccountID, Status: vm.StateRunning, ENIId: eni,
		Instance: &ec2.Instance{InstanceId: aws.String(id), NetworkInterfaces: []*ec2.InstanceNetworkInterface{
			instanceNIC(eni, h.vpcID, h.subnetID, 0),
		}},
	}
	h.instance.mu.Unlock()
	return id
}

// bound creates the fixture load balancer, a TCP group on port 8080 and the
// listener forwarding to it, and returns the group ARN.
func (h *ovnCreateHarness) bound() string {
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	_, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(h.t, err)
	h.takeOps()
	h.instance.take()
	return tgArn
}

func descs(targets ...fixtureTarget) []*elbv2.TargetDescription {
	out := make([]*elbv2.TargetDescription, 0, len(targets))
	for _, t := range targets {
		d := &elbv2.TargetDescription{Id: aws.String(t.InstanceID)}
		if t.Port != 0 {
			d.Port = aws.Int64(t.Port)
		}
		out = append(out, d)
	}
	return out
}

func (h *ovnCreateHarness) register(tgArn string, targets ...fixtureTarget) error {
	_, err := h.svc.RegisterTargets(h.t.Context(), &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(targets...)}, testAccountID)
	return err
}

func (h *ovnCreateHarness) deregister(tgArn string, targets ...fixtureTarget) error {
	_, err := h.svc.DeregisterTargets(h.t.Context(), &elbv2.DeregisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(targets...)}, testAccountID)
	return err
}

// tuple is a target with an explicit port, or 0 for the group default.
func tuple(id string, port int64) fixtureTarget { return fixtureTarget{InstanceID: id, Port: port} }

func TestOVNFixtureTargets_RegisterSealsSetAndRepeats(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a, b := h.running(1), h.running(2)
	nameKey := LBNameKey("ovn-nlb", testAccountID)
	reads := []string{"Get " + KeyPrefixTG + groupID(tgArn), "LeaderGet " + vpcSlotKey(testAccountID, h.vpcID), "LeaderGet " + nameKey, "LeaderGet " + nameKey}
	tgBefore, err := h.svc.store.GetTargetGroupByArn(t.Context(), tgArn)
	require.NoError(t, err)
	sideBefore := h.sideEffects()
	h.takeOps()

	require.NoError(t, h.register(tgArn, tuple(b, 9000), tuple(a, 0), tuple(a, 0)))
	h.requireCalls(map[string][]string{KVBucketELBv2: append(slices.Clone(reads), "Update "+nameKey)})
	assert.Equal(t, []string{b, a}, h.instance.take(), "one named read per new instance, in request order")
	owner := h.owner("ovn-nlb")
	assert.Equal(t, uint64(3), owner.OwnerRevision)
	assert.Equal(t, []fixtureTarget{{a, 8080}, {b, 9000}}, owner.Listener.Targets, "sorted, default port resolved, repeat dropped")
	tgAfter, err := h.svc.store.GetTargetGroupByArn(t.Context(), tgArn)
	require.NoError(t, err)
	assert.Equal(t, tgBefore, tgAfter, "the group record is not a second copy of the set")
	assert.Equal(t, sideBefore, h.sideEffects())
	assert.Zero(t, h.launches())

	elbBefore := h.state(KVBucketELBv2)
	h.takeOps()
	require.NoError(t, h.register(tgArn, tuple(a, 8080), tuple(b, 9000)), "an exact duplicate is a no-op")
	h.requireCalls(map[string][]string{KVBucketELBv2: reads})
	assert.Empty(t, h.instance.take(), "a present tuple needs no instance read")
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2))

	require.NoError(t, h.register(tgArn, tuple(a, 8081)), "another port of the same instance is a new tuple")
	assert.Equal(t, []string{a}, h.instance.take())
	assert.Equal(t, []fixtureTarget{{a, 8080}, {a, 8081}, {b, 9000}}, h.owner("ovn-nlb").Listener.Targets)

	desc, err := h.describe(testAccountID, byARN(owner.Root.LoadBalancerArn))
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, aws.StringValue(desc.State.Code), "membership does not activate the load balancer")
}

func TestOVNFixtureTargets_DeregisterRemovesTuplesWithoutReads(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a, b := h.running(1), h.running(2)
	require.NoError(t, h.register(tgArn, tuple(a, 0), tuple(b, 0), tuple(b, 9000)))
	h.instance.take()
	delete(h.instance.vms, b)

	require.NoError(t, h.deregister(tgArn, tuple(b, 0)), "an instance that has gone still deregisters")
	assert.Empty(t, h.instance.take())
	owner := h.owner("ovn-nlb")
	assert.Equal(t, uint64(4), owner.OwnerRevision)
	assert.Equal(t, []fixtureTarget{{a, 8080}, {b, 9000}}, owner.Listener.Targets)

	elbBefore := h.state(KVBucketELBv2)
	h.takeOps()
	require.NoError(t, h.deregister(tgArn, tuple(b, 0), tuple(h.running(3), 0)), "removing a missing tuple is a no-op")
	for bucket, calls := range h.takeOps() {
		assert.Empty(t, opsOf(calls, opKVWrites), "write on %s", bucket)
	}
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2))

	require.NoError(t, h.deregister(tgArn, tuple(a, 8080), tuple(b, 9000)))
	owner = h.owner("ovn-nlb")
	assert.Equal(t, uint64(5), owner.OwnerRevision)
	assert.Empty(t, owner.Listener.Targets)
	assert.Empty(t, h.instance.take())
}

func TestOVNFixtureTargets_InvalidBatchKeepsSnapshot(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	present := h.running(1)
	require.NoError(t, h.register(tgArn, tuple(present, 0)))
	fresh := h.running(2)
	now := time.Now()
	for n, edit := range map[int]func(*vm.VM){
		10: func(v *vm.VM) { v.Status = vm.StateStopped },
		11: func(v *vm.VM) { v.Status = vm.StatePending },
		12: func(v *vm.VM) { v.Status = vm.StateShuttingDown },
		13: func(v *vm.VM) { v.DeletionTimestamp = &now },
		14: func(v *vm.VM) { v.ManagedBy = "elbv2" },
		15: func(v *vm.VM) { v.AccountID = otherAccountID },
		16: func(v *vm.VM) {
			v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{
				instanceNIC("eni-second", h.vpcID, h.subnetID, 1), instanceNIC(v.ENIId, "vpc-other", "subnet-other", 0),
			}
		},
		17: func(v *vm.VM) { v.ENIId = "" },
		18: func(v *vm.VM) { v.Instance.NetworkInterfaces = nil },
	} {
		h.running(n)
		h.instance.set(targetID(n), edit)
	}
	want := h.owner("ovn-nlb")
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	h.takeOps()
	h.instance.take()

	cases := map[string]struct {
		id   string
		code string
	}{
		"stopped":                         {targetID(10), awserrors.ErrorELBv2InvalidTarget},
		"pending":                         {targetID(11), awserrors.ErrorELBv2InvalidTarget},
		"shutting down":                   {targetID(12), awserrors.ErrorELBv2InvalidTarget},
		"terminate decided":               {targetID(13), awserrors.ErrorELBv2InvalidTarget},
		"hidden managed":                  {targetID(14), awserrors.ErrorELBv2InvalidTarget},
		"other account":                   {targetID(15), awserrors.ErrorELBv2InvalidTarget},
		"primary elsewhere, secondary in": {targetID(16), awserrors.ErrorELBv2InvalidTarget},
		"no recorded primary":             {targetID(17), awserrors.ErrorELBv2InvalidTarget},
		"no interfaces":                   {targetID(18), awserrors.ErrorELBv2InvalidTarget},
		"absent":                          {targetID(19), awserrors.ErrorELBv2InvalidTarget},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := h.register(tgArn, tuple(present, 0), tuple(fresh, 0), tuple(tc.id, 0))
			requireFenceCode(t, err, tc.code)
			assert.Equal(t, []string{fresh, tc.id}, h.instance.take(), "the whole batch is checked before the write")
			h.requireNoEffects(elbBefore, sideBefore)
			assert.Equal(t, want, h.owner("ovn-nlb"), "the revision and the whole set stay as they were")
		})
	}
	t.Run("failed read", func(t *testing.T) {
		h.instance.fail = errors.New("injected read failure")
		defer func() { h.instance.fail = nil }()
		requireFenceCode(t, h.register(tgArn, tuple(fresh, 0)), awserrors.ErrorServerInternal)
		h.requireNoEffects(elbBefore, sideBefore)
		assert.Equal(t, want, h.owner("ovn-nlb"))
	})
}

func TestOVNFixtureTargets_RefusesRequestShapesBeforeReads(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	id := h.running(1)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	many := make([]*elbv2.TargetDescription, fixtureMaxTargets+1)
	for i := range many {
		many[i] = &elbv2.TargetDescription{Id: aws.String(targetID(100 + i))}
	}
	one := func(edit func(*elbv2.TargetDescription)) []*elbv2.TargetDescription {
		d := &elbv2.TargetDescription{Id: aws.String(id)}
		edit(d)
		return []*elbv2.TargetDescription{d}
	}
	group := tgArn
	for name, tc := range map[string]struct {
		arn     *string
		targets []*elbv2.TargetDescription
		code    string
	}{
		"no group":            {nil, one(func(*elbv2.TargetDescription) {}), awserrors.ErrorMissingParameter},
		"no targets":          {&group, nil, awserrors.ErrorMissingParameter},
		"too many targets":    {&group, many, awserrors.ErrorValidationError},
		"nil target":          {&group, []*elbv2.TargetDescription{nil}, awserrors.ErrorValidationError},
		"no ID":               {&group, one(func(d *elbv2.TargetDescription) { d.Id = nil }), awserrors.ErrorValidationError},
		"availability zone":   {&group, one(func(d *elbv2.TargetDescription) { d.AvailabilityZone = aws.String(h.az) }), awserrors.ErrorValidationError},
		"port 0":              {&group, one(func(d *elbv2.TargetDescription) { d.Port = aws.Int64(0) }), awserrors.ErrorValidationError},
		"port 65536":          {&group, one(func(d *elbv2.TargetDescription) { d.Port = aws.Int64(65536) }), awserrors.ErrorValidationError},
		"IP target":           {&group, one(func(d *elbv2.TargetDescription) { d.Id = aws.String("10.81.1.9") }), awserrors.ErrorELBv2InvalidTarget},
		"key characters":      {&group, one(func(d *elbv2.TargetDescription) { d.Id = aws.String("i-0123456*") }), awserrors.ErrorELBv2InvalidTarget},
		"malformed group":     {aws.String(tgArn + ".x"), one(func(*elbv2.TargetDescription) {}), awserrors.ErrorELBv2TargetGroupNotFound},
		"other account group": {aws.String(resourcearn.FormatELBv2TargetGroup(fixtureRegion, otherAccountID, "ovn-tg", groupID(tgArn))), one(func(*elbv2.TargetDescription) {}), awserrors.ErrorELBv2TargetGroupNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.svc.RegisterTargets(t.Context(), &elbv2.RegisterTargetsInput{TargetGroupArn: tc.arn, Targets: tc.targets}, testAccountID)
			requireFenceCode(t, err, tc.code)
			_, err = h.svc.DeregisterTargets(t.Context(), &elbv2.DeregisterTargetsInput{TargetGroupArn: tc.arn, Targets: tc.targets}, testAccountID)
			requireFenceCode(t, err, tc.code)
			h.requireCalls(map[string][]string{})
			assert.Empty(t, h.instance.take())
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
	t.Run("nil input", func(t *testing.T) {
		_, err := h.svc.RegisterTargets(t.Context(), nil, testAccountID)
		requireFenceCode(t, err, awserrors.ErrorMissingParameter)
		_, err = h.svc.DeregisterTargets(t.Context(), nil, testAccountID)
		requireFenceCode(t, err, awserrors.ErrorMissingParameter)
		h.requireCalls(map[string][]string{})
	})
}

func TestOVNFixtureTargets_RefusesUnboundGroups(t *testing.T) {
	h := newOVNCreateHarness(t)
	h.bound()
	id := h.running(1)
	const unbound, kind = "forward target", "TCP instance"
	groups := map[string]struct{ arn, reason string }{
		"group without a listener": {aws.StringValue(h.tcpGroup("spare-tg", 8080).TargetGroupArn), unbound},
		"UDP group":                {h.storedGroup("b000000000000001", func(tg *TargetGroupRecord) { tg.Protocol = ProtocolUDP }), kind},
		"IP group":                 {h.storedGroup("b000000000000002", func(tg *TargetGroupRecord) { tg.TargetType = TargetTypeIP }), kind},
		"VPC without a slot":       {h.storedGroup("b000000000000003", func(tg *TargetGroupRecord) { tg.VpcId = "vpc-other" }), unbound},
	}
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	for name, tc := range groups {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			for _, err := range []error{h.register(tc.arn, tuple(id, 0)), h.deregister(tc.arn, tuple(id, 0))} {
				requireFenceCode(t, err, awserrors.ErrorELBv2InvalidConfigurationRequest)
				_, msg, _ := awserrors.ResolveErrorDetail(err)
				assert.Contains(t, msg, tc.reason)
			}
			assert.Empty(t, h.instance.take())
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
	t.Run("missing group", func(t *testing.T) {
		arn := resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "gone", "dddd000000000004")
		requireFenceCode(t, h.register(arn, tuple(id, 0)), awserrors.ErrorELBv2TargetGroupNotFound)
		h.requireNoEffects(elbBefore, sideBefore)
	})
	boundArn := groupsBound(h)
	for name, prefix := range map[string]string{"failed slot read": keyPrefixOVNVPCSlot, "failed owner read": KeyPrefixLBName} {
		t.Run(name, func(t *testing.T) {
			h.elbKV.setFailGetPrefix(prefix, errInjectedRead)
			defer h.elbKV.setFailGet(nil)
			requireFenceCode(t, h.register(boundArn, tuple(id, 0)), awserrors.ErrorServerInternal)
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
}

// groupsBound returns the ARN of the group the fixture listener forwards to.
func groupsBound(h *ovnCreateHarness) string {
	owner, err := h.svc.ovn.owners.GetSealedOwner(h.t.Context(), "ovn-nlb", testAccountID)
	require.NoError(h.t, err)
	return owner.Listener.TargetGroupArn
}

// A VPC slot that names another owner than the claim it points to does not bind
// the group, and no membership change reaches that claim.
func TestOVNFixtureTargets_StaleSlotOwnerRefused(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	id := h.running(1)
	kv := h.bucket(KVBucketELBv2)
	key := vpcSlotKey(testAccountID, h.vpcID)
	e, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var slot fixtureSlot
	require.NoError(t, json.Unmarshal(e.Value(), &slot))
	slot.OwnerID = "ffff000000000009"
	raw, err := json.Marshal(slot)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, raw)
	require.NoError(t, err)
	want := h.owner("ovn-nlb")
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	h.takeOps()

	requireFenceCode(t, h.register(tgArn, tuple(id, 0)), awserrors.ErrorELBv2InvalidConfigurationRequest)
	requireFenceCode(t, h.deregister(tgArn, tuple(id, 0)), awserrors.ErrorELBv2InvalidConfigurationRequest)
	assert.Empty(t, h.instance.take())
	h.requireNoEffects(elbBefore, sideBefore)
	assert.Equal(t, want, h.owner("ovn-nlb"))
}

func TestOVNFixtureTargets_SetLimit(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	var full []fixtureTarget
	for n := range fixtureMaxTargets {
		full = append(full, tuple(h.running(n+1), 0))
	}
	require.NoError(t, h.register(tgArn, full...), "a request of exactly the limit is accepted")
	assert.Len(t, h.instance.take(), fixtureMaxTargets, "one named read per instance")
	want := h.owner("ovn-nlb")
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	h.takeOps()

	err := h.register(tgArn, tuple(h.running(99), 0))
	requireFenceCode(t, err, awserrors.ErrorELBv2InvalidConfigurationRequest)
	h.requireNoEffects(elbBefore, sideBefore)
	assert.Equal(t, want, h.owner("ovn-nlb"))
	require.NoError(t, h.register(tgArn, full[0]), "a duplicate at the limit is still a no-op")
}

func TestOVNFixtureTargets_ConcurrentEditsBothCommit(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a, b := h.running(1), h.running(2)
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
		require.NoError(t, h.register(tgArn, tuple(b, 0)))
		return update()
	})

	require.NoError(t, h.register(tgArn, tuple(a, 0)))
	assert.Equal(t, []string{a, b}, h.instance.take(), "the retry reuses the request's earlier check of a")
	owner := h.owner("ovn-nlb")
	assert.Equal(t, uint64(4), owner.OwnerRevision, "one revision per committed edit")
	assert.Equal(t, []fixtureTarget{{a, 8080}, {b, 8080}}, owner.Listener.Targets)
}

func TestOVNFixtureTargets_RetryChecksTupleRemovedByWinner(t *testing.T) {
	for name, stopRemoved := range map[string]bool{"still running": false, "stopped meanwhile": true} {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			tgArn := h.bound()
			a, b := h.running(1), h.running(2)
			require.NoError(t, h.register(tgArn, tuple(a, 0)))
			h.instance.take()
			h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
				require.NoError(t, h.deregister(tgArn, tuple(a, 0)))
				if stopRemoved {
					h.instance.set(a, func(v *vm.VM) { v.Status = vm.StateStopped })
				}
				return update()
			})

			err := h.register(tgArn, tuple(a, 0), tuple(b, 0))
			assert.Equal(t, []string{b, a}, h.instance.take(), "a is checked once it becomes an addition")
			owner := h.owner("ovn-nlb")
			if stopRemoved {
				requireFenceCode(t, err, awserrors.ErrorELBv2InvalidTarget)
				assert.Equal(t, uint64(4), owner.OwnerRevision, "only the deregistration committed")
				assert.Empty(t, owner.Listener.Targets, "b is not committed without a")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, uint64(5), owner.OwnerRevision)
			assert.Equal(t, []fixtureTarget{{a, 8080}, {b, 8080}}, owner.Listener.Targets, "the complete batch commits")
		})
	}
}

func TestOVNFixtureTargets_UncertainEditIsServerError(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a := h.running(1)
	want := h.owner("ovn-nlb")
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(string, []byte, func() (uint64, error)) (uint64, error) {
		return 0, errors.New("injected timeout")
	})
	requireFenceCode(t, h.register(tgArn, tuple(a, 0)), awserrors.ErrorServerInternal)
	assert.Equal(t, want, h.owner("ovn-nlb"))
	require.NoError(t, h.register(tgArn, tuple(a, 0)), "a caller retry succeeds")
	assert.Len(t, h.owner("ovn-nlb").Listener.Targets, 1)
}
