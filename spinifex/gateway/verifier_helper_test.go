package gateway

import "github.com/mulgadc/spinifex/internal/testkit/gwfix"

// withVerifier authenticates through the IAM service the config holds, so a
// test that changes it changes authentication too.
func withVerifier(gw *GatewayConfig) *GatewayConfig {
	gw.Credentials = &gwfix.Verifier{IAM: gw.IAMService}
	return withFakeCapabilities(gw)
}
