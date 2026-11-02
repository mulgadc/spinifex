package systemvpc

import (
	"maps"
	"net/netip"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The private shape is a VPC and one subnet carved from the spec's supernet,
// owned and role-tagged like Ensure's, with no gateway, route table or EIP.
func TestEnsurePrivateBuildsOnlyAVPCAndOneSubnet(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("sqs-demo")

	refs, err := EnsurePrivate(t.Context(), f, spec, "000000000000")
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"vpc": 1, "subnet": 1}, f.creates)
	assert.Empty(t, f.igws)
	assert.Empty(t, f.rts)
	assert.Empty(t, f.ngws)
	assert.Empty(t, f.eips)

	vpcCIDR, _, privateCIDRs, err := spec.cidrs()
	require.NoError(t, err)
	assert.Equal(t, vpcCIDR, refs.VpcCIDR)
	assert.Equal(t, privateCIDRs[0], refs.SubnetCIDR)
	sub := f.subnets[refs.SubnetID]
	require.NotNil(t, sub)
	assert.Equal(t, refs.VpcID, aws.StringValue(sub.VpcId))
	assert.Equal(t, refs.SubnetCIDR, aws.StringValue(sub.CidrBlock))
	assert.Equal(t, spec.Roles().PrivateSubnet, tagValue(sub.Tags, spec.RoleTagKey))
	assert.Equal(t, spec.Name, tagValue(sub.Tags, spec.OwnerTagKey))
	assert.Equal(t, spec.Roles().VPC, tagValue(f.vpcs[refs.VpcID].Tags, spec.RoleTagKey))

	super := netip.MustParsePrefix(spec.Supernet)
	assert.True(t, super.Contains(netip.MustParsePrefix(refs.SubnetCIDR).Addr()), "subnet outside the supernet")
}

func TestEnsurePrivateIsIdempotent(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("sqs-demo")
	first, err := EnsurePrivate(t.Context(), f, spec, "000000000000")
	require.NoError(t, err)
	creates := maps.Clone(f.creates)

	second, err := EnsurePrivate(t.Context(), f, spec, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, creates, f.creates, "a re-run must create nothing")
}

// A VPC left by a run that failed before its subnet is reused, not duplicated.
func TestEnsurePrivateConvergesAfterPartialFailure(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("sqs-demo")
	vpcCIDR, _, _, err := spec.cidrs()
	require.NoError(t, err)
	vpcID, err := ensureVPC(t.Context(), f, spec, "000000000000", vpcCIDR)
	require.NoError(t, err)

	refs, err := EnsurePrivate(t.Context(), f, spec, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, vpcID, refs.VpcID)
	assert.Equal(t, 1, f.creates["vpc"])
}

func TestEnsurePrivateRejectsAnUnusableSpec(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("sqs-demo")
	spec.Supernet = "10.0.0.0/16"
	_, err := EnsurePrivate(t.Context(), f, spec, "000000000000")
	require.Error(t, err)
	assert.Empty(t, f.creates)
}
