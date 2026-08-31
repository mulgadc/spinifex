package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reaperTestAccountID = "123456789012"

// TestENIOrphanReaperScopeIsClusterWide pins the scope. The record it reaps has
// no instance, so no node owns it; running the sweep node-locally would have
// every node racing to delete the same records.
func TestENIOrphanReaperScopeIsClusterWide(t *testing.T) {
	r := &eniOrphanReaper{}
	assert.Equal(t, vm.ScopeClusterWide, r.Scope())
	assert.Equal(t, "eni-orphan", r.Class())
}

func TestENIOrphanReaperSweep(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)

	ctx := context.Background()
	vpcOut, err := vpcSvc.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, reaperTestAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, reaperTestAccountID)
	require.NoError(t, err)

	eniOut, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    subnetOut.Subnet.SubnetId,
		Description: aws.String(handlers_ec2_vpc.AutoENIDescriptionPrefix + "i-abandoned"),
	}, reaperTestAccountID)
	require.NoError(t, err)
	eniID := *eniOut.NetworkInterface.NetworkInterfaceId
	require.NoError(t, vpcSvc.UpdateENI(reaperTestAccountID, eniID, func(rec *handlers_ec2_vpc.ENIRecord) {
		rec.CreatedAt = time.Now().Add(-time.Hour)
	}))

	r := &eniOrphanReaper{vpc: vpcSvc, minAge: 15 * time.Minute}

	reaped, err := r.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
	require.Error(t, err, "the record must be gone, or it keeps blocking its security group")

	// A second pass has nothing left and must not report work or fail.
	reaped, err = r.Sweep(ctx)
	require.NoError(t, err)
	assert.Zero(t, reaped)
}

// fakeInstanceIndex answers the instance-existence question from fixed sets.
type fakeInstanceIndex struct {
	live       []string
	terminated []string
	liveErr    error
	termErr    error
}

func instanceRecords(ids []string) []*vm.InstanceRecord {
	out := make([]*vm.InstanceRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, &vm.InstanceRecord{Metadata: resource.Metadata{Name: id}})
	}
	return out
}

func (f *fakeInstanceIndex) ListInstanceRecords() ([]*vm.InstanceRecord, error) {
	return instanceRecords(f.live), f.liveErr
}

func (f *fakeInstanceIndex) ListTerminatedInstanceRecords() ([]*vm.InstanceRecord, error) {
	return instanceRecords(f.terminated), f.termErr
}

// fakeEIPDisassociator records which ENIs were asked to give up their EIP.
type fakeEIPDisassociator struct {
	associated map[string]bool
	calls      []string
	err        error
}

func (f *fakeEIPDisassociator) DisassociateByENI(_ context.Context, _, eniID string) (bool, error) {
	f.calls = append(f.calls, eniID)
	if f.err != nil {
		return false, f.err
	}
	return f.associated[eniID], nil
}

// staleReaperFixture builds a VPC with one ENI attached to instanceID, aged past
// the sweep's guard, plus the reaper under test.
func staleReaperFixture(t *testing.T, instanceID string, index instanceIndex, eip eipDisassociator) (*eniOrphanReaper, *handlers_ec2_vpc.VPCServiceImpl, string) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)

	ctx := t.Context()
	vpcOut, err := vpcSvc.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, reaperTestAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, reaperTestAccountID)
	require.NoError(t, err)

	eniOut, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    subnetOut.Subnet.SubnetId,
		Description: aws.String(handlers_ec2_vpc.AutoENIDescriptionPrefix + instanceID),
	}, reaperTestAccountID)
	require.NoError(t, err)
	eniID := *eniOut.NetworkInterface.NetworkInterfaceId

	_, err = vpcSvc.AttachENI(reaperTestAccountID, eniID, instanceID, 0)
	require.NoError(t, err)
	require.NoError(t, vpcSvc.UpdateENI(reaperTestAccountID, eniID, func(rec *handlers_ec2_vpc.ENIRecord) {
		rec.CreatedAt = time.Now().Add(-time.Hour)
	}))

	return &eniOrphanReaper{vpc: vpcSvc, instances: index, eip: eip, minAge: 15 * time.Minute}, vpcSvc, eniID
}

