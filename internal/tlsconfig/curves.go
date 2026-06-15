// Package tlsconfig pins the cluster-wide TLS 1.3 curve preference list.
// Hardcoded to prevent silent downgrade via misconfiguration; the FIPS build
// is the per-deployment choice.
package tlsconfig

import "crypto/tls"

// Curves is the fixed TLS 1.3 curve allowlist: two ML-KEM hybrids (PQ-capable
// peers always negotiate these) followed by two classical fallbacks for SDKs
// without ML-KEM. Weak Go defaults (SecP256r1MLKEM768, P-256, P-521) excluded.
var Curves = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.SecP384r1MLKEM1024,
	tls.X25519,
	tls.CurveP384,
}
