package host

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the spinifex module root from this file's location
// so systemd-unit invariant tests can read build/ artifacts.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "build", "systemd")); err != nil {
		t.Fatalf("could not locate build/systemd from %s: %v", root, err)
	}
	return root
}

func readUnit(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestIMDS_NetnsHostMountNSInvariant pins the post-netns sandbox contract: per-tap
// IMDS removed the in-process setns, so vpcd no longer enters a per-VPC netns. The
// filesystem sandbox is restored, CAP_SYS_ADMIN is dropped, and namespaces lock down.
func TestIMDS_NetnsHostMountNSInvariant(t *testing.T) {
	root := repoRoot(t)

	vpcd := readUnit(t, root, "build/systemd/spinifex-vpcd.service")
	target := readUnit(t, root, "build/systemd/spinifex.target")

	// Filesystem-sandbox directives are restored now that no setns runs.
	for _, want := range []string{
		"ProtectSystem", "ProtectHome", "PrivateTmp", "ProtectKernelTunables",
		"ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups",
		"ProtectProc", "ReadOnlyPaths", "ReadWritePaths",
	} {
		if !directiveSet(vpcd, want) {
			t.Errorf("spinifex-vpcd.service must set %s= — per-tap IMDS removed the in-process setns, so the filesystem sandbox is restored", want)
		}
	}

	// MountFlags was the shared-mount workaround the netns required; it must not return.
	if directiveSet(vpcd, "MountFlags") {
		t.Errorf("spinifex-vpcd.service must not set MountFlags= — the shared-mount workaround retired with the netns")
	}

	// CAP_SYS_ADMIN's sole consumer was setns(CLONE_NEWNET); with the netns gone no
	// capability directive may grant it, and namespaces lock down (no net/mnt allowance).
	for _, key := range []string{"AmbientCapabilities", "CapabilityBoundingSet"} {
		if strings.Contains(directiveValue(vpcd, key), "SYS_ADMIN") {
			t.Errorf("spinifex-vpcd.service %s must not grant CAP_SYS_ADMIN — its sole consumer (setns) is gone", key)
		}
	}
	if v := directiveValue(vpcd, "RestrictNamespaces"); v != "yes" {
		t.Errorf("spinifex-vpcd.service must set RestrictNamespaces=yes (no per-VPC netns remains), got %q", v)
	}

	// The removed shared-mount workaround must not reappear by reference.
	if strings.Contains(vpcd, "spinifex-netns.service") && !strings.Contains(vpcd, "# ") {
		t.Errorf("spinifex-vpcd.service must not depend on spinifex-netns.service (removed)")
	}
	if directiveSet(target, "Wants") && strings.Contains(target, "spinifex-netns.service") {
		t.Errorf("spinifex.target must not pull in spinifex-netns.service (removed)")
	}
	if _, err := os.Stat(filepath.Join(root, "build", "systemd", "spinifex-netns.service")); err == nil {
		t.Errorf("build/systemd/spinifex-netns.service must be removed — the shared-mount workaround was proven insufficient")
	}
}

// directiveSet reports whether key is set as an actual unit directive (a line
// `key=...`), ignoring comment prose that merely names it.
func directiveSet(body, key string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
			return true
		}
	}
	return false
}

// directiveValue returns the value of the first `key=...` directive line, or "".
func directiveValue(body, key string) string {
	for line := range strings.SplitSeq(body, "\n") {
		s := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(s, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
