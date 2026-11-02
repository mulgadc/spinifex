package daemon

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// Measures a child the test owns: the test binary's own RSS grows with every
// test that ran before it, so it cannot bound anything.
func TestMeasureQEMURSSMiB_Self(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	rss, err := measureQEMURSSMiB(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("measureQEMURSSMiB child: %v", err)
	}
	if rss <= 0 {
		t.Errorf("expected RSS > 0, got %f", rss)
	}
	if rss > 64 {
		t.Errorf("RSS %f MiB seems unreasonably high for sleep(1)", rss)
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
