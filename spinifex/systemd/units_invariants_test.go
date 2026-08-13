package systemd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// allUnitFiles lists every unit in dir regardless of type — .service,
// .slice, .target and .timer — for invariants that apply to all 16.
func allUnitFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		for _, suffix := range []string{".service", ".slice", ".target", ".timer"} {
			if strings.HasSuffix(n, suffix) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// timeoutStopSec parses a unit's TimeoutStopSec= value (this repo always
// writes plain seconds, no unit suffix) into a time.Duration.
func timeoutStopSec(t *testing.T, unit string) time.Duration {
	t.Helper()
	for l := range strings.SplitSeq(unit, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "TimeoutStopSec="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("parse TimeoutStopSec=%q: %v", v, err)
			}
			return time.Duration(n) * time.Second
		}
	}
	t.Fatal("unit has no TimeoutStopSec=")
	return 0
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
		// vhost-net carries the guest NIC datapath; DevicePolicy=closed denies
		// the open with EPERM without it and no vhost=on guest can launch.
		"DeviceAllow=/dev/vhost-net rw",
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
// KillMode=process (guests survive a daemon restart — DDIL reattach), and
// ExecStop passes --unless-restarting so PartOf=spinifex.target firing on
// every target stop only skips the drain when a restart is already queued.
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
	if !hasDirective(drain, "ExecStop=/usr/local/bin/spx admin node drain --local --timeout=120s --unless-restarting") {
		t.Error("spinifex-shutdown.service must run spx admin node drain --local --unless-restarting on stop via ExecStop")
	}

	daemon := readUnit(t, dir, "spinifex-daemon.service")
	if !hasDirective(daemon, "KillMode=process") {
		t.Error("spinifex-daemon.service must keep KillMode=process — guests survive daemon restart (DDIL)")
	}

	// shutdownVolumes deliberately spares nbdkit that a guest is still writing
	// through. Without KillMode=process the cgroup SIGTERMs then SIGKILLs it
	// anyway at TimeoutStopSec, which is the corruption that skip exists to avoid.
	viperblock := readUnit(t, dir, "spinifex-viperblock.service")
	if !hasDirective(viperblock, "KillMode=process") {
		t.Error("spinifex-viperblock.service must keep KillMode=process — in-use nbdkit survives a viperblock restart (DDIL)")
	}

	// shutdownVolumes reaps idle nbdkit concurrently, so the unit only has to
	// outlive the single slowest utils.KillProcess grace (120s), not that
	// grace summed across every mounted volume.
	const killProcessGracePeriod = 120 * time.Second
	if got := timeoutStopSec(t, viperblock); got <= killProcessGracePeriod {
		t.Errorf("spinifex-viperblock.service TimeoutStopSec=%v must strictly exceed the %v KillProcess grace period", got, killProcessGracePeriod)
	}

	target := readUnit(t, dir, "spinifex.target")
	if !strings.Contains(target, "spinifex-shutdown.service") {
		t.Error("spinifex.target Wants= must include spinifex-shutdown.service")
	}
}

// TestRG11_LeanUnits asserts the RG-11 contract: unit/slice files carry settings
// plus terse # RG-n references, not paragraphs of rationale, and never reference
// external planning or CI artifacts (reasoning is kept separately).
func TestRG11_LeanUnits(t *testing.T) {
	dir := unitsDir(t)
	// External planning and CI references that must not appear in a unit comment.
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
			// The version marker is structural metadata, not rationale prose,
			// so it does not count against the comment budget.
			if unitVersionRe.MatchString(ls) {
				continue
			}
			comments++
			if m := planRef.FindString(ls); m != "" {
				t.Errorf("unit comment references external planning or CI material (%q): %s", m, ls)
			}
		}
		if comments > maxComments {
			t.Errorf("RG-11: %s has %d comment lines (budget %d) — strip the rationale novel, keep settings + a terse # RG-n tag", name, comments, maxComments)
		}
	}
}

