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

// TestIMDS_NetnsSharedMountInvariant pins the host<->vpcd mount-propagation
// contract for per-VPC IMDS netns (see spinifex-vpcd.service + bead js-133):
// vpcd's MountFlags=shared is necessary but inert unless /run/netns is ALREADY a
// shared mount on the host before vpcd unshares. spinifex-netns.service supplies
// that, UNSANDBOXED so the make-shared lands in the host mount namespace, ordered
// Before vpcd. Dropping any half of this coupling reintroduces the setns(2)
// EINVAL failure (CI run 26920490454), so guard the whole shape here.
func TestIMDS_NetnsSharedMountInvariant(t *testing.T) {
	root := repoRoot(t)

	vpcd := readUnit(t, root, "build/systemd/spinifex-vpcd.service")
	netns := readUnit(t, root, "build/systemd/spinifex-netns.service")
	target := readUnit(t, root, "build/systemd/spinifex.target")

	// vpcd keeps shared propagation and depends on the host-side prep unit.
	mustContain(t, "spinifex-vpcd.service", vpcd, "MountFlags=shared")
	mustOrderingRefs(t, vpcd, "spinifex-netns.service")

	// The prep unit must run in the HOST mount namespace: oneshot, ordered
	// before vpcd, establishing shared propagation on /run/netns, with NO
	// sandbox directive that would fork a private mount ns and hide it.
	mustContain(t, "spinifex-netns.service", netns, "Type=oneshot")
	mustContain(t, "spinifex-netns.service", netns, "Before=spinifex-vpcd.service")
	mustContain(t, "spinifex-netns.service", netns, "mount --make-shared /run/netns")
	for _, banned := range []string{"ProtectSystem", "ProtectHome", "PrivateTmp", "MountFlags", "RestrictNamespaces"} {
		if directiveSet(netns, banned) {
			t.Errorf("spinifex-netns.service must stay UNSANDBOXED but sets %s= — a private mount ns hides make-shared from the host", banned)
		}
	}

	// The target pulls the prep unit in at boot (units start via the target's
	// Wants=, not per-unit enable).
	mustContain(t, "spinifex.target", target, "spinifex-netns.service")
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

// mustOrderingRefs asserts the unit names the dependency in both After= and
// Wants= so it is both ordered after and pulled in alongside.
func mustOrderingRefs(t *testing.T, body, dep string) {
	t.Helper()
	for _, key := range []string{"After=", "Wants="} {
		found := false
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(line, key) && strings.Contains(line, dep) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("spinifex-vpcd.service: %s line must reference %s", key, dep)
		}
	}
}
