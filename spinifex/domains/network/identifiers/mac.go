// Package identifiers derives stable network identifiers from resource IDs.
package identifiers

import (
	"crypto/sha256"
	"net"
)

// HashMAC returns a deterministic locally-administered unicast MAC for id
// (SHA-256; first octet 0x02). id must be globally unique; callers sharing a
// base ID across resource classes must compose a class tag (for example,
// "dev:"+id).
func HashMAC(id string) string {
	sum := sha256.Sum256([]byte(id))
	b := make([]byte, 6)
	b[0] = 0x02
	copy(b[1:], sum[:5])
	return net.HardwareAddr(b).String()
}
