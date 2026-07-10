package admin

import (
	"os"
	"testing"
)

// TestMain lowers the RSA key size for the whole admin test package. 2048-bit
// keys are still valid and generate several times faster than the 4096-bit
// production default, which dominates the admin suite runtime.
func TestMain(m *testing.M) {
	certKeyBits = 2048
	os.Exit(m.Run())
}
