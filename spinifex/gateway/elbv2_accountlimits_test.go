//test:in-package — drives the ELBv2 service through the gateway test helpers
// and the quota fixtures, none of which are reachable from outside the package.

package gateway

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

// elbv2Request builds an authenticated Query request for body, as the SigV4
// middleware would hand it to the service.
func elbv2Request(accountID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "elasticloadbalancing")
	ctx = withPrincipalField(ctx, fieldAccountID, accountID)
	return withTestIdentity(req.WithContext(ctx))
}

// serveAccountLimits runs DescribeAccountLimits through the service with live
// NATS, so the request is dispatched rather than stopped at the NATS guard.
func serveAccountLimits(t *testing.T, gw *GatewayConfig, accountID, body string) ([]byte, error) {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	gw.NATSConn = nc
	gw.IAMService = allowAllIAMService()
	withELBv2(gw)

	w := httptest.NewRecorder()
	if err := gw.serveELBv2(w, elbv2Request(accountID, body)); err != nil {
		return nil, err
	}
	return w.Body.Bytes(), nil
}

// describeAccountLimits decodes the XML DescribeAccountLimits writes, keyed by
// limit name.
func describeAccountLimits(t *testing.T, gw *GatewayConfig, accountID string) map[string]string {
	t.Helper()

	body, err := serveAccountLimits(t, gw, accountID, "Action=DescribeAccountLimits")
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
		quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))

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
		quota.Overrides{LoadBalancers: quotaPtr(9)}, "operator"))
	require.NoError(t, gw.Quota.PutAccountQuota(t.Context(), other,
		quota.Overrides{LoadBalancers: quotaPtr(4)}, "operator"))

	assert.Equal(t, "9", describeAccountLimits(t, gw, account)["application-load-balancers"])
	assert.Equal(t, "4", describeAccountLimits(t, gw, other)["application-load-balancers"])
}

// A disabled quota means "not enforced", not "nothing permitted", so the entries
// report the AWS default rather than the unconsulted configured value.
func TestELBv2DescribeAccountLimitsDefaultsWhenQuotaDisabled(t *testing.T) {
	disabled := quota.New(quota.Limits{LoadBalancers: 0}, nil)

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

// A sealed override must refuse the load balancer cap with Blocked rather
// than the generic InternalError every other read failure maps to.
func TestELBv2DescribeAccountLimitsBlockedOnSealedRecord(t *testing.T) {
	gw, account := quotaTestGateway(t)
	gw.Quota.SetAccountTerminationChecker(&quotaStubChecker{terminating: map[string]bool{account: true}})
	_, err := gw.Quota.SealAccountOverrides(t.Context(), account)
	require.NoError(t, err)

	body, err := serveAccountLimits(t, gw, account, "Action=DescribeAccountLimits")
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorBlocked, err.Error())
	assert.Empty(t, body)
}

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
