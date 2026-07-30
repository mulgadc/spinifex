package utils

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
