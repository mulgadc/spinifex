//test:in-package — drives the unexported dispatch entry and the quota lookup it
// performs, neither of which is reachable from outside the gateway package.

package gateway

import (
	"encoding/xml"
	"testing"

	"github.com/aws/aws-sdk-go/service/elbv2"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountLimitsResponse is the wire shape a caller parses, so the test asserts
// on what leaves the gateway rather than on the struct behind it.
type accountLimitsResponse struct {
	XMLName xml.Name `xml:"DescribeAccountLimitsResponse"`
	Limits  []struct {
		Name string `xml:"Name"`
		Max  string `xml:"Max"`
	} `xml:"DescribeAccountLimitsResult>Limits>member"`
}

// describeAccountLimits drives the registered dispatch entry and decodes the
// XML it writes, keyed by limit name.
func describeAccountLimits(t *testing.T, gw *GatewayConfig, accountID string) map[string]string {
	t.Helper()

	action, ok := elbv2Actions["DescribeAccountLimits"]
	require.True(t, ok, "DescribeAccountLimits is not registered in elbv2Actions")

	input, err := action.parse(map[string]string{"Action": "DescribeAccountLimits"})
	require.NoError(t, err)

	body, err := action.dispatch(t.Context(), "DescribeAccountLimits", input, gw, accountID)
	require.NoError(t, err)

	var decoded accountLimitsResponse
	require.NoError(t, xml.Unmarshal(body, &decoded))
	require.NotEmpty(t, decoded.Limits)

	byName := make(map[string]string, len(decoded.Limits))
	for _, limit := range decoded.Limits {
		byName[limit.Name] = limit.Max
	}
	return byName
}

// The reported cap must be the one CreateLoadBalancer enforces, including an
// override an operator has stored since the gateway started.
func TestELBv2DescribeAccountLimitsReportsStoredQuota(t *testing.T) {
	gw, account := quotaTestGateway(t)

	byName := describeAccountLimits(t, gw, account)
	assert.Equal(t, "2", byName["application-load-balancers"])
	assert.Equal(t, "2", byName["network-load-balancers"])

	require.NoError(t, gw.Quota.PutAccountQuota(t.Context(), account,
		handlers_quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))

	byName = describeAccountLimits(t, gw, account)
	assert.Equal(t, "9", byName["application-load-balancers"])
	assert.Equal(t, "9", byName["network-load-balancers"])
}

// The limit must be resolved per request, so one account's override cannot be
// served to another.
func TestELBv2DescribeAccountLimitsIsPerAccount(t *testing.T) {
	gw, account := quotaTestGateway(t)
	const other = "000000000043"

	require.NoError(t, gw.Quota.PutAccountQuota(t.Context(), account,
		handlers_quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))
	require.NoError(t, gw.Quota.PutAccountQuota(t.Context(), other,
		handlers_quota.Overrides{LoadBalancers: quotaPtr(4)}, "operator"))

	assert.Equal(t, "9", describeAccountLimits(t, gw, account)["application-load-balancers"])
	assert.Equal(t, "4", describeAccountLimits(t, gw, other)["application-load-balancers"])
}

// A disabled quota means "not enforced", not "nothing permitted", so the entries
// report the AWS default rather than the unconsulted configured value.
func TestELBv2DescribeAccountLimitsDefaultsWhenQuotaDisabled(t *testing.T) {
	disabled := handlers_quota.New(handlers_quota.Limits{LoadBalancers: 0}, nil)

	for name, gw := range map[string]*GatewayConfig{
		"disabled": {DisableLogging: true, Quota: disabled},
		"unwired":  {DisableLogging: true},
	} {
		t.Run(name, func(t *testing.T) {
			byName := describeAccountLimits(t, gw, "000000000042")
			assert.Equal(t, "50", byName["application-load-balancers"])
			assert.Equal(t, "50", byName["network-load-balancers"])
		})
	}
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

// An exempt account has no cap enforced against it, so the resolver answers with
// the AWS default without reaching the override store.
func TestELBv2AccountLoadBalancerLimitDefaultsForExemptAccount(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	limit, err := accountLoadBalancerLimit(t.Context(), gw, "000000000042")
	require.NoError(t, err)
	assert.Equal(t, gateway_elbv2.DefaultLoadBalancerLimit, limit)
}

// The dispatch entry must accept the paging parameters the model defines and
// still answer with the whole list.
func TestELBv2DescribeAccountLimitsAcceptsPaging(t *testing.T) {
	gw, account := quotaTestGateway(t)

	action := elbv2Actions["DescribeAccountLimits"]
	input, err := action.parse(map[string]string{
		"Action": "DescribeAccountLimits", "Marker": "2", "PageSize": "1",
	})
	require.NoError(t, err)
	require.IsType(t, &elbv2.DescribeAccountLimitsInput{}, input)

	body, err := action.dispatch(t.Context(), "DescribeAccountLimits", input, gw, account)
	require.NoError(t, err)

	var decoded accountLimitsResponse
	require.NoError(t, xml.Unmarshal(body, &decoded))
	assert.Len(t, decoded.Limits, len(gateway_elbv2.DescribeAccountLimits(&elbv2.DescribeAccountLimitsInput{}, 2).Limits))
	assert.NotContains(t, string(body), "NextMarker")
}
