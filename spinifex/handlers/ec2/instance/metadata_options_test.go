package handlers_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The constant block reports the IMDSv2-only posture: required tokens, endpoint
// enabled, IPv6/tags metadata disabled, state applied. Only the hop limit moves.
func TestBuildMetadataOptions_ConstantBlock(t *testing.T) {
	opts := buildMetadataOptions(nil)
	require.NotNil(t, opts)
	assert.Equal(t, ec2.InstanceMetadataOptionsStateApplied, aws.StringValue(opts.State))
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens))
	assert.Equal(t, ec2.InstanceMetadataEndpointStateEnabled, aws.StringValue(opts.HttpEndpoint))
	assert.Equal(t, ec2.InstanceMetadataProtocolStateDisabled, aws.StringValue(opts.HttpProtocolIpv6))
	assert.Equal(t, ec2.InstanceMetadataTagsStateDisabled, aws.StringValue(opts.InstanceMetadataTags))
	assert.Equal(t, int64(1), aws.Int64Value(opts.HttpPutResponseHopLimit), "nil hop limit defaults to 1")
}

func TestBuildMetadataOptions_HopLimit(t *testing.T) {
	cases := map[string]struct {
		in   *int64
		want int64
	}{
		"in-range 2":   {aws.Int64(2), 2},
		"upper bound":  {aws.Int64(64), 64},
		"lower bound":  {aws.Int64(1), 1},
		"zero clamps":  {aws.Int64(0), 1},
		"over clamps":  {aws.Int64(65), 1},
		"nil defaults": {nil, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, aws.Int64Value(buildMetadataOptions(tc.in).HttpPutResponseHopLimit))
		})
	}
}

// Every instance launched after this change carries the constant block so
// DescribeInstances surfaces the IMDSv2-only posture without a projection change.
func TestRunInstance_MetadataOptionsSeeded(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}

	_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
	})
	require.NoError(t, err)
	require.NotNil(t, ec2Instance.MetadataOptions, "launch must stamp the metadata-options block")
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(ec2Instance.MetadataOptions.HttpTokens))
	assert.Equal(t, int64(1), aws.Int64Value(ec2Instance.MetadataOptions.HttpPutResponseHopLimit))
}

// A requested hop limit is reflected on the instance; the rest stay invariant.
func TestRunInstance_MetadataOptionsHopLimitFromRequest(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}

	_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
		ImageId:         aws.String("ami-0abcdef1234567890"),
		InstanceType:    aws.String("t3.micro"),
		MetadataOptions: &ec2.InstanceMetadataOptionsRequest{HttpPutResponseHopLimit: aws.Int64(2)},
	})
	require.NoError(t, err)
	require.NotNil(t, ec2Instance.MetadataOptions)
	assert.Equal(t, int64(2), aws.Int64Value(ec2Instance.MetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(ec2Instance.MetadataOptions.HttpTokens))
}
