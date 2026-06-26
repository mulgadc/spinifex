//go:build e2e

package cert

import (
	"crypto/tls"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTLSPosture asserts the deployed awsgw listeners actually negotiate
// TLS 1.3 and a PQ-hybrid curve from the pinned policy. This is the live
// proof for the PQC Phase 2.1 work:
// it runs under the production FIPS build (GOFIPS140=v1.0.0,
// GODEBUG=fips140=on) and catches regressions where unit tests pass but
// something in the deploy path silently disables ML-KEM.
func TestTLSPosture(t *testing.T) {
	env := harness.LoadEnv(t)

	approvedHybrids := []tls.CurveID{
		tls.X25519MLKEM768,     // Cat 3, wins under Go 1.26 stdlib default order
		tls.SecP384r1MLKEM1024, // Cat 5, CNSA 2.0
	}

	for _, ip := range env.ServiceIPs {
		ip := ip
		t.Run(ip, func(t *testing.T) {
			t.Parallel()
			version, curve, err := harness.FetchTLSPosture(ip, env.AWSGWPort, env.DefaultTimeout)
			require.NoErrorf(t, err, "tls posture probe %s:%d", ip, env.AWSGWPort)

			assert.Equalf(t, uint16(tls.VersionTLS13), version,
				"negotiated version on %s:%d = 0x%x, want TLS 1.3", ip, env.AWSGWPort, version)
			assert.Truef(t, slices.Contains(approvedHybrids, curve),
				"negotiated curve on %s:%d = %v, want one of %v (PQ hybrid)",
				ip, env.AWSGWPort, curve, approvedHybrids)
			t.Logf("awsgw %s:%d negotiated version=%s curve=%v",
				ip, env.AWSGWPort, tls.VersionName(version), curve)
		})
	}
}
