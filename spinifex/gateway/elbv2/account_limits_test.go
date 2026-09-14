package gateway_elbv2_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// limitsByName indexes one response so a case can assert on the entry it cares
// about without depending on the order the table is written in.
func limitsByName(t *testing.T, out elbv2.DescribeAccountLimitsOutput) map[string]*elbv2.Limit {
	t.Helper()
	byName := make(map[string]*elbv2.Limit, len(out.Limits))
	for _, limit := range out.Limits {
		require.NotNil(t, limit.Name)
		byName[*limit.Name] = limit
	}
	require.Len(t, byName, len(out.Limits), "a limit name must not be reported twice")
	return byName
}

// A caller that reads limits before provisioning must find every name AWS
// returns, because a missing entry sends it back to its own assumption.
func TestDescribeAccountLimitsReportsEveryName(t *testing.T) {
	out := gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 50)
	byName := limitsByName(t, out)

	for _, name := range []string{
		"application-load-balancers",
		"network-load-balancers",
		"target-groups",
		"target-groups-per-application-load-balancer",
		"target-groups-per-action-on-application-load-balancer",
		"target-groups-per-action-on-network-load-balancer",
		"listeners-per-application-load-balancer",
		"listeners-per-network-load-balancer",
		"rules-per-application-load-balancer",
		"condition-values-per-alb-rule",
		"condition-wildcards-per-alb-rule",
		"targets-per-application-load-balancer",
		"targets-per-network-load-balancer",
		"targets-per-availability-zone-per-network-load-balancer",
		"certificates-per-application-load-balancer",
		"certificates-per-network-load-balancer",
	} {
		limit, ok := byName[name]
		require.True(t, ok, "limit %q is missing from the response", name)
		require.NotNil(t, limit.Max, "limit %q reports no maximum", name)
		assert.NotEmpty(t, *limit.Max)
	}
}

// The whole point of sourcing from the quota service: the reported cap is the
// one CreateLoadBalancer enforces, for both load balancer types.
func TestDescribeAccountLimitsReportsTheEnforcedCap(t *testing.T) {
	byName := limitsByName(t, gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 7))

	assert.Equal(t, "7", aws.StringValue(byName["application-load-balancers"].Max))
	assert.Equal(t, "7", aws.StringValue(byName["network-load-balancers"].Max))

	// A limit with no counterpart in the quota service keeps its AWS default.
	assert.Equal(t, "3000", aws.StringValue(byName["target-groups"].Max))
}

// An uncapped account reports no maximum rather than a number that would cap a
// caller which has no cap.
func TestDescribeAccountLimitsOmitsMaxWhenUnlimited(t *testing.T) {
	byName := limitsByName(t, gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, -1))

	assert.Nil(t, byName["application-load-balancers"].Max)
	assert.Nil(t, byName["network-load-balancers"].Max)
	assert.Equal(t, "3000", aws.StringValue(byName["target-groups"].Max))
}

// The list is short and fixed, so the paging parameters are accepted and the
// whole list comes back on one page with nothing to follow.
func TestDescribeAccountLimitsReturnsOnePage(t *testing.T) {
	out := gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{
		Marker:   aws.String("2"),
		PageSize: aws.Int64(1),
	}, 50)

	assert.Nil(t, out.NextMarker)
	assert.Len(t, out.Limits, len(gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 50).Limits))
}

// A returned pointer must not be shared across calls, or one caller's response
// could be mutated by another's.
func TestDescribeAccountLimitsDoesNotShareEntries(t *testing.T) {
	first := gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 50)
	second := gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 50)

	require.NotEmpty(t, first.Limits)
	for i := range first.Limits {
		assert.NotSame(t, first.Limits[i], second.Limits[i])
	}
}