// TestAllServiceUnitsDeclareTimeoutStopSec asserts every .service unit pins an
// explicit TimeoutStopSec, so a newly added unit that forgets one falls back to
// systemd's blanket 90s default instead of a value chosen for that service's
// own SIGTERM handling.
func TestAllServiceUnitsDeclareTimeoutStopSec(t *testing.T) {
	dir := unitsDir(t)
	for _, name := range unitFiles(t, dir) {
		if !strings.HasSuffix(name, ".service") {
			continue
		}
		u := readUnit(t, dir, name)
		found := false
		for l := range strings.SplitSeq(u, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "TimeoutStopSec=") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s must declare an explicit TimeoutStopSec=", name)
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

// TestGeneratedUnitsMatchSource asserts units_gen.go (the embedded copy
// Reconcile ships and compares against) is byte-identical to build/systemd.
// A stale generated file would make `spx admin upgrade` write the wrong
// content; regenerate with `go generate ./...` when this fails.
func TestGeneratedUnitsMatchSource(t *testing.T) {
	dir := unitsDir(t)
	source := allUnitFiles(t, dir)

	if len(Units) != len(source) {
		t.Errorf("units_gen.go has %d units, build/systemd has %d — run `go generate ./...`", len(Units), len(source))
	}
	for _, name := range source {
		want := readUnit(t, dir, name)
		got, ok := Units[name]
		if !ok {
			t.Errorf("units_gen.go missing %s — run `go generate ./...`", name)
			continue
		}
		if got != want {
			t.Errorf("units_gen.go for %s is stale — run `go generate ./...`", name)
		}
	}
}

// unitBodyHashes pins the sha256 of each unit's body (everything after the
// `# spinifex-unit-version:` line) for its current version. Editing a unit's
// body without bumping the marker and adding a matching entry here fails
// TestUnitBodyPinnedToVersion — the forcing function that keeps a running
// node's reconciled units honest about what changed.
var unitBodyHashes = map[string]map[int]string{
	"regenerate-ssh-host-keys.service": {1: "5b2cf4c2ba5d5799c790e32896928c235db92a069583a87004801f58df390e55"},
	"spinifex-awsgw.service":           {1: "dd662cebca19acca9dbcd0a6f29e6a40125805bdfeabcb94c02fbae07d1f2d6e"},
	"spinifex-daemon.service": {
		1: "04b0f23b9cda322e4012d589acc196abe466b619dec5ac0d832897b9af2f926c",
		2: "97a2d4c72967a9bdf9545ff74593de532e8469568bb717c9958d33b77e62d0ed",
		3: "8059fe13a678b8cb2a383ae75c069560cf39adece9a9394e17781998c711cd57",
	},
	"spinifex-firewall.service":      {1: "01d0a7a79f47eedca02aaab3ff97a0b6462d61b7834e16c7336a7b96034dc392"},
	"spinifex-guests.slice":          {1: "22dac772e81a9a716db98415a4cf590885c9cf19d4290cc21e645e5fe15bc793"},
	"spinifex-nats-watchdog.service": {1: "b778e398da37c4cb3c170781f074c76555cb6a137a7f96f8dc692457b12aa9b8"},
	"spinifex-nats-watchdog.timer":   {1: "9d43a8d2aa4fd80ab5f944d7620c666fd2a255850de2e6494479b8746922c979"},
	"spinifex-nats.service":          {1: "f7f9900b95e364dc2575684a9bf16fa1f96a903a3a4aa773a34c04d761106838"},
	"spinifex-northstar.service":     {1: "030c382aed20f4383acdb7df7a830d48dd76dc29be5965b595cc3842ae43a3f1"},
	"spinifex-predastore.service":    {1: "d0f6415b8f0e6ff3c045f2d3ce2794c347bf141066d7e0bd85fcec48797854d8"},
	"spinifex-qmp-collector.service": {1: "beb18e6dd9351901f19d992cb2f757fb0e0e4a4d986402ccdb0ebb0a449f225c"},
	"spinifex-shutdown.service":      {1: "bcdc455916f35aa7494b2fe25e691339e8f1e22f031dfd9fd95203a9aa4bdaa4"},
	"spinifex-system.slice":          {1: "ca450c2b28a8b13dd767957fa9469bd74bd222d7abed79945e83d564d5ce16dd"},
	"spinifex-ui.service":            {1: "9d5ec3785bf730405f23ec9df79675bf65ecbca9df47c15f5a4b4639393afbc5"},
	"spinifex-viperblock.service":    {1: "5c8cdd2004abf8e5725cbce0200565b9b29be7ab3210a4e0a2cdfe37ec5facb9"},
	"spinifex-vpcd.service":          {1: "1b722640310145767cd34e87f4804852e63f46d2145ed20f8cc5f5400ebc5965"},
	"spinifex.slice":                 {1: "f73d9343e0e1bedd647835c8bb0c80fb3a3bd66474661234ecac23a4caafc24f"},
	"spinifex.target":                {1: "0ffba9faee5a477f8ff7466a6bccb4dc7e04f5cf92a405553c242e6548402078"},
}

// TestUnitBodyPinnedToVersion asserts each unit's body hash matches the
// pinned hash for its current version marker. A body edit without a version
// bump changes the hash at the same version number and fails; bumping the
// version without adding a new table entry also fails, since no entry
// exists for the new version yet.
func TestUnitBodyPinnedToVersion(t *testing.T) {
	dir := unitsDir(t)
	for _, name := range allUnitFiles(t, dir) {
		u := readUnit(t, dir, name)
		version := unitVersion(u)
		body := stripVersionMarkerLine(u)
		sum := sha256.Sum256([]byte(body))
		hash := hex.EncodeToString(sum[:])

		byVersion, ok := unitBodyHashes[name]
		if !ok {
			t.Errorf("%s: no entries in unitBodyHashes — add one for version %d", name, version)
			continue
		}
		want, ok := byVersion[version]
		if !ok {
			t.Errorf("%s: no unitBodyHashes entry for version %d — bumped the marker without pinning its new body hash", name, version)
			continue
		}
		if want != hash {
			t.Errorf("%s: body hash for version %d changed (got %s, want %s) — bump # spinifex-unit-version and add a new unitBodyHashes entry", name, version, hash, want)
		}
	}
}

// stripVersionMarkerLine drops the first line when it is the unit-version
// marker, so the body hash covers content only, not the marker itself.
func stripVersionMarkerLine(content string) string {
	first, rest, found := strings.Cut(content, "\n")
	if found && unitVersionRe.MatchString(strings.TrimSpace(first)) {
		return rest
	}
	return content
}
