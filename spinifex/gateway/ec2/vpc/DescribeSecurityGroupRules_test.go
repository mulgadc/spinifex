package gateway_ec2_vpc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeSecurityGroupRules_NilInput_PassesValidation(t *testing.T) {
	// Nil input means "all rules in account"; the gateway must not reject it.
	// The call dives into the NATS service (nil conn) and fails there, so the
	// assertion is that the error is not the gateway's malformed-ID error.
	_, err := DescribeSecurityGroupRules(context.Background(), nil, nil, testAccountID)
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, err.Error())
}

func TestDescribeSecurityGroupRules_MalformedRuleID(t *testing.T) {
	cases := []string{
		"sgr-toolong0123456789abcdef",
		"sgr-XYZ",
		"sg-0123456789abcdef0",
		"",
		"sgr-0123456789abcdefX",
	}
	for _, bad := range cases {
		input := &ec2.DescribeSecurityGroupRulesInput{
			SecurityGroupRuleIds: []*string{aws.String(bad)},
		}
		_, err := DescribeSecurityGroupRules(context.Background(), input, nil, testAccountID)
		assert.EqualError(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, "expected InvalidSecurityGroupRuleId.Malformed for %q", bad)
	}
}

func TestDescribeSecurityGroupRules_NilRuleIDEntry(t *testing.T) {
	input := &ec2.DescribeSecurityGroupRulesInput{
		SecurityGroupRuleIds: []*string{nil},
	}
	_, err := DescribeSecurityGroupRules(context.Background(), input, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
}

func TestDescribeSecurityGroupRules_ValidRuleIDPassesValidation(t *testing.T) {
	// A well-formed sgr- ID passes gateway validation; the call then dives
	// into the NATS service (nil conn) and fails there.
	input := &ec2.DescribeSecurityGroupRulesInput{
		SecurityGroupRuleIds: []*string{aws.String("sgr-0123456789abcdef0")},
	}
	_, err := DescribeSecurityGroupRules(context.Background(), input, nil, testAccountID)
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, err.Error())
}
