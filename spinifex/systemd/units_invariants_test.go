package systemd

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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

// directiveContains reports whether an active list directive contains a value.
func directiveContains(unit, key, value string) bool {
	prefix := key + "="
	for line := range strings.SplitSeq(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, prefix) {
			continue
		}
		if slices.Contains(strings.Fields(strings.TrimPrefix(line, prefix)), value) {
			return true
		}
	}
	return false
}

// TestRG9_TierConfinement asserts the RG-9 least-privilege contract.
// Storage/control tier drops all caps; daemon and vpcd are the two privileged exceptions.
// Adding capabilities or weakening the locked-down baseline without updating this test fails CI.
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

	// Northstar: locked-down baseline plus exactly CAP_NET_BIND_SERVICE so the
	// unprivileged user binds :53 without root. No broader; ambient caps stay
	// compatible with NoNewPrivileges=yes.
	northstar := readUnit(t, dir, "spinifex-northstar.service")
	for _, want := range []string{
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"MemoryDenyWriteExecute=yes",
		"SystemCallArchitectures=native",
	} {
		if !hasDirective(northstar, want) {
			t.Errorf("RG-9: northstar must carry %q (exactly CAP_NET_BIND_SERVICE for :53)", want)
		}
	}

	// Network tier (vpcd): per-tap IMDS dropped the in-process setns, so CAP_SYS_ADMIN
	// is gone and the cap set is exactly the network minimum. NoNewPrivileges stays off
	// (RG-10: vpcd shells out to sudo for ip/ovs-vsctl/dhcpcd, like the daemon).
	vpcd := readUnit(t, dir, "spinifex-vpcd.service")
	for _, want := range []string{
		"AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID",
		"CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID",
	} {
		if !hasDirective(vpcd, want) {
			t.Errorf("RG-9: vpcd must carry exactly %q — CAP_SYS_ADMIN dropped with the per-tap cutover", want)
		}
	}
	if !hasDirective(vpcd, "NoNewPrivileges=no") {
		t.Error("RG-9/RG-10: vpcd (network tier) keeps NoNewPrivileges=no while it shells out to sudo (ip/ovs-vsctl/dhcpcd)")
	}
	if !hasDirective(vpcd, "SystemCallArchitectures=native") {
		t.Error("RG-9: vpcd must keep SystemCallArchitectures=native")
	}
}

// TestOptionalNorthstarActivation keeps the static target and restart wiring
// that surrounds the command's configuration-aware activation behavior.
func TestOptionalNorthstarActivation(t *testing.T) {
	dir := unitsDir(t)
	target := readUnit(t, dir, "spinifex.target")
	if !directiveContains(target, "Wants", "spinifex-northstar.service") {
		t.Error("spinifex.target must start Northstar when node configuration enables it")
	}

	northstar := readUnit(t, dir, "spinifex-northstar.service")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/spx service northstar start",
		"Environment=SPINIFEX_CONFIG_PATH=/etc/spinifex/spinifex.toml",
		"Restart=on-failure",
		"RestartSec=5",
	} {
		if !hasDirective(northstar, want) {
			t.Errorf("configured Northstar activation must retain %q", want)
		}
	}
}

// TestGracefulDrainOrdering asserts the graceful-shutdown contract: the drain
// oneshot orders After= the storage/daemon units (so a target/host stop runs its
// ExecStop drain first, while those services are still up), the daemon keeps
// KillMode=process (guests survive a daemon restart — DDIL reattach), and the
// drain is wired into the target so a target/host stop triggers it.
func TestGracefulDrainOrdering(t *testing.T) {
	dir := unitsDir(t)

	drain := readUnit(t, dir, "spinifex-shutdown.service")
	var afterLine string
	for l := range strings.SplitSeq(drain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "After=") {
			afterLine = strings.TrimSpace(l)
		}
	}
	if afterLine == "" {
		t.Fatal("spinifex-shutdown.service must declare After= the storage/daemon units")
	}
	for _, dep := range []string{
		"spinifex-nats.service",
		"spinifex-predastore.service",
		"spinifex-viperblock.service",
		"spinifex-daemon.service",
	} {
		if !strings.Contains(afterLine, dep) {
			t.Errorf("spinifex-shutdown.service After= must include %s so the drain stops before it", dep)
		}
	}
	if !hasDirective(drain, "ExecStop=/usr/local/bin/spx admin node drain --local --timeout=120s") {
		t.Error("spinifex-shutdown.service must drain the local node on stop via ExecStop")
	}

	daemon := readUnit(t, dir, "spinifex-daemon.service")
	if !hasDirective(daemon, "KillMode=process") {
		t.Error("spinifex-daemon.service must keep KillMode=process — guests survive daemon restart (DDIL)")
	}

	target := readUnit(t, dir, "spinifex.target")
	if !strings.Contains(target, "spinifex-shutdown.service") {
		t.Error("spinifex.target Wants= must include spinifex-shutdown.service")
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

// TestApplicationUnitsExportTelemetry asserts every unit that runs an spx
// application service sources the telemetry drop-in. That file carries
// OTEL_EXPORTER_OTLP_ENDPOINT, MULGA_ENV and MULGA_SOURCE, so a unit missing it
// starts a service whose instruments record into a no-op provider — the process
// runs healthy and simply reports nothing, which is invisible until someone
// notices an empty dashboard.
//
// Units that host an agent or a one-shot rather than an spx service are exempt:
// they either export on their own (the collectors) or have nothing to export.
func TestApplicationUnitsExportTelemetry(t *testing.T) {
	dir := unitsDir(t)
	const telemetry = "EnvironmentFile=-/etc/spinifex/telemetry.env"

	exempt := []string{
		"spinifex-nats-watchdog.service", // periodic health probe, no OTel SDK
		"spinifex-shutdown.service",      // one-shot drain on halt
		"regenerate-ssh-host-keys.service",
	}

	for _, name := range unitFiles(t, dir) {
		if !strings.HasSuffix(name, ".service") || slices.Contains(exempt, name) {
			continue
		}
		u := readUnit(t, dir, name)
		// Only units that actually launch an spx service can emit telemetry.
		if !strings.Contains(u, "/usr/local/bin/spx service ") {
			continue
		}
		if !hasDirective(u, telemetry) {
			t.Errorf("%s runs an spx service but does not source %s; its telemetry would be silently dropped", name, telemetry)
		}
	}
}
