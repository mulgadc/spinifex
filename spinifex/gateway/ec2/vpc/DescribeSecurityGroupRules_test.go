package gateway_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestDescribeSecurityGroupRules_NilInput_PassesValidation(t *testing.T) {
	// Nil input means "all rules in account". The call dives into the NATS
	// service (nil conn) and will fail there, but the gateway-side validation
	// must accept nil — that's what this assertion proves: the error is not
	// the gateway's InvalidParameterValue.
	_, err := DescribeSecurityGroupRules(nil, nil, testAccountID)
	assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, err.Error())
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
		_, err := DescribeSecurityGroupRules(input, nil, testAccountID)
		assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue, "expected InvalidParameterValue for %q", bad)
	}
}

func TestDescribeSecurityGroupRules_NilRuleIDEntry(t *testing.T) {
	input := &ec2.DescribeSecurityGroupRulesInput{
		SecurityGroupRuleIds: []*string{nil},
	}
	_, err := DescribeSecurityGroupRules(input, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeSecurityGroupRules_ValidRuleIDPassesValidation(t *testing.T) {
	// A well-formed sgr- ID passes gateway validation; the call then dives
	// into the NATS service (nil conn) and fails there. The assertion is
	// that the error is NOT the gateway's InvalidParameterValue.
	input := &ec2.DescribeSecurityGroupRulesInput{
		SecurityGroupRuleIds: []*string{aws.String("sgr-0123456789abcdef0")},
	}
	_, err := DescribeSecurityGroupRules(input, nil, testAccountID)
	assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
