package gateway

import "github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"

// withVerifier authenticates through the same IAM and STS instances the config
// holds, so a test that changes one of them changes authentication too.
func withVerifier(gw *GatewayConfig) *GatewayConfig {
	gw.Credentials = credverify.New(gw.IAMService, gw.STSService)
	return gw
}
