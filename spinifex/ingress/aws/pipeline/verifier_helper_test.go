package pipeline

import "github.com/mulgadc/spinifex/internal/testkit/gwfix"

// withVerifier authenticates through the IAM service the config holds, so a
// test that changes it changes authentication too.
func withVerifier(gw *Config) *Config {
	gw.Credentials = &gwfix.Verifier{IAM: gwfix.DepsOf(gw).IAM}
	return withFakeCapabilities(gw)
}
