package awsgw

import (
	"os"
	"strings"
	"testing"
)

// TestIMDSRuntimeAbsentFromAWSGW guards against re-introducing the privileged
// IMDS listener stack into awsgw, whose systemd unit is the most hardened in
// the fleet (NoNewPrivileges=yes, empty CapabilityBoundingSet). The veth +
// privileged-socket bind cannot succeed there; the runtime lives in vpcd, and
// awsgw only answers the IMDS control-plane RPCs (STS + IAM).
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
