// Package tlsconfig pins the cluster-wide TLS 1.3 curve preference list.
//
// The list is hardcoded because mixed classification on a single cluster is
// not supported and the certified FIPS build is itself the per-deployment
// choice. A typo in a config string would silently downgrade where a missing
// constant cannot.
package tlsconfig

import "crypto/tls"

// Curves is the fixed TLS 1.3 curve allowlist:
//
//   - X25519MLKEM768     — Cat 3 ML-KEM hybrid, wins under Go 1.26 stdlib filter
//   - SecP384r1MLKEM1024 — Cat 5 ML-KEM hybrid, fallback for peers that prefer it
//   - X25519             — classical TLS 1.3 fallback for SDKs without ML-KEM (aws CLI v2 today)
//   - CurveP384          — classical TLS 1.3 fallback (Java/.NET enterprise SDKs)
//
// PQ-capable peers (browsers since 2024, Go aws-sdk v2 with Go 1.24+) always
// negotiate the hybrid first; classical fallback only triggers for peers that
// cannot do ML-KEM. Those connections still get TLS 1.3 + AES-256-GCM but
// remain HNDL-exposed — accepted trade-off until AWS CLI v2 / bundled
// OpenSSL ships ML-KEM (currently lagging upstream OpenSSL 3.5).
//
// Weak/niche entries from the Go stdlib default (SecP256r1MLKEM768, P-256,
// P-521) are intentionally excluded so they cannot be advertised.
//
// Go 1.26 stdlib treats tls.Config.CurvePreferences as an allowlist filter
// applied to the stdlib default order, not as a user-supplied order.
//
// Callers must not mutate this slice. Assign it directly to
// tls.Config.CurvePreferences; do not append to it.
var Curves = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.SecP384r1MLKEM1024,
	tls.X25519,
	tls.CurveP384,
}
