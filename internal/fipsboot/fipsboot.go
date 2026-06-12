// Package fipsboot enforces FIPS 140-3 at startup via blank import in every binary.
// Requires GOFIPS140=v1.0.0 at build time; panics if GODEBUG=fips140=off disables
// the runtime FIPS mode.
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
