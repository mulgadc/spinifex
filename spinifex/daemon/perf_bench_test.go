package daemon

import (
	"os"
	"testing"
)

// Phase 1 gate constants — changing these is a deliberate decision.
const (
	phase1BootGateMs = 500
	phase1RSSGateMiB = 25
)

func TestBootGate_Phase1(t *testing.T) {
	if phase1BootGateMs != 500 {
		t.Errorf("Phase 1 boot gate changed: got %d, want 500", phase1BootGateMs)
	}
	if phase1RSSGateMiB != 25 {
		t.Errorf("Phase 1 RSS gate changed: got %d, want 25", phase1RSSGateMiB)
	}
}

func TestMeasureQEMURSSMiB_Self(t *testing.T) {
	rss, err := measureQEMURSSMiB(os.Getpid())
	if err != nil {
		t.Fatalf("measureQEMURSSMiB self: %v", err)
	}
	if rss <= 0 {
		t.Errorf("expected RSS > 0, got %f", rss)
	}
	if rss > 1024 {
		t.Errorf("RSS %f MiB seems unreasonably high for a test process", rss)
	}
}

func TestMeasureQEMURSSMiB_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, -999} {
		_, err := measureQEMURSSMiB(pid)
		if err == nil {
			t.Errorf("expected error for pid=%d, got nil", pid)
		}
	}
}

func TestMeasureQEMURSSMiB_NonExistentPID(t *testing.T) {
	_, err := measureQEMURSSMiB(999999999)
	if err == nil {
		t.Error("expected error for non-existent pid, got nil")
	}
}
