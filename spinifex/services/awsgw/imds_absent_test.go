package awsgw

import (
	"os"
	"strings"
	"testing"
)

// TestIMDSRuntimeAbsentFromAWSGW is a regression guard for
// docs/development/bugs/imds-relocate-to-vpcd.md: the privileged IMDS listener
// stack must never be constructed inside awsgw, whose systemd unit is the most
// hardened in the fleet (NoNewPrivileges=yes, empty CapabilityBoundingSet). The
// veth + privileged-socket bind cannot succeed there; the runtime lives in vpcd.
// awsgw only answers the IMDS control-plane RPCs (STS + IAM).
func TestIMDSRuntimeAbsentFromAWSGW(t *testing.T) {
	src, err := os.ReadFile("awsgw.go")
	if err != nil {
		t.Fatalf("read awsgw.go: %v", err)
	}
	if strings.Contains(string(src), "NewIMDSServiceImpl") {
		t.Fatal("awsgw must not construct the IMDS runtime (NewIMDSServiceImpl) — " +
			"the privileged listener stack belongs in vpcd. See " +
			"docs/development/bugs/imds-relocate-to-vpcd.md.")
	}
}
