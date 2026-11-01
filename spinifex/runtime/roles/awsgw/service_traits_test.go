package awsgw

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	route53awsapi "github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
)

// productionTraits is the traits each production registration declares.
func productionTraits(t *testing.T) map[string]dispatch.ServiceTraits {
	t.Helper()
	gw := &pipeline.Config{}
	require.NoError(t, wireServiceRegistry(gw,
		iamawsapi.NewRegistration(iamawsapi.Deps{}),
		stsawsapi.NewRegistration(stsawsapi.Deps{}),
		route53awsapi.NewRegistration(route53awsapi.Deps{}),
		eksawsapi.NewRegistration(eksawsapi.Deps{}),
		ec2awsapi.NewRegistration(ec2awsapi.Deps{}),
		operatoradmin.NewRegistration(operatoradmin.Deps{}),
	))
	out := map[string]dispatch.ServiceTraits{}
	for _, svc := range []string{"iam", "sts", "route53", "eks", "ec2", "spinifex"} {
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

func TestProductionServiceTraits_XMLErrorShape(t *testing.T) {
	got := productionTraits(t)

	assert.Equal(t, dispatch.XMLErrorShapeEC2, got["ec2"].XMLErrorShape)
	assert.Equal(t, dispatch.XMLErrorShapeEC2, got["spinifex"].XMLErrorShape)
	for _, svc := range []string{"iam", "sts", "route53", "eks"} {
		assert.Equal(t, dispatch.XMLErrorShapeGeneric, got[svc].XMLErrorShape, svc)
	}
}

// S3 is not served here, but its rejections keep S3's body.
func TestWireServiceRegistry_DeclaresTheS3Scope(t *testing.T) {
	gw := &pipeline.Config{}
	require.NoError(t, wireServiceRegistry(gw, iamawsapi.NewRegistration(iamawsapi.Deps{})))

	traits, ok := gw.Services.Traits("s3")
	require.True(t, ok)
	assert.Equal(t, dispatch.XMLErrorShapeS3, traits.XMLErrorShape)
	assert.True(t, traits.PayloadBody)
	_, served := gw.Services.Lookup("s3")
	assert.False(t, served, "a declared scope is not a served service")
}

func TestProductionServiceTraits_GlobalSigningRegion(t *testing.T) {
	got := productionTraits(t)

	assert.Equal(t, "us-east-1", got["route53"].GlobalSigningRegion)
	for _, svc := range []string{"iam", "sts", "eks", "ec2", "spinifex"} {
		assert.Empty(t, got[svc].GlobalSigningRegion, svc)
	}
}

func TestProductionServiceTraits_OnlyS3ScopeIsPayloadBody(t *testing.T) {
	for svc, traits := range productionTraits(t) {
		assert.False(t, traits.PayloadBody, svc)
	}
}
