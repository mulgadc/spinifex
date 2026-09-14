package gateway_ec2_vpc_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func TestUpdateSecurityGroupRuleDescriptionsIngress_NilInput(t *testing.T) {
	_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestUpdateSecurityGroupRuleDescriptionsEgress_NilInput(t *testing.T) {
	_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestUpdateSecurityGroupRuleDescriptions_MissingGroupId(t *testing.T) {
	_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(),
		&ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(),
		&ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{GroupId: aws.String("")}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestUpdateSecurityGroupRuleDescriptions_MalformedRuleID(t *testing.T) {
	for _, bad := range []string{"sgr-toolong0123456789abcdef", "sgr-XYZ", "sg-0123456789abcdef0", ""} {
		_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
			GroupId: aws.String("sg-0123456789abcdef0"),
			SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
				{SecurityGroupRuleId: aws.String(bad), Description: aws.String("x")},
			},
		}, nil, testAccountID)
		assert.EqualError(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, "expected malformed for %q", bad)
	}

	_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId:                       aws.String("sg-0123456789abcdef0"),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{nil},
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
}

// A well-formed request clears gateway validation and fails downstream on the
// nil NATS connection instead.
func TestUpdateSecurityGroupRuleDescriptions_ValidRequestPassesValidation(t *testing.T) {
	_, err := gateway_ec2_vpc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String("sg-0123456789abcdef0"),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String("sgr-0123456789abcdef0"), Description: aws.String("x")},
		},
	}, nil, testAccountID)
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, err.Error())
}
