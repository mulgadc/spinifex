package elbv2

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fenceVIPs gives the service one owner store over a fence handle, as the
// daemon shares one store between admission and the parent dependency checks,
// and puts the subnet check behind a hold.
func (h *ovnCreateHarness) fenceVIPs() (*fenceKV, *fenceCheck) {
	fk := &fenceKV{KeyValue: h.elbKV}
	owners := &fixtureOwnerStore{kv: fk, leaderGet: fk.leaderGet(h.js), parents: h.svc.ovn.parents, now: h.clock.now, claimTTL: lbNameClaimTTL}
	h.svc.ovn.owners = owners
	check := &fenceCheck{}
	check.set(owners.subnetBlocksDeletion)
	h.vpcs.SetDependencyCheck(ovnDependencyKind, owners.vpcBlocksDeletion)
	h.vpcs.SetSubnetDependencyCheck(ovnDependencyKind, check.call)
	return fk, check
}

// vipLoop runs one drift loop of a pass for arn under the real reconcile lease.
func (h *ovnCreateHarness) vipLoop(arn string) (*vipPass, chan<- struct{}) {
	lease := reconcile.NewLeaderLease(kvstore.OpenSyncBucket(h.js), reconcile.KeyVPCDReconcile, "vpcd-a")
	pass := newVIPPass(arn, h.vpcID)
	trigger, _ := h.drift(pass, lease)
	return pass, trigger
}

// startPass triggers a pass and hands it the service without waiting for it.
func (h *ovnCreateHarness) startPass(trigger chan<- struct{}, p *vipPass) {
	h.t.Helper()
	trigger <- struct{}{}
	select {
	case p.service <- h.svc:
	case <-time.After(intentDeadline):
		h.t.Fatal("the triggered pass did not reach the reconciler")
	}
}

// subnetAssociation is an explicit route table association made through the
// route table service.
type subnetAssociation struct {
	rtbs          *ec2routetable.RouteTableServiceImpl
	routeTableID  string
	associationID string
}

func (h *ovnCreateHarness) associateSubnet() subnetAssociation {
	h.t.Helper()
	ctx := h.t.Context()
	rtbs, err := ec2routetable.NewRouteTableServiceImplWithNATS(ctx, nil, h.nc)
	require.NoError(h.t, err)
	rtb, err := rtbs.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(h.vpcID)}, testAccountID)
	require.NoError(h.t, err)
	out, err := rtbs.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{RouteTableId: rtb.RouteTable.RouteTableId, SubnetId: aws.String(h.subnetID)}, testAccountID)
	require.NoError(h.t, err)
	a := subnetAssociation{rtbs: rtbs, routeTableID: aws.StringValue(rtb.RouteTable.RouteTableId), associationID: aws.StringValue(out.AssociationId)}
	h.requireAssociated(a)
	return a
}

// requireAssociated asserts the explicit association still names the subnet.
func (h *ovnCreateHarness) requireAssociated(a subnetAssociation) {
	h.t.Helper()
	out, err := a.rtbs.DescribeRouteTables(h.t.Context(), &ec2.DescribeRouteTablesInput{RouteTableIds: []*string{aws.String(a.routeTableID)}}, testAccountID)
	require.NoError(h.t, err)
	require.Len(h.t, out.RouteTables, 1)
	var subnets []string
	for _, assoc := range out.RouteTables[0].Associations {
		if aws.StringValue(assoc.RouteTableAssociationId) == a.associationID {
			subnets = append(subnets, aws.StringValue(assoc.SubnetId))
		}
	}
	assert.Equal(h.t, []string{h.subnetID}, subnets, "the explicit association survives")
}

// subnetRecord is the committed subnet entry, or nil when it is gone.
func (h *ovnCreateHarness) subnetRecord() (jetstream.KeyValueEntry, *ec2vpc.SubnetRecord) {
	h.t.Helper()
	e, err := h.bucket(ec2vpc.KVBucketSubnets).Get(h.t.Context(), kvstore.AccountKey(testAccountID, h.subnetID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	require.NoError(h.t, err)
	var rec ec2vpc.SubnetRecord
	require.NoError(h.t, json.Unmarshal(e.Value(), &rec))
	return e, &rec
}

func (h *ovnCreateHarness) deleteSubnet() error {
	_, err := h.vpcs.DeleteSubnet(h.t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnetID)}, testAccountID)
	return err
}

