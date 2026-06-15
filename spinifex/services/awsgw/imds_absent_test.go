package awsgw

import (
	"os"
	"strings"
	"testing"
)

// TestIMDSRuntimeAbsentFromAWSGW guards against importing the privileged IMDS
// listener stack (NewIMDSServiceImpl) into awsgw; that runtime lives in vpcd.
func TestIMDSRuntimeAbsentFromAWSGW(t *testing.T) {
	src, err := os.ReadFile("awsgw.go")
	if err != nil {
		t.Fatalf("read awsgw.go: %v", err)
	}
	if strings.Contains(string(src), "NewIMDSServiceImpl") {
		t.Fatal("awsgw must not construct the IMDS runtime (NewIMDSServiceImpl) — " +
			"the privileged listener stack belongs in vpcd.")
	}
}
