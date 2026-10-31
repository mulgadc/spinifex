package gateway

import (
	"net/http"
	"testing"
	"time"

	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/registryadapt"
	"github.com/stretchr/testify/require"
)

// newTestRegistryHandler composes the /v2 handler the way the awsgw role does, over
// gw's IAM and STS services, with registry serving authorized requests.
func newTestRegistryHandler(t *testing.T, gw *GatewayConfig, iss *ecrauth.Issuer, verify *ecrauth.Verifier, registry ...http.Handler) http.Handler {
	t.Helper()
	cfg := ecrregistry.BridgeConfig{
		Verifier:  verify,
		Issuer:    iss,
		Admission: accountadmission.New(gw.IAMService),
		Region:    gw.Region,
	}
	cfg = registryadapt.Config(cfg, credverify.New(gw.IAMService, gw.STSService),
		policyauth.New(gw.IAMService, policyauth.WithRetryBase(time.Millisecond)))
	if len(registry) > 0 {
		cfg.Registry = registry[0]
	}
	b, err := ecrregistry.NewBridge(cfg)
	require.NoError(t, err)
	return ingresshttp.NewHostMatcher(gw.InternalSuffix, "dkr", "ecr").Middleware(b.Handler())
}
