package systemd

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// unitsDir locates build/systemd by walking up from this test file.
func unitsDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	for range 8 {
		cand := filepath.Join(dir, "build", "systemd")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("build/systemd not found above %s", self)
	return ""
}

func readUnit(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func unitFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".service") || strings.HasSuffix(n, ".slice") {
			out = append(out, n)
		}
	}
	return out
}

// hasDirective reports whether unit carries an exact directive line (trimmed).
func hasDirective(unit, line string) bool {
	for l := range strings.SplitSeq(unit, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// TestRG9_TierConfinement asserts the RG-9 per-tier least-privilege contract:
// the locked-down storage/control tier drops all capabilities; the daemon and
// vpcd are the two privileged exceptions, each confined to exactly what its role
// needs and no broader. Adding a capability/device to any tier — or dropping the
// baseline from the locked-down tier — without updating this test fails CI.
func TestRG9_TierConfinement(t *testing.T) {
	dir := unitsDir(t)

	// Storage/control tier: near-zero privilege.
	lockedDown := []string{
		"spinifex-nats.service",
		"spinifex-predastore.service",
		"spinifex-viperblock.service",
		"spinifex-awsgw.service",
		"spinifex-ui.service",
	}
	for _, name := range lockedDown {
		u := readUnit(t, dir, name)
		for _, want := range []string{
			"CapabilityBoundingSet=", // empty — all caps dropped
			"NoNewPrivileges=yes",
			"ProtectSystem=strict",
			"MemoryDenyWriteExecute=yes",
			"SystemCallArchitectures=native",
			"RestrictNamespaces=yes",
		} {
			if !hasDirective(u, want) {
				t.Errorf("RG-9: %s (locked-down tier) must carry %q", name, want)
			}
		}
	}

	// Daemon tier: privileged by necessity (GPU vfio), no broader.
	daemon := readUnit(t, dir, "spinifex-daemon.service")
	if !hasDirective(daemon, "AmbientCapabilities=CAP_SYS_ADMIN CAP_DAC_OVERRIDE") {
		t.Error("RG-9: daemon must carry exactly CAP_SYS_ADMIN CAP_DAC_OVERRIDE — no broader")
	}
	for _, dev := range []string{
		"DeviceAllow=/dev/kvm rw",
		"DeviceAllow=/dev/net/tun rw",
		"DeviceAllow=char-vfio rw",
		"DeviceAllow=/dev/vfio/vfio rw",
	} {
		if !hasDirective(daemon, dev) {
			t.Errorf("RG-9: daemon must carry the explicit device allowlist entry %q", dev)
		}
	}
	if hasDirective(daemon, "NoNewPrivileges=yes") {
		t.Error("RG-9/RG-10: daemon must NOT set NoNewPrivileges=yes while it shells out to sudo (tracked RG-10 gap)")
	}
	for _, want := range []string{"MemoryDenyWriteExecute=yes", "SystemCallArchitectures=native"} {
		if !hasDirective(daemon, want) {
			t.Errorf("RG-9: daemon must keep hardening baseline %q", want)
		}
	}

	// Network tier (vpcd): privileged for in-process netns; a documented exception,
	// not the locked-down tier. It deliberately runs NoNewPrivileges=no.
	vpcd := readUnit(t, dir, "spinifex-vpcd.service")
	if !hasDirective(vpcd, "NoNewPrivileges=no") {
		t.Error("RG-9: vpcd (network tier) is the documented NoNewPrivileges=no exception")
	}
	if !hasDirective(vpcd, "SystemCallArchitectures=native") {
		t.Error("RG-9: vpcd must keep SystemCallArchitectures=native")
	}
}

// TestRG11_LeanUnits asserts the RG-11 contract: unit/slice files carry settings
// plus terse # RG-n references, not paragraphs of rationale, and never reference
// a plan doc, bead, or CI run (project policy — reasoning lives in the ADR).
func TestRG11_LeanUnits(t *testing.T) {
	dir := unitsDir(t)
	// Plan/bead/doc/CI-run references that must not appear in a unit comment.
	planRef := regexp.MustCompile(`(?i)siv-[0-9]+|mulga-[a-z0-9-]+|[a-z0-9_-]+\.md|\b[0-9]{9,}\b`)
	const maxComments = 12

	for _, name := range unitFiles(t, dir) {
		u := readUnit(t, dir, name)
		comments := 0
		for l := range strings.SplitSeq(u, "\n") {
			ls := strings.TrimSpace(l)
			if !strings.HasPrefix(ls, "#") {
				continue
			}
			comments++
			if m := planRef.FindString(ls); m != "" {
				t.Errorf("RG-11: %s comment references a plan/bead/CI artifact (%q); rationale belongs in the ADR: %s", name, m, ls)
			}
		}
		if comments > maxComments {
			t.Errorf("RG-11: %s has %d comment lines (budget %d) — strip the rationale novel, keep settings + a terse # RG-n tag", name, comments, maxComments)
		}
	}
}