// requireDeleteRefusedIntact runs a real DeleteSubnet and asserts it is refused
// with code before any mark or cascade: the subnet, its association and both
// slots keep their revisions.
func (h *ovnCreateHarness) requireDeleteRefusedIntact(a subnetAssociation, code string) {
	h.t.Helper()
	before, _ := h.subnetRecord()
	require.NotNil(h.t, before)
	elbBefore := h.state(KVBucketELBv2)
	requireFenceCode(h.t, h.deleteSubnet(), code)
	after, rec := h.subnetRecord()
	require.NotNil(h.t, after, "the subnet survives")
	assert.Equal(h.t, before.Revision(), after.Revision(), "no mark was written")
	assert.False(h.t, rec.Deleting)
	h.requireAssociated(a)
	assert.Equal(h.t, elbBefore, h.state(KVBucketELBv2), "the owner and both slots stay")
	assert.NotEmpty(h.t, h.rawSlotOwner(vpcSlotKey(testAccountID, h.vpcID)))
	assert.NotEmpty(h.t, h.rawSlotOwner(subnetSlotKey(testAccountID, h.subnetID)))
}

// requireOneVIP asserts the subnet row holds exactly the one address under the
// load balancer ID.
func (h *ovnCreateHarness) requireOneVIP(token, ip string) {
	h.t.Helper()
	row, _ := h.ipamRow(h.subnetID)
	assert.Equal(h.t, []ec2vpc.IPEntry{{IP: ip, Purpose: ec2vpc.PurposeLBVIP, OwnerID: token}}, row.Allocated)
}

func (h *ovnCreateHarness) ipamRowAbsent() bool {
	h.t.Helper()
	_, err := h.bucket(ec2vpc.KVBucketIPAM).Get(h.t.Context(), h.subnetID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return true
	}
	require.NoError(h.t, err)
	return false
}

// A real DeleteSubnet during each stage of a held reservation pass is refused
// before it marks or cascades, and the pass then converges on one address.
func TestOVNFixtureVIP_DeleteSubnetDuringPassIsRefused(t *testing.T) {
	type barrier struct {
		reached <-chan struct{}
		free    func()
	}
	cases := map[string]func(fk *fenceKV, ipamKV *vipIPAMKV) barrier{
		"before the pass confirms the owner": func(fk *fenceKV, _ *vipIPAMKV) barrier {
			hold := fk.arm("Get", false, func(key string) bool { return key == LBNameKey("ovn-nlb", testAccountID) })
			return barrier{reached: hold.reached, free: sync.OnceFunc(hold.free)}
		},
		"after the evidence, before the allocator reads": func(_ *fenceKV, ipamKV *vipIPAMKV) barrier {
			reached, held := make(chan struct{}), make(chan struct{})
			ipamKV.read = func() { close(reached); <-held }
			return barrier{reached: reached, free: sync.OnceFunc(func() { close(held) })}
		},
		"while the IPAM write is pending": func(_ *fenceKV, ipamKV *vipIPAMKV) barrier {
			reached, held := make(chan struct{}), make(chan struct{})
			ipamKV.hook = func(write func() (uint64, error)) (uint64, error) {
				close(reached)
				<-held
				return write()
			}
			return barrier{reached: reached, free: sync.OnceFunc(func() { close(held) })}
		},
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			fk, check := h.fenceVIPs()
			arn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
			token := h.owner("ovn-nlb").Root.LoadBalancerID
			assoc := h.associateSubnet()
			ipamKV := h.openVIPs()
			pass, trigger := h.vipLoop(arn)

			b := arm(fk, ipamKV)
			defer b.free()
			h.startPass(trigger, pass)
			select {
			case <-b.reached:
			case <-time.After(intentDeadline):
				t.Fatal("the pass did not reach its barrier")
			}

			h.requireDeleteRefusedIntact(assoc, awserrors.ErrorDependencyViolation)
			assert.Equal(t, 1, check.runs(), "one dependency check, then the refusal")
			assert.True(t, h.ipamRowAbsent(), "the held pass has not committed an address")

			b.free()
			got := h.awaitPass(pass)
			require.NoError(t, got.err)
			assert.Equal(t, "10.81.1.4", got.ip)
			h.requireOneVIP(token, got.ip)
			h.requireDeleteRefusedIntact(assoc, awserrors.ErrorDependencyViolation)
		})
	}
}

