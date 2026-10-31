package gateway

import (
	"encoding/xml"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

// The service must accept the paging parameters the model defines and still
// answer with the whole list.
func TestELBv2DescribeAccountLimitsAcceptsPaging(t *testing.T) {
	gw, account := quotaTestGateway(t)

	body, err := serveAccountLimits(t, gw, account, "Action=DescribeAccountLimits&Marker=2&PageSize=1")
	require.NoError(t, err)

	var decoded accountLimitsResponse
	require.NoError(t, xml.Unmarshal(body, &decoded))
	assert.Len(t, decoded.Limits, len(elbv2awsapi.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 2).Limits))
	assert.NotContains(t, string(body), "NextMarker")
}

// A disabled quota means "not enforced", not "nothing permitted", so the entries
// report the AWS default rather than the unconsulted configured value.
func TestELBv2DescribeAccountLimitsDefaultsWhenQuotaDisabled(t *testing.T) {
	disabled := quota.New(quota.Limits{LoadBalancers: 0}, nil)

	for name, gw := range map[string]*GatewayConfig{
		"disabled": withFakeCapabilities(withQuota(&GatewayConfig{DisableLogging: true}, disabled)),
		"unwired":  withFakeCapabilities(&GatewayConfig{DisableLogging: true}),
	} {
		t.Run(name, func(t *testing.T) {
			byName := describeAccountLimits(t, gw, "000000000042")
			assert.Equal(t, "50", byName["application-load-balancers"])
			assert.Equal(t, "50", byName["network-load-balancers"])
		})
	}
}

// The limit must be resolved per request, so one account's override cannot be
// served to another.
func TestELBv2DescribeAccountLimitsIsPerAccount(t *testing.T) {
	gw, account := quotaTestGateway(t)
	const other = "000000000043"

	require.NoError(t, quotaFor(gw).PutAccountQuota(t.Context(), account,
		quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))
	require.NoError(t, quotaFor(gw).PutAccountQuota(t.Context(), other,
		quota.Overrides{LoadBalancers: quotaPtr(4)}, "operator"))

	assert.Equal(t, "9", describeAccountLimits(t, gw, account)["application-load-balancers"])
	assert.Equal(t, "4", describeAccountLimits(t, gw, other)["application-load-balancers"])
}

// The reported cap must be the one CreateLoadBalancer enforces, including an
// override an operator has stored since the gateway started.
func TestELBv2DescribeAccountLimitsReportsStoredQuota(t *testing.T) {
	gw, account := quotaTestGateway(t)

	byName := describeAccountLimits(t, gw, account)
	assert.Equal(t, "2", byName["application-load-balancers"])
	assert.Equal(t, "2", byName["network-load-balancers"])

	require.NoError(t, quotaFor(gw).PutAccountQuota(t.Context(), account,
		quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))

	byName = describeAccountLimits(t, gw, account)
	assert.Equal(t, "9", byName["application-load-balancers"])
	assert.Equal(t, "9", byName["network-load-balancers"])
}

// The limits Spinifex does not police are still advertised, so a caller does not
// fall back to an assumption of its own.
func TestELBv2DescribeAccountLimitsReportsUnenforcedLimits(t *testing.T) {
	gw, account := quotaTestGateway(t)

	byName := describeAccountLimits(t, gw, account)
	assert.Equal(t, "3000", byName["target-groups"])
	assert.Equal(t, "50", byName["listeners-per-application-load-balancer"])
	assert.Equal(t, "100", byName["rules-per-application-load-balancer"])
	assert.Equal(t, "1000", byName["targets-per-application-load-balancer"])
	assert.Equal(t, "25", byName["certificates-per-application-load-balancer"])
}

// A sealed override must refuse the load balancer cap with Blocked rather
// than the generic InternalError every other read failure maps to.
func TestELBv2DescribeAccountLimitsBlockedOnSealedRecord(t *testing.T) {
	gw, account := quotaTestGateway(t)
	quotaFor(gw).SetAccountTerminationChecker(&quotaStubChecker{terminating: map[string]bool{account: true}})
	_, err := quotaFor(gw).SealAccountOverrides(t.Context(), account)
	require.NoError(t, err)

	body, err := serveAccountLimits(t, gw, account, "Action=DescribeAccountLimits")
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorBlocked, err.Error())
	assert.Empty(t, body)
}
