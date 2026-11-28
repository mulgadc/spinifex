package vm

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const seededCap = "net_raw"

// TestQEMULaunchDropsChildCapabilities measures the capability masks of a
// process launched the way QEMU is launched, through the same Config.Execute
// path the daemon uses.
//
// The measurement must run as a non-root identity holding a seeded ambient
// capability: root's permitted set survives exec regardless of the wrapper,
// and an unprivileged process without an ambient capability has nothing to
// drop, so neither shape can discriminate the fix. This invocation arranges
// that identity — directly under setpriv when it is root, under sudo when it
// is an unprivileged CI runner — and re-execs itself; the re-exec'd process
// is the real measurement.
func TestQEMULaunchDropsChildCapabilities(t *testing.T) {
	switch {
	case os.Geteuid() == 0:
		t.Log("root: re-executing as a non-root identity with a seeded ambient capability")
		reexecSeeded(t, "setpriv")
		return
	case ambientCaps() != 0:
		t.Log("non-root holding an ambient capability: measuring in place")
	default:
		if _, err := exec.LookPath("sudo"); err != nil {
			t.Skip("needs a seeded ambient capability; neither root nor sudo is available here")
		}
		t.Log("unprivileged: re-executing under sudo with a seeded ambient capability")
		reexecSeeded(t, "sudo", "-n", "setpriv")
		return
	}

	// A script stands in for the QEMU binary: it signals that the exec has
	// landed, then stays alive long enough for the masks to be read. Under
	// the wrapper the pid belongs to setpriv until the exec happens, so
	// sampling before the signal would read the wrapper's inherited masks.
	dir := t.TempDir()
	target := filepath.Join(dir, "qemu-standin")
	marker := filepath.Join(dir, "execed")
	script := "#!/bin/sh\necho ready > " + marker + "\nexec sleep 30\n"
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

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in never signalled the exec")
		}
		time.Sleep(10 * time.Millisecond)
	}

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

// reexecSeeded runs this test again as a non-root identity holding the seeded
// ambient capability, via prefix ("setpriv", or "sudo -n setpriv" on a CI
// runner), and fails when the re-exec'd measurement fails. The test binary is
// copied into a unique directory first: a build-cache binary is not traversable
// by the switched identity, and a shared path would collide between concurrent
// runs. The directory is traversable without being listable, and only the
// parent writes to it.
func reexecSeeded(t *testing.T, prefix ...string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "capdrop-reexec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "test-bin")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	args := append([]string{}, prefix...)
	args = append(args,
		"--reuid=65534", "--regid=65534", "--clear-groups",
		"--ambient-caps=+"+seededCap, "--inh-caps=+"+seededCap, "--no-new-privs", "--",
		bin, "-test.run=^TestQEMULaunchDropsChildCapabilities$", "-test.v", "-test.timeout=5m",
	)
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("seeded re-exec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestQEMULaunchDropsChildCapabilities") {
		t.Fatalf("seeded re-exec did not pass:\n%s", out)
	}
}

// ambientCaps reads this process's CapAmb from /proc/self/status; 0 when the
// field is absent or unreadable.
func ambientCaps() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if v, ok := strings.CutPrefix(scanner.Text(), "CapAmb:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
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
