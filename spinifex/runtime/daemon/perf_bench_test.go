package daemon

import (
	"os"
	"testing"
)

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
