package awsapi

import (
	"context"
	"encoding/xml"
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

// A registration with no quota reports the AWS default cap and enforces none.
func TestDescribeAccountLimits_NoQuotaReportsDefault(t *testing.T) {
	out, err := actions["DescribeAccountLimits"].dispatch(t.Context(), "DescribeAccountLimits",
		&elbv2.DescribeAccountLimitsInput{}, Deps{}, "123456789012")
	require.NoError(t, err)
	assert.Equal(t, "50", limitMax(t, out, "application-load-balancers"))
}

// limitMax reads one limit out of the response by name, because the members of
// a limit are written in map order.
func limitMax(t *testing.T, body []byte, name string) string {
	t.Helper()
	var resp struct {
		Limits []struct {
			Name string `xml:"Name"`
			Max  string `xml:"Max"`
		} `xml:"DescribeAccountLimitsResult>Limits>member"`
	}
	require.NoError(t, xml.Unmarshal(body, &resp))
	for _, l := range resp.Limits {
		if l.Name == name {
			return l.Max
		}
	}
	t.Fatalf("limit %q not in response", name)
	return ""
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
	assert.Equal(t, "7", limitMax(t, out, "application-load-balancers"))
}
