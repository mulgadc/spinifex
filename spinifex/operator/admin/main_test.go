package admin

import (
	"os"
	"testing"
)

// TestMain lowers the RSA key size for the whole admin test package. 1024 bits
// is the smallest size crypto/rsa generates, and it runs the same certificate
// code as the 4096-bit production default at a fraction of the CPU.
func TestMain(m *testing.M) {
	certKeyBits = 1024
	os.Exit(m.Run())
}
