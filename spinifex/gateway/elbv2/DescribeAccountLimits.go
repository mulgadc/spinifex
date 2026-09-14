package gateway_elbv2

import (
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

// DefaultLoadBalancerLimit is what the load balancer entries report when no
// quota is being enforced. Zero would read as "no load balancers permitted",
// which is the opposite of what a disabled quota means.
const DefaultLoadBalancerLimit = 50

// staticLimits are the limits Spinifex advertises at their AWS defaults but
// does not police. They are reported anyway, because a caller that sees no
// entry falls back to its own assumption rather than to no limit at all.
var staticLimits = []struct {
	name    string
	maximum int
}{
	{"target-groups", 3000},
	{"target-groups-per-application-load-balancer", 100},
	{"target-groups-per-action-on-application-load-balancer", 5},
	{"target-groups-per-action-on-network-load-balancer", 1},
	{"listeners-per-application-load-balancer", 50},
	{"listeners-per-network-load-balancer", 50},
	{"rules-per-application-load-balancer", 100},
	{"condition-values-per-alb-rule", 5},
	{"condition-wildcards-per-alb-rule", 5},
	{"targets-per-application-load-balancer", 1000},
	{"targets-per-network-load-balancer", 3000},
	{"targets-per-availability-zone-per-network-load-balancer", 500},
	{"certificates-per-application-load-balancer", 25},
	{"certificates-per-network-load-balancer", 25},
}

// DescribeAccountLimits reports the ELBv2 limits in force for one account.
// loadBalancerLimit is resolved from the account quota service by the dispatch
// layer, which is where that dependency lives without an import cycle.
func DescribeAccountLimits(input *elbv2.DescribeAccountLimitsInput, loadBalancerLimit int) elbv2.DescribeAccountLimitsOutput {
	limits := make([]*elbv2.Limit, 0, len(staticLimits)+2)

	// ALBs and NLBs draw on one enforced pool, so both report the same number
	// rather than a split CreateLoadBalancer would not honour.
	limits = append(limits,
		accountLimit("application-load-balancers", loadBalancerLimit),
		accountLimit("network-load-balancers", loadBalancerLimit),
	)
	for _, limit := range staticLimits {
		limits = append(limits, accountLimit(limit.name, limit.maximum))
	}

	// The list is short and fixed, so Marker and PageSize are accepted and the
	// whole list is returned on one page, with no NextMarker to follow.
	return elbv2.DescribeAccountLimitsOutput{Limits: limits}
}

// accountLimit renders one entry. A negative maximum is the quota service's
// "unlimited", and Max is optional in the model, so no maximum is reported
// rather than a number that would cap a caller which has no cap.
func accountLimit(name string, maximum int) *elbv2.Limit {
	limit := &elbv2.Limit{Name: aws.String(name)}
	if maximum >= 0 {
		limit.Max = aws.String(strconv.Itoa(maximum))
	}
	return limit
}
