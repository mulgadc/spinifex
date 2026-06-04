package host

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the spinifex submodule root from this test file's location
// (spinifex/network/host/ → three levels up) so the systemd-unit invariants can
// read build/ artifacts regardless of the test's working directory.
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

// TestIMDS_NetnsHostMountNSInvariant pins the host-mount-namespace contract for
// per-VPC IMDS netns (see spinifex-vpcd.service + bead js-133): vpcd creates the
// netns via `ip netns add` and binds the listener via in-process setns, so the
// /run/netns/<vpc> bind-mount must live in the HOST mount namespace — host-visible
// for diagnostics and durable across a vpcd restart. Any filesystem-sandbox
// directive forks a PRIVATE mount ns that traps the bind-mount inside vpcd, so the
// host sees only a stale handle that fails setns(2) EINVAL (CI runs 26920490454,
// 26922587471). The earlier MountFlags=shared + spinifex-netns.service shared-mount
// workaround was proven insufficient and removed; vpcd now simply shares the host
// mount ns. Guard that shape: re-adding any mount-ns directive reintroduces the bug.
func TestIMDS_NetnsHostMountNSInvariant(t *testing.T) {
	root := repoRoot(t)

	vpcd := readUnit(t, root, "build/systemd/spinifex-vpcd.service")
	target := readUnit(t, root, "build/systemd/spinifex.target")

	// No filesystem-sandbox directive may be set on vpcd: each forks a private
	// mount namespace and re-traps the netns bind-mount.
	for _, banned := range []string{
		"ProtectSystem", "ProtectHome", "PrivateTmp", "ProtectKernelTunables",
		"ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups",
		"ProtectProc", "ReadOnlyPaths", "ReadWritePaths", "MountFlags",
	} {
		if directiveSet(vpcd, banned) {
			t.Errorf("spinifex-vpcd.service must share the HOST mount ns but sets %s= — a private mount ns traps /run/netns/<vpc> and host setns(2) fails EINVAL", banned)
		}
	}

	// vpcd still needs CAP_SYS_ADMIN for setns(CLONE_NEWNET) and the net+mnt
	// namespace allowance for the in-netns `ip` plumbing.
	mustContain(t, "spinifex-vpcd.service", vpcd, "CAP_SYS_ADMIN")
	mustContain(t, "spinifex-vpcd.service", vpcd, "RestrictNamespaces=net mnt")

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

func mustContain(t *testing.T, name, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s: missing %q", name, want)
	}
}