// TestENIOrphanReaperReapsStaleAttachment is the zombie case: the ENI still
// names an instance, but that instance is in neither the live nor the terminated
// space, so nothing else will ever delete the record — and until it goes, its
// logical switch port stays unbindable and its EIP stays associated.
func TestENIOrphanReaperReapsStaleAttachment(t *testing.T) {
	index := &fakeInstanceIndex{live: []string{"i-somebody-else"}, terminated: []string{"i-recent"}}
	eip := &fakeEIPDisassociator{associated: map[string]bool{}}
	r, vpcSvc, eniID := staleReaperFixture(t, "i-gone", index, eip)
	eip.associated[eniID] = true

	reaped, err := r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
	require.Error(t, err, "the record must be gone, or the reconciler keeps driving toward a port that cannot bind")
	assert.Equal(t, []string{eniID}, eip.calls, "the EIP association must be released with the interface")

	reaped, err = r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)
}

// TestENIOrphanReaperSparesKnownInstances pins the guard that matters most: an
// ENI whose instance is running, stopped, or recently terminated is in use, and
// deleting it would cut a live guest off the network.
func TestENIOrphanReaperSparesKnownInstances(t *testing.T) {
	for name, index := range map[string]*fakeInstanceIndex{
		"running or stopped": {live: []string{"i-known"}},
		"terminated":         {terminated: []string{"i-known"}},
	} {
		t.Run(name, func(t *testing.T) {
			eip := &fakeEIPDisassociator{associated: map[string]bool{}}
			r, vpcSvc, eniID := staleReaperFixture(t, "i-known", index, eip)

			reaped, err := r.Sweep(t.Context())
			require.NoError(t, err)
			assert.Zero(t, reaped)

			_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
			assert.NoError(t, err)
			assert.Empty(t, eip.calls)
		})
	}
}

// TestENIOrphanReaperSkipsStaleSweepOnListingError pins the fail-closed rule. A
// listing that errors part-way reads as "the instance is gone" for everything
// missing from it, so the sweep must decline rather than reap against it.
func TestENIOrphanReaperSkipsStaleSweepOnListingError(t *testing.T) {
	for name, index := range map[string]*fakeInstanceIndex{
		"live listing failed":       {liveErr: errors.New("kv unavailable")},
		"terminated listing failed": {live: []string{"i-other"}, termErr: errors.New("kv unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			eip := &fakeEIPDisassociator{associated: map[string]bool{}}
			r, vpcSvc, eniID := staleReaperFixture(t, "i-gone", index, eip)

			reaped, err := r.Sweep(t.Context())
			require.NoError(t, err, "a listing fault is not the sweep's failure; it just cannot decide")
			assert.Zero(t, reaped)

			_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
			assert.NoError(t, err)
		})
	}
}

// TestENIOrphanReaperWithoutInstanceIndexSkipsStaleSweep pins that a node with
// no instance-record source (no JetStream manager) still runs the
// abandoned-launch sweep and simply declines the staleness question.
func TestENIOrphanReaperWithoutInstanceIndexSkipsStaleSweep(t *testing.T) {
	r, vpcSvc, eniID := staleReaperFixture(t, "i-gone", nil, nil)

	reaped, err := r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)

	_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
	assert.NoError(t, err)
}

// TestENIOrphanReaperDeletesENIDespiteEIPFailure pins that a stuck EIP
// disassociation cannot pin the ENI. Leaving the interface behind is the worse
// outcome: the association is retried on the next sweep, the zombie port is not.
func TestENIOrphanReaperDeletesENIDespiteEIPFailure(t *testing.T) {
	index := &fakeInstanceIndex{}
	eip := &fakeEIPDisassociator{err: errors.New("kv unavailable")}
	r, vpcSvc, eniID := staleReaperFixture(t, "i-gone", index, eip)

	reaped, err := r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
	assert.Error(t, err)
}
