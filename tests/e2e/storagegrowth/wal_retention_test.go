//go:build e2e

package storagegrowth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// walRetentionBaseDirEnv overrides where a node keeps per-volume local
	// state, for a cluster laid out differently from the installed default.
	walRetentionBaseDirEnv = "SPINIFEX_VIPERBLOCK_BASEDIR"

	// walRetentionDefaultBaseDir is the installed default.
	walRetentionDefaultBaseDir = "/var/lib/spinifex/viperblock"

	// walRetentionFloorBytes is the smallest retained WAL worth reporting on.
	// Below this the absolute number is too small to matter whatever the ratio,
	// and a short or throttled churn would otherwise shout about a few MiB.
	walRetentionFloorBytes = 256 << 20

	// walRetentionWarnRatio is the fraction of guest-written bytes that may
	// still be sitting in local WAL when the churn ends. A volume that reclaims
	// drained WAL keeps this far below 1; a volume that reclaims nothing sits
	// at roughly 1, because every byte written is still on disk.
	walRetentionWarnRatio = 0.25
)

// walMeasurement is one node's view of a volume's retained local WAL.
type walMeasurement struct {
	Host  string
	Bytes int64
	Files int
}

// reportWALRetention measures the local WAL still on disk for volID and prints
// it prominently, so the retention shows up in the run's output rather than
// only in a node's filesystem.
//
// It must be called while the volume is still ATTACHED. Detaching closes the
// volume, and Close calls RemoveLocalFiles, which deletes the whole per-volume
// directory — so a measurement taken afterwards always reads zero. That is the
// specific reason this condition has never appeared in a CI result despite this
// suite producing it on every run.
//
// Deliberately does NOT fail the test. Retention is unconditional today, so an
// assertion here would be red on every run of every branch and would train
// readers to skip it. It reports loudly instead, and becomes a genuine
// regression guard the day reclaim lands — at which point the warning goes
// quiet on its own and can be turned into a hard assertion.
func reportWALRetention(t *testing.T, volID string, bytesWritten int64) {
	t.Helper()

	m, ok := measureVolumeWAL(t, volID)
	if !ok {
		harness.Step(t, "WAL retention: no local state found for %s on any node — skipping measurement", volID)
		return
	}

	harness.Detail(t, "wal_host", m.Host, "wal_files", m.Files,
		"wal_bytes", m.Bytes, "wal_human", humanBytes(m.Bytes),
		"bytes_written", bytesWritten, "bytes_written_human", humanBytes(bytesWritten))

	var ratio float64
	if bytesWritten > 0 {
		ratio = float64(m.Bytes) / float64(bytesWritten)
	}

	if m.Bytes < walRetentionFloorBytes || ratio < walRetentionWarnRatio {
		harness.Step(t, "WAL retention: %s retained across %d files (%.0f%% of bytes written) — within expectations",
			humanBytes(m.Bytes), m.Files, ratio*100)
		return
	}

	summary := fmt.Sprintf(
		"viperblock retained %s of WAL across %d files on %s for volume %s after writing %s (%.0f%% of everything written is still on local disk). "+
			"Nothing trims a WAL file while a volume is open, so local WAL grows with uptime rather than with outstanding writes. "+
			"On a long-lived volume this fills the node disk and makes the next open replay the entire history. Tracking: mulga-7bhpq.",
		humanBytes(m.Bytes), m.Files, m.Host, volID, humanBytes(bytesWritten), ratio*100)

	printWALBanner(t, m, volID, bytesWritten, ratio)

	// A workflow-command annotation surfaces this on the run's summary page,
	// not just in the log body, which is what gets it in front of someone who
	// did not already scroll to this suite.
	if os.Getenv("GITHUB_ACTIONS") != "" {
		fmt.Fprintf(os.Stdout, "::warning title=viperblock WAL retention (mulga-7bhpq)::%s\n",
			strings.ReplaceAll(summary, "\n", "%0A"))
	}
}

