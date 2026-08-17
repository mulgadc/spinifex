package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default-placement path resolves the VPC through the same EC2 surface a
// customer calls, so every filter name it sends has to be one that surface
// accepts. The camelCase 'isDefault' spelling failed this: DescribeVpcs answered
// InvalidParameterValue and every create without an explicit subnet group was
// refused before it reached the launch path.
func TestDefaultVPCID_SendsFilterNamesTheEC2SurfaceAccepts(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	require.NotEmpty(t, h.network.vpcFilters, "the default-placement path must filter the describe")
	for _, filter := range h.network.vpcFilters {
		name := aws.StringValue(filter.Name)
		assert.True(t, handlers_ec2_vpc.SupportsDescribeVpcsFilter(name),
			"DescribeVpcs rejects the filter %q; use the name the EC2 surface accepts", name)
	}
}

// An explicit subnet group carries its own VPC, so the default lookup — and the
// filter that goes with it — must not be issued at all.
func TestResolvePlacement_SkipsTheDefaultLookupForAnExplicitSubnetGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
	require.NoError(t, err)
	h.network.vpcFilters = nil

	in := validCreateInput()
	in.DBSubnetGroupName = aws.String(testSubnetGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.network.vpcFilters,
		"a named subnet group already resolves the VPC; no default lookup should be issued")
}
