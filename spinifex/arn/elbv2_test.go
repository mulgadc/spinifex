package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
)

func TestFormatELBv2LoadBalancer(t *testing.T) {
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/50dc6c495c0c9188",
		arn.FormatELBv2LoadBalancer("us-east-1", "123456789012", "my-alb", "50dc6c495c0c9188", "application"))
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/my-nlb/50dc6c495c0c9188",
		arn.FormatELBv2LoadBalancer("us-east-1", "123456789012", "my-nlb", "50dc6c495c0c9188", "network"))
}

// An unrecognised type takes the application segment, which is what the handler
// does: only "network" is spelled "net".
func TestELBv2LBPathSegment(t *testing.T) {
	assert.Equal(t, "net", arn.ELBv2LBPathSegment("network"))
	assert.Equal(t, "app", arn.ELBv2LBPathSegment("application"))
	assert.Equal(t, "app", arn.ELBv2LBPathSegment(""))
}

func TestFormatELBv2TargetGroup(t *testing.T) {
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:us-west-2:111222333444:targetgroup/my-tg/deadbeef",
		arn.FormatELBv2TargetGroup("us-west-2", "111222333444", "my-tg", "deadbeef"))
}

func TestFormatELBv2Listener(t *testing.T) {
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/app/my-alb/lbid123/listener456",
		arn.FormatELBv2Listener("eu-west-1", "999888777666", "my-alb", "lbid123", "listener456", "application"))
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/net/my-nlb/lbid123/listener456",
		arn.FormatELBv2Listener("eu-west-1", "999888777666", "my-nlb", "lbid123", "listener456", "network"))
}

func TestFormatELBv2Resource(t *testing.T) {
	assert.Equal(t,
		"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener-rule/app/alb/lb1/l1/r1",
		arn.FormatELBv2Resource("ap-southeast-2", "123456789012", arn.ELBv2ListenerRule, "app/alb/lb1/l1/r1"))
}

func TestParseELBv2(t *testing.T) {
	parsed, ok := arn.ParseELBv2("arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/app/prod/abc123")
	assert.True(t, ok)
	assert.Equal(t, "ap-southeast-2", parsed.Region)
	assert.Equal(t, "123456789012", parsed.AccountID)
	assert.Equal(t, arn.ELBv2LoadBalancer, parsed.Kind)
	assert.Equal(t, "app/prod/abc123", parsed.Resource)
}

// Round-tripping every builder through the parser is what stops the gate and
// the handler drifting onto different ARN formats.
func TestParseELBv2_RoundTripsEveryBuilder(t *testing.T) {
	region, account := "ap-southeast-2", "123456789012"
	cases := []struct {
		value string
		kind  arn.ELBv2ResourceType
	}{
		{arn.FormatELBv2LoadBalancer(region, account, "prod", "abc123", "application"), arn.ELBv2LoadBalancer},
		{arn.FormatELBv2TargetGroup(region, account, "web", "def456"), arn.ELBv2TargetGroup},
		{arn.FormatELBv2Listener(region, account, "prod", "abc123", "l1", "network"), arn.ELBv2Listener},
		{arn.FormatELBv2Resource(region, account, arn.ELBv2ListenerRule, "app/prod/abc123/l1/r1"), arn.ELBv2ListenerRule},
	}
	for _, tc := range cases {
		parsed, ok := arn.ParseELBv2(tc.value)
		assert.True(t, ok, tc.value)
		assert.Equal(t, tc.kind, parsed.Kind, tc.value)
		assert.Equal(t, region, parsed.Region, tc.value)
		assert.Equal(t, account, parsed.AccountID, tc.value)
	}
}

func TestParseELBv2_Rejects(t *testing.T) {
	rejected := []string{
		"",
		"not-an-arn",
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/i-abc",
		"arn:aws-cn:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/app/prod/abc",
		"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:widget/app/prod/abc",
		"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer",
		"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012",
	}
	for _, value := range rejected {
		_, ok := arn.ParseELBv2(value)
		assert.False(t, ok, value)
	}
}
