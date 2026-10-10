package identifiers

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateResourceID generates a unique resource ID with the given prefix.
// Format: {prefix}-{17 hex chars} using crypto/rand.
func GenerateResourceID(prefix string) string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		//nolint:forbidigo // Continuing after the CSPRNG fails could create predictable resource IDs.
		panic("crypto/rand failed: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b)[:17]
}
