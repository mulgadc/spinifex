//go:debug rsa1024min=0

package admin

import (
	"os"
	"testing"
)

// TestMain lowers the RSA key size for the whole admin test package. The
// 512-bit keys run the same certificate code as the 4096-bit production
// default at a fraction of the CPU; the debug setting above permits them.
func TestMain(m *testing.M) {
	certKeyBits = 512
	os.Exit(m.Run())
}
