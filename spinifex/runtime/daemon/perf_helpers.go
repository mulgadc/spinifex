package daemon

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// measureQEMURSSMiB reads /proc/<pid>/status and returns RSS in MiB.
func measureQEMURSSMiB(pid int) (float64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid: %d", pid)
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, fmt.Errorf("open /proc/%d/status: %w", pid, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected VmRSS format: %q", line)
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS kB: %w", err)
		}
		return kb / 1024, nil
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/%d/status", pid)
}
