package awsgw

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	route53awsapi "github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// productionTraits is the traits each production registration declares.
func productionTraits(t *testing.T) map[string]dispatch.ServiceTraits {
	t.Helper()
	gw := &gateway.GatewayConfig{}
	require.NoError(t, wireServiceRegistry(gw,
		iamawsapi.NewRegistration(iamawsapi.Deps{}),
		stsawsapi.NewRegistration(stsawsapi.Deps{}),
		route53awsapi.NewRegistration(route53awsapi.Deps{}),
		eksawsapi.NewRegistration(eksawsapi.Deps{}),
	))
	out := map[string]dispatch.ServiceTraits{}
	for _, svc := range []string{"iam", "sts", "route53", "eks"} {
		e, ok := gw.Services.Lookup(svc)
		require.True(t, ok, svc)
		out[svc] = e.Traits()
	}
	return out
}

func TestProductionServiceTraits_XMLThrottleCode(t *testing.T) {
	got := productionTraits(t)

	for _, svc := range []string{"iam", "sts", "route53"} {
		assert.Equal(t, awserrors.ErrorThrottling, got[svc].XMLThrottleCode, svc)
	}
	assert.Empty(t, got["eks"].XMLThrottleCode, "EKS is a JSON registration and never renders the XML throttle")
}
