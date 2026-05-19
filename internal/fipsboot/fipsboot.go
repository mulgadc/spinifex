// Package fipsboot enforces FIPS 140-3 mode at process startup.
//
// Blank-import this package from every binary's main:
//
//	import _ "github.com/mulgadc/spinifex/internal/fipsboot"
//
// Builds with GOFIPS140=v1.0.0 link the Go Cryptographic Module v1.0.0
// (NIST CMVP cert #5247). At runtime FIPS mode can still be turned off
// via GODEBUG=fips140=off; init() panics in that case to close the hole.
package fipsboot

import (
	"crypto/fips140"
)

func init() {
	if !fips140.Enabled() {
		panic("fipsboot: FIPS 140-3 mode is not enabled at runtime — refusing to start. " +
			"Build with GOFIPS140=v1.0.0 and do not set GODEBUG=fips140=off.")
	}
}
