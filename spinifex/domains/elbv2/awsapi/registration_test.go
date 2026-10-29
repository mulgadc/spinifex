package awsapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistration_RegistersUnderCredentialScopeAndInventoryName(t *testing.T) {
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(NewRegistration(Deps{})))
	reg := b.Build()

	entry, ok := reg.Lookup("elasticloadbalancing")
	require.True(t, ok)
	assert.Equal(t, dispatch.ErrorEnvelopeXML, entry.ErrorEnvelope())

	inv := reg.Inventory()["elasticloadbalancingv2"]
	assert.Equal(t, ActionNames(), inv.Registered)
	assert.Empty(t, inv.Stubbed)
	assert.Empty(t, inv.Unsupported)
}

func TestResolveAction_ReadsActionAndRewindsBody(t *testing.T) {
	body := "Action=DescribeLoadBalancers&Version=2015-12-01"
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	assert.Equal(t, "DescribeLoadBalancers", resolveAction(r))

	again, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(again), "the dispatcher must still see the whole body")
}

func TestResolveAction_UnparseableBodyIsEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=%zz"))
	assert.Empty(t, resolveAction(r))
}

// A registration with no quota reports the AWS default cap and enforces none.
func TestDescribeAccountLimits_NoQuotaReportsDefault(t *testing.T) {
	out, err := actions["DescribeAccountLimits"].dispatch(t.Context(), "DescribeAccountLimits",
		&elbv2.DescribeAccountLimitsInput{}, Deps{}, "123456789012")
	require.NoError(t, err)
	assert.Contains(t, string(out), "<Max>50</Max><Name>application-load-balancers</Name>")
}

type fixedQuota struct{ limit int }

func (fixedQuota) EnforceLoadBalancers(_ context.Context, _ *nats.Conn, _ string, _ int) error {
	return nil
}
func (fixedQuota) Exempt(string) bool { return false }
func (q fixedQuota) LoadBalancerLimit(context.Context, string) (int, error) {
	return q.limit, nil
}

func TestDescribeAccountLimits_ReportsTheQuotaLimit(t *testing.T) {
	out, err := actions["DescribeAccountLimits"].dispatch(t.Context(), "DescribeAccountLimits",
		&elbv2.DescribeAccountLimitsInput{}, Deps{Quota: fixedQuota{limit: 7}}, "123456789012")
	require.NoError(t, err)
	assert.Contains(t, string(out), "<Max>7</Max><Name>application-load-balancers</Name>")
}
