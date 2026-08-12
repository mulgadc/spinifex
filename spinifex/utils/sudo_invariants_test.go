package utils_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// policyOwners are the only files allowed to build a sudo invocation. Everything
// else must route through utils.SudoCommand / host.NewExecRunner so the
// escalation policy in NeedsPrivilege applies.
var policyOwners = map[string]bool{
	filepath.Join("spinifex", "utils", "sudo.go"):          true,
	filepath.Join("spinifex", "network", "host", "run.go"): true,
}

// repoRoot walks up from this test file to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("go.mod not found above %s", self)
	return ""
}

// TestRG10_SudoOnlyThroughThePolicy fails when a package builds its own sudo
// invocation. vpcd carried such a copy: every OVS/OVN call it made was escalated
// unconditionally, so it kept working only because the sudoers grants existed,
// and it broke the OVN flows-ready barrier the moment they were removed. A local
// copy also silently re-widens the sudoers surface a reviewer thinks is gone.
func TestRG10_SudoOnlyThroughThePolicy(t *testing.T) {
	root := repoRoot(t)
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			// The e2e harness is not a daemon: it runs as the developer or CI
			// user, who holds full sudo, and shells out to inspect a live node.
			// The policy governs what the service users escalate.
			case "tests":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if policyOwners[rel] {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, `exec.Command(`) && strings.Contains(line, `"sudo"`) {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("RG-10: these build sudo invocations directly, bypassing utils.NeedsPrivilege:\n  %s\n"+
			"Use utils.SudoCommand or host.NewExecRunner so the OVS/OVN socket clients stay unescalated.",
			strings.Join(offenders, "\n  "))
	}
}

// TestRG10_VPCDHoldsNoSudoersGrant pins the two halves that have to move
// together: vpcd's unit grants CAP_NET_ADMIN and CAP_NET_RAW ambiently, and in
// exchange it gets no sudoers rules. Re-adding a rule reopens the hole the caps
// closed — every candidate takes unrestricted args, and `sudo ip netns exec
// <ns> /bin/sh` is a root shell.
func TestRG10_VPCDHoldsNoSudoersGrant(t *testing.T) {
	root := repoRoot(t)

	unit, err := os.ReadFile(filepath.Join(root, "build", "systemd", "spinifex-vpcd.service"))
	if err != nil {
		t.Fatalf("read vpcd unit: %v", err)
	}
	ambient := ambientLine(string(unit))
	for _, capability := range []string{"CAP_NET_ADMIN", "CAP_NET_RAW"} {
		if !strings.Contains(ambient, capability) {
			t.Fatalf("RG-10: spinifex-vpcd.service must grant %s ambiently; without it "+
				"ip/iptables/arping fail as the service user and the sudoers rules cannot stay removed", capability)
		}
	}

	setup, err := os.ReadFile(filepath.Join(root, "scripts", "setup.sh"))
	if err != nil {
		t.Fatalf("read setup.sh: %v", err)
	}
	for i, line := range strings.Split(string(setup), "\n") {
		if strings.HasPrefix(line, "spinifex-vpcd ALL=") {
			t.Fatalf("RG-10: scripts/setup.sh:%d grants spinifex-vpcd a sudoers rule:\n  %s\n"+
				"vpcd runs these under its ambient capabilities; add the capability to the unit instead.", i+1, line)
		}
	}
}

// TestSudoersGrantsCarryNoArgumentWildcard pins the sudo-rs constraint. Ubuntu
// has shipped sudo-rs as the default sudo since 25.10, and it rejects a `*`
// inside a command argument outright — visudo fails and setup.sh aborts.
func TestSudoersGrantsCarryNoArgumentWildcard(t *testing.T) {
	root := repoRoot(t)
	setup, err := os.ReadFile(filepath.Join(root, "scripts", "setup.sh"))
	if err != nil {
		t.Fatalf("read setup.sh: %v", err)
	}
	for i, line := range strings.Split(string(setup), "\n") {
		if !strings.HasPrefix(line, "spinifex-daemon ALL=") {
			continue
		}
		_, cmds, _ := strings.Cut(line, "NOPASSWD:")
		for spec := range strings.SplitSeq(cmds, ",") {
			fields := strings.Fields(spec)
			// A standalone final `*` is the one form sudo-rs accepts, so drop it
			// and treat any wildcard left inside an argument as the failure.
			if n := len(fields); n > 0 && fields[n-1] == "*" {
				fields = fields[:n-1]
			}
			if strings.Contains(strings.Join(fields, " "), "*") {
				t.Fatalf("scripts/setup.sh:%d grants a wildcard inside a command argument:\n  %s\n"+
					"sudo-rs rejects this; name a fixed-verb helper instead (see EndpointSysctlHelper).", i+1, line)
			}
		}
	}
}

// ambientLine returns the unit's AmbientCapabilities= value, or "".
func ambientLine(unit string) string {
	for line := range strings.SplitSeq(unit, "\n") {
		if v, ok := strings.CutPrefix(line, "AmbientCapabilities="); ok {
			return v
		}
	}
	return ""
}