// A stale ordinary read that misses the slots neither lets the subnet go nor
// stops the reservation. A failed leader read of the slot is an error to both.
func TestOVNFixtureVIP_SlotReadFaults(t *testing.T) {
	t.Run("stale ordinary slot miss", func(t *testing.T) {
		h := newOVNCreateHarness(t)
		h.fenceVIPs()
		arn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
		token := h.owner("ovn-nlb").Root.LoadBalancerID
		assoc := h.associateSubnet()
		h.openVIPs()
		h.elbKV.lag(vpcSlotKey(testAccountID, h.vpcID), nil)
		h.elbKV.lag(subnetSlotKey(testAccountID, h.subnetID), nil)
		_, err := h.elbKV.Get(t.Context(), subnetSlotKey(testAccountID, h.subnetID))
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "an ordinary read misses the slot")

		h.requireDeleteRefusedIntact(assoc, awserrors.ErrorDependencyViolation)
		pass, trigger := h.vipLoop(arn)
		got := h.runPass(trigger, pass)
		require.NoError(t, got.err)
		h.requireOneVIP(token, got.ip)
		h.requireDeleteRefusedIntact(assoc, awserrors.ErrorDependencyViolation)
	})

	t.Run("failed leader slot read", func(t *testing.T) {
		h := newOVNCreateHarness(t)
		fk, _ := h.fenceVIPs()
		arn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
		assoc := h.associateSubnet()
		ipamKV := h.openVIPs()
		injected := errors.New("injected leader read failure")
		fk.setFailGet(func(key string) error {
			if key == subnetSlotKey(testAccountID, h.subnetID) {
				return injected
			}
			return nil
		})

		h.requireDeleteRefusedIntact(assoc, awserrors.ErrorServerInternal)
		pass, trigger := h.vipLoop(arn)
		got := h.runPass(trigger, pass)
		require.ErrorIs(t, got.err, injected, "an unread slot is not absence")
		assert.Empty(t, got.ip)
		assert.Zero(t, ipamKV.writeCount())
		assert.True(t, h.ipamRowAbsent(), "no new reservation")
		h.requireDeleteRefusedIntact(assoc, awserrors.ErrorServerInternal)
	})
}

// A DeleteSubnet held after its first dependency check loses its stale mark to
// the real Create's touch and seal, rechecks and is refused; the association,
// the unmarked subnet and the address remain.
func TestOVNFixtureVIP_StaleDeleteLosesToAdmission(t *testing.T) {
	h := newOVNCreateHarness(t)
	_, check := h.fenceVIPs()
	assoc := h.associateSubnet()
	h.openVIPs()

	hold := check.arm()
	free := sync.OnceFunc(hold.free)
	defer free()
	deleted := fenceAsync(h.deleteSubnet)
	hold.wait(t)

	lb := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(lb.LoadBalancerArn)
	token := h.owner("ovn-nlb").Root.LoadBalancerID
	pass, trigger := h.vipLoop(arn)
	got := h.runPass(trigger, pass)
	require.NoError(t, got.err)
	h.requireOneVIP(token, got.ip)

	free()
	select {
	case err := <-deleted:
		requireFenceCode(t, err, awserrors.ErrorDependencyViolation)
	case <-time.After(fenceHoldWait):
		t.Fatal("the resumed delete did not return")
	}
	assert.Equal(t, 2, check.runs(), "the stale mark lost and the delete checked again")
	_, rec := h.subnetRecord()
	require.NotNil(t, rec, "the subnet survives")
	assert.False(t, rec.Deleting, "the subnet is unmarked")
	h.requireAssociated(assoc)
	h.requireOneVIP(token, got.ip)
	assert.NotEmpty(t, h.rawSlotOwner(subnetSlotKey(testAccountID, h.subnetID)))
}

// A DeleteSubnet that commits while Create waits to publish its slots wins:
// the resumed Create and the reservation path leave no sealed owner, no slot
// and no allocator write.
func TestOVNFixtureVIP_DeleteBeforeSlotsWins(t *testing.T) {
	h := newOVNCreateHarness(t)
	fk, _ := h.fenceVIPs()
	ipamKV := h.openVIPs()

	slot := fk.arm("Create", false, isSlotKey)
	free := sync.OnceFunc(slot.free)
	defer free()
	created := fenceAsync(func() error {
		_, err := h.svc.CreateLoadBalancer(h.t.Context(), h.input("ovn-nlb", h.subnetID, h.groups[0]), testAccountID)
		return err
	})
	slot.wait(t)
	_, pending, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok, "admission holds a pending claim")
	require.False(t, pending.Sealed)
	arn := pending.Root.LoadBalancerArn

	require.NoError(t, h.deleteSubnet(), "the delete commits before any slot exists")
	entry, _ := h.subnetRecord()
	require.Nil(t, entry)

	free()
	select {
	case err := <-created:
		requireFenceCode(t, err, awserrors.ErrorELBv2SubnetNotFound)
	case <-time.After(fenceHoldWait):
		t.Fatal("the resumed Create did not return")
	}
	sealed, err := h.svc.ovn.owners.GetSealedOwner(t.Context(), "ovn-nlb", testAccountID)
	require.NoError(t, err)
	assert.Nil(t, sealed, "no sealed owner")
	_, _, claimed := h.rawClaim("ovn-nlb")
	assert.False(t, claimed, "the abandoned claim is gone")
	assert.Empty(t, h.rawSlotOwner(vpcSlotKey(testAccountID, h.vpcID)), "no VPC slot")
	assert.Empty(t, h.rawSlotOwner(subnetSlotKey(testAccountID, h.subnetID)), "no subnet slot")

	pass, trigger := h.vipLoop(arn)
	got := h.runPass(trigger, pass)
	require.ErrorIs(t, got.err, errOVNVIPRefused)
	assert.Empty(t, got.ip)
	assert.Zero(t, ipamKV.writeCount(), "no allocator write")
	assert.True(t, h.ipamRowAbsent())
}
