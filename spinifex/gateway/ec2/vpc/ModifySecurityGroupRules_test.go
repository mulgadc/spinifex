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

func modifyUpdate(ruleID string) *ec2.SecurityGroupRuleUpdate {
	return &ec2.SecurityGroupRuleUpdate{
		SecurityGroupRuleId: aws.String(ruleID),
		SecurityGroupRule: &ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			CidrIpv4:   aws.String("10.0.0.0/24"),
		},
	}
}

func TestModifySecurityGroupRules_NilInput(t *testing.T) {
	_, err := gateway_ec2_vpc.ModifySecurityGroupRules(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifySecurityGroupRules_MissingGroupId(t *testing.T) {
	_, err := gateway_ec2_vpc.ModifySecurityGroupRules(context.Background(), &ec2.ModifySecurityGroupRulesInput{
		SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{modifyUpdate("sgr-0123456789abcdef0")},
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifySecurityGroupRules_EmptyUpdateList(t *testing.T) {
	_, err := gateway_ec2_vpc.ModifySecurityGroupRules(context.Background(), &ec2.ModifySecurityGroupRulesInput{
		GroupId: aws.String("sg-0123456789abcdef0"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifySecurityGroupRules_MalformedRuleID(t *testing.T) {
	for _, bad := range []string{"foo", "SGR-0123456789abcdef0", "sgr-0123456789abcdef01", "sg-0123456789abcdef0", ""} {
		_, err := gateway_ec2_vpc.ModifySecurityGroupRules(context.Background(), &ec2.ModifySecurityGroupRulesInput{
			GroupId:            aws.String("sg-0123456789abcdef0"),
			SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{modifyUpdate(bad)},
		}, nil, testAccountID)
		assert.EqualError(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, "expected malformed for %q", bad)
	}
}

// A short or non-hex suffix is NotFound on AWS, so it must reach the handler
// rather than be rejected here as malformed.
func TestModifySecurityGroupRules_WellFormedRuleIDPassesValidation(t *testing.T) {
	for _, id := range []string{"sgr-0123456789abcdef0", "sgr-xyz", "sgr-"} {
		_, err := gateway_ec2_vpc.ModifySecurityGroupRules(context.Background(), &ec2.ModifySecurityGroupRulesInput{
			GroupId:            aws.String("sg-0123456789abcdef0"),
			SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{modifyUpdate(id)},
		}, nil, testAccountID)
		require.Error(t, err)
		assert.NotEqual(t, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, err.Error(), "id %q", id)
	}
}
