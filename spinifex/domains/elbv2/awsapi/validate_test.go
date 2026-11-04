package awsapi

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidators_ModelConstraints breaks one model constraint per case and
// expects ValidationError naming the member; an empty member means in range.
func TestValidators_ModelConstraints(t *testing.T) {
	tgArn := aws.String("arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/tg/1")
	ctg := func(mut func(*elbv2.CreateTargetGroupInput)) func() error {
		in := &elbv2.CreateTargetGroupInput{Name: aws.String("tg")}
		mut(in)
		return func() error { return ValidateCreateTargetGroupInput(in) }
	}
	mtg := func(mut func(*elbv2.ModifyTargetGroupInput)) func() error {
		in := &elbv2.ModifyTargetGroupInput{TargetGroupArn: tgArn}
		mut(in)
		return func() error { return ValidateModifyTargetGroupInput(in) }
	}
	clb := func(mut func(*elbv2.CreateLoadBalancerInput)) func() error {
		in := &elbv2.CreateLoadBalancerInput{Name: aws.String("lb")}
		mut(in)
		return func() error { return ValidateCreateLoadBalancerInput(in) }
	}
	ssl := func(lbType string) func() error {
		return func() error {
			return ValidateDescribeSSLPoliciesInput(&elbv2.DescribeSSLPoliciesInput{LoadBalancerType: aws.String(lbType)})
		}
	}
	mla := func(attrs []ListenerAttribute) func() error {
		return func() error {
			return ValidateModifyListenerAttributesInput(&ModifyListenerAttributesInput{
				ListenerArn: aws.String("arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener/app/lb/1/2"),
				Attributes:  attrs,
			})
		}
	}

	cases := []struct {
		name   string
		call   func() error
		member string
	}{
		{"ctg interval below", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthCheckIntervalSeconds = aws.Int64(4) }), "healthCheckIntervalSeconds"},
		{"ctg interval above", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthCheckIntervalSeconds = aws.Int64(301) }), "healthCheckIntervalSeconds"},
		{"ctg timeout below", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthCheckTimeoutSeconds = aws.Int64(1) }), "healthCheckTimeoutSeconds"},
		{"ctg timeout above", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthCheckTimeoutSeconds = aws.Int64(121) }), "healthCheckTimeoutSeconds"},
		{"ctg healthy below", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthyThresholdCount = aws.Int64(1) }), "healthyThresholdCount"},
		{"ctg healthy above", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthyThresholdCount = aws.Int64(11) }), "healthyThresholdCount"},
		{"ctg unhealthy below", ctg(func(i *elbv2.CreateTargetGroupInput) { i.UnhealthyThresholdCount = aws.Int64(1) }), "unhealthyThresholdCount"},
		{"ctg unhealthy above", ctg(func(i *elbv2.CreateTargetGroupInput) { i.UnhealthyThresholdCount = aws.Int64(11) }), "unhealthyThresholdCount"},
		{"ctg protocol", ctg(func(i *elbv2.CreateTargetGroupInput) { i.HealthCheckProtocol = aws.String("ICMP") }), "healthCheckProtocol"},
		{"ctg in range", ctg(func(i *elbv2.CreateTargetGroupInput) {
			i.HealthCheckProtocol = aws.String("TCP_QUIC")
			i.HealthCheckIntervalSeconds = aws.Int64(300)
			i.HealthCheckTimeoutSeconds = aws.Int64(2)
			i.HealthyThresholdCount = aws.Int64(10)
			i.UnhealthyThresholdCount = aws.Int64(2)
		}), ""},
		{"mtg interval below", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthCheckIntervalSeconds = aws.Int64(4) }), "healthCheckIntervalSeconds"},
		{"mtg interval above", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthCheckIntervalSeconds = aws.Int64(301) }), "healthCheckIntervalSeconds"},
		{"mtg timeout below", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthCheckTimeoutSeconds = aws.Int64(1) }), "healthCheckTimeoutSeconds"},
		{"mtg timeout above", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthCheckTimeoutSeconds = aws.Int64(121) }), "healthCheckTimeoutSeconds"},
		{"mtg healthy below", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthyThresholdCount = aws.Int64(1) }), "healthyThresholdCount"},
		{"mtg healthy above", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthyThresholdCount = aws.Int64(11) }), "healthyThresholdCount"},
		{"mtg unhealthy below", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.UnhealthyThresholdCount = aws.Int64(1) }), "unhealthyThresholdCount"},
		{"mtg unhealthy above", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.UnhealthyThresholdCount = aws.Int64(11) }), "unhealthyThresholdCount"},
		{"mtg protocol", mtg(func(i *elbv2.ModifyTargetGroupInput) { i.HealthCheckProtocol = aws.String("http") }), "healthCheckProtocol"},
		{"mtg in range", mtg(func(i *elbv2.ModifyTargetGroupInput) {
			i.HealthCheckProtocol = aws.String("HTTP")
			i.HealthCheckIntervalSeconds = aws.Int64(5)
			i.HealthCheckTimeoutSeconds = aws.Int64(120)
			i.HealthyThresholdCount = aws.Int64(2)
			i.UnhealthyThresholdCount = aws.Int64(10)
		}), ""},
		{"clb ip address type", clb(func(i *elbv2.CreateLoadBalancerInput) { i.IpAddressType = aws.String("ipv6") }), "ipAddressType"},
		{"clb pool too long", clb(func(i *elbv2.CreateLoadBalancerInput) {
			i.CustomerOwnedIpv4Pool = aws.String("ipv4pool-coip-" + strings.Repeat("a", 243))
		}), "customerOwnedIpv4Pool"},
		{"clb pool pattern", clb(func(i *elbv2.CreateLoadBalancerInput) { i.CustomerOwnedIpv4Pool = aws.String("pool-1") }), "customerOwnedIpv4Pool"},
		{"clb in range", clb(func(i *elbv2.CreateLoadBalancerInput) {
			i.IpAddressType = aws.String("dualstack-without-public-ipv4")
			i.CustomerOwnedIpv4Pool = aws.String("ipv4pool-coip-" + strings.Repeat("a", 242))
		}), ""},
		{"ssl lb type", ssl("classic"), "loadBalancerType"},
		{"ssl in range", ssl("gateway"), ""},
		{"mla attributes omitted", mla(nil), "attributes"},
		{"mla in range", mla([]ListenerAttribute{{Key: aws.String("tcp.idle_timeout.seconds"), Value: aws.String("350")}}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if tc.member == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationError, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), "at '"+tc.member+"'")
		})
	}
}