// printWALBanner writes the human-facing block. Straight to stdout for the same
// reason harness.Phase does it: t.Logf buffers until the test ends and is
// indented into the noise.
func printWALBanner(t *testing.T, m walMeasurement, volID string, bytesWritten int64, ratio float64) {
	t.Helper()

	const bar = "════════════════════════════════════════════════════════════════════════"
	lines := []string{
		"",
		"╔" + bar + "╗",
		"  WAL RETENTION — viperblock reclaimed nothing while this volume was open",
		"╚" + bar + "╝",
		fmt.Sprintf("  volume          %s", volID),
		fmt.Sprintf("  node            %s", m.Host),
		fmt.Sprintf("  wal files       %d", m.Files),
		fmt.Sprintf("  wal on disk     %s", humanBytes(m.Bytes)),
		fmt.Sprintf("  bytes written   %s", humanBytes(bytesWritten)),
		fmt.Sprintf("  still retained  %.0f%% of everything the guest wrote", ratio*100),
		"",
		"  A WAL file is only unlinked by crash recovery or by Close. Nothing trims",
		"  one during normal operation, so local WAL grows with UPTIME, not with",
		"  outstanding un-drained writes. Two consequences are already filed:",
		"    mulga-5mukw  a node reaching 100% disk pauses every guest in the cluster",
		"    mulga-xd0gx  an instance cannot restart; open replays the whole history",
		"  Root cause: mulga-7bhpq",
		"",
		"  This does not fail the run. It is unconditional today, so failing here",
		"  would be red on every branch. Turn it into an assertion once reclaim lands.",
		"",
	}
	fmt.Fprintln(os.Stdout, strings.Join(lines, "\n"))
}

// measureVolumeWAL finds the node holding volID's local state and returns its
// retained WAL size and file count. Scans every node rather than resolving
// placement: the volume lives on exactly one node, the check is a stat, and
// scanning keeps this independent of how placement is discovered.
func measureVolumeWAL(t *testing.T, volID string) (walMeasurement, bool) {
	t.Helper()

	baseDir := os.Getenv(walRetentionBaseDirEnv)
	if baseDir == "" {
		baseDir = walRetentionDefaultBaseDir
	}
	walDir := baseDir + "/" + volID + "/wal"

	// Every step runs under sudo, the directory test included. The viperblock
	// base dir is 0700 spinifex-viperblock, so an unprivileged `[ -d ]` is false
	// even when the volume is right there — which would silently downgrade this
	// to "no local state found" on every run.
	script := fmt.Sprintf(`d=%s
sudo -n test -d "$d" || { echo MISSING; exit 0; }
b=$(sudo -n du -sb "$d" 2>/dev/null | cut -f1)
n=$(sudo -n find "$d" -type f 2>/dev/null | wc -l)
echo "OK ${b:-0} ${n:-0}"`, harness.ShellQuote(walDir))

	for _, host := range walRetentionHosts() {
		out, err := runShellOnHost(host, script)
		if err != nil {
			// A node that cannot be reached is not a finding — the volume is
			// on exactly one node and the others are expected to say MISSING.
			harness.Step(t, "WAL retention: probe of %s failed (%v)", displayHost(host), err)
			continue
		}
		b, n, ok := parseWALProbe(out)
		if !ok {
			continue
		}
		return walMeasurement{Host: displayHost(host), Bytes: b, Files: n}, true
	}
	return walMeasurement{}, false
}

// parseWALProbe reads the "OK <bytes> <files>" line the probe script emits.
func parseWALProbe(out string) (int64, int, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 || fields[0] != "OK" {
			continue
		}
		b, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		return b, n, true
	}
	return 0, 0, false
}

// walRetentionHosts returns the node addresses to probe. An empty string means
// "this machine", which is the single-node layout where the test runs on the
// node itself and SPINIFEX_NODES is not set.
func walRetentionHosts() []string {
	c, err := harness.ClusterFromEnv()
	if err != nil || len(c.Nodes) == 0 {
		return []string{""}
	}
	hosts := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		hosts = append(hosts, n.Addr)
	}
	return hosts
}

// runShellOnHost runs script on host, or locally when host is empty.
func runShellOnHost(host, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if host != "" {
		out, err := harness.NewPeerSSH().Run(ctx, host, script)
		return string(out), err
	}
	out, err := exec.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("local shell: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func displayHost(host string) string {
	if host == "" {
		return "local"
	}
	return host
}

// humanBytes renders b in the largest unit that keeps it above 1.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}
