//go:build e2e

package harness

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// EffectiveCapsForUnit returns the effective-capability bitmask (CapEff) of a
// systemd unit's MainPID. ok is false when the unit has no live MainPID or its
// /proc status is unreadable, so the caller can skip the assertion off-node. CapEff
// is read with `sudo -n` since the hardened unit may hide /proc with ProtectProc.
func EffectiveCapsForUnit(t *testing.T, unit string) (caps uint64, ok bool) {
	t.Helper()

	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return 0, false
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return 0, false
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("sudo", "-n", "grep", "-m1", "^CapEff:", "/proc/"+pid+"/status")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, false
	}
	// Line shape: "CapEff:\t00000000000034c0".
	fields := strings.Fields(stdout.String())
	if len(fields) < 2 {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[1], 16, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
