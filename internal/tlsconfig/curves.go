// Package tlsconfig pins the cluster-wide TLS 1.3 curve preference list.
//
// The list is hardcoded because mixed classification on a single cluster is
// not supported and the certified FIPS build is itself the per-deployment
// choice. A typo in a config string would silently downgrade where a missing
// constant cannot.
package tlsconfig

import "crypto/tls"

// Curves is the fixed PQ-only TLS 1.3 curve allowlist:
//
//   - X25519MLKEM768     — Cat 3 ML-KEM hybrid, wins under Go 1.26 stdlib filter
//   - SecP384r1MLKEM1024 — Cat 5 ML-KEM hybrid, Cat 5 fallback for peers that prefer it
//
// Classical curves (X25519, P-384, P-256, P-521) and the weak hybrid
// SecP256r1MLKEM768 are intentionally excluded so they cannot be advertised.
// Pairing this allowlist with tls.VersionTLS13 minimum closes the HNDL gap
// absolutely: every handshake is ML-KEM hybrid, no classical key exchange,
// no TLS 1.2.
//
// Go 1.26 stdlib treats tls.Config.CurvePreferences as an allowlist filter
// applied to the stdlib default order, not as a user-supplied order.
//
// Callers must not mutate this slice. Assign it directly to
// tls.Config.CurvePreferences; do not append to it.
var Curves = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.SecP384r1MLKEM1024,
}
