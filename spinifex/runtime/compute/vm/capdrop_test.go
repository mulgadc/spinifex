package vm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQEMULaunchDropsChildCapabilities measures the capability masks of a
// process launched the way QEMU is launched, through the same Config.Execute
// path the daemon uses. The mechanism can only be exercised from a privileged
// parent: an unprivileged parent has no capabilities to inherit, so both the
// wrapped and the plain shape measure zero and the test cannot discriminate.
// CI runs this as root, where the pre-fix shape fails — the child holds the
// parent's permitted set — and the fixed shape passes with every mask zero.
func TestQEMULaunchDropsChildCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("measuring inherited capabilities needs a privileged parent; CI runs this as root")
	}

	// A script stands in for the QEMU binary: it ignores the QEMU arguments
	// and stays alive long enough for the masks to be read. Going through a
	// shebang keeps the measurement honest for both shapes — the kernel execs
	// /bin/sh either way.
	dir := t.TempDir()
	target := filepath.Join(dir, "qemu-standin")
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	old := resolveQEMUBinary
	resolveQEMUBinary = func(arch string) (string, error) { return target, nil }
	t.Cleanup(func() { resolveQEMUBinary = old })

	cfg := Config{
		Name:         "capdrop-test",
		MachineType:  "pc",
		CPUType:      "base",
		CPUCount:     1,
		Memory:       128,
		Architecture: "x86_64",
		KernelImage:  "/dev/null", // the stand-in script ignores QEMU arguments
	}
	cmd, err := cfg.Execute()
	if err != nil {
		t.Fatalf("build launch command: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start launch command: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	masks, err := procCapabilityMasks(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("child capability masks: inh=%#x prm=%#x eff=%#x amb=%#x",
		masks["CapInh"], masks["CapPrm"], masks["CapEff"], masks["CapAmb"])

	for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		if masks[field] != 0 {
			t.Errorf("launched child holds capabilities: %s=%#x (want 0)", field, masks[field])
		}
	}
}

// procCapabilityMasks reads the capability fields from /proc/<pid>/status.
// The exec race is handled by polling: the child is a sleep, so once the pid
// is readable it stays readable.
func procCapabilityMasks(pid int) (map[string]uint64, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
		if err == nil {
			masks, parseErr := parseCapabilityMasks(f)
			f.Close()
			if parseErr == nil {
				return masks, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("read /proc/%d/status: %w", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseCapabilityMasks(f *os.File) (map[string]uint64, error) {
	masks := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
			if strings.HasPrefix(line, field+":") {
				v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, field+":")), 16, 64)
				if err != nil {
					return nil, err
				}
				masks[field] = v
			}
		}
	}
	if len(masks) != 4 {
		return nil, os.ErrNotExist
	}
	return masks, nil
}
