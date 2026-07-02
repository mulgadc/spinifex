package host

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestExecRunnerSeparatesStdoutStderr guards the regression where sudo's
// "unable to send audit message" stderr line (emitted under a restrictive
// CapabilityBoundingSet) was folded into the parsed value by CombinedOutput,
// breaking prefix/equality parsing in ListIMDSTaps and the other ovs/ovn
// readers. Success must return stdout only; failure must retain stderr so the
// "File exists" idempotency checks still match.
func TestExecRunnerSeparatesStdoutStderr(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("execRunner shells via sudo for non-root; run as root to exercise the direct path")
	}
	r := NewExecRunner()

	out, err := r.Run(context.Background(), "sh", "-c", "echo noise >&2; echo value")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "value" {
		t.Errorf("success must return stdout only, got %q (stderr must be excluded)", got)
	}

	out, err = r.Run(context.Background(), "sh", "-c", "echo RTNETLINK answers: File exists >&2; exit 2")
	if err == nil {
		t.Fatal("expected error from exit 2")
	}
	if !strings.Contains(string(out), "File exists") {
		t.Errorf("error path must retain stderr for diagnostics, got %q", out)
	}
}
