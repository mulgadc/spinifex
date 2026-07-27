//go:build e2e

package storagegrowth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	// imagePullDevice is the guest-visible attach point requested for the
	// workload volume. Distinct from npassDevice so the two workloads can
	// never collide if a future change runs them against the same instance.
	imagePullDevice      = "/dev/sdg"
	imagePullVolumeLabel = "imagepull"
	// imagePullMount is the persistent mountpoint for the workload volume.
	// Unlike npass's format-write-unmount cycle, this mount stays live for
	// the whole pull: it doubles as the container runtime's data-root, so
	// every layer byte the pull writes lands on the measured volume instead
	// of the AMI's root disk.
	imagePullMount = "/mnt/imagepull"

	// imagePullMaxDuration bounds the whole install+pull background job.
	// Generous because it only needs to cover the small (~100-500MB)
	// verification image comfortably; a full-size sweep point may need this
	// raised if a much larger image is ever driven through this workload.
	imagePullMaxDuration = 60 * time.Minute

	// imagePullDefaultImage is a public image pinned by digest, used when
	// SPINIFEX_STORAGEGROWTH_IMAGE is unset. docker.io/library/node:20 as of
	// writing: ~398MB compressed across many distinct layers — big enough to
	// write a materially growing, non-repeating footprint (the whole point of
	// this workload versus npass, see below), small enough that a routine run
	// finishes in a few minutes. The full-size scenario this workload exists
	// to reproduce (~13GB) is supplied by overriding the env var, not by
	// raising this default.
	imagePullDefaultImage = "docker.io/library/node@sha256:cacf10e99285cbbc891452e31249c1b5ec3ba225f40028fae946b75aeaf1b66a"

	// Dedicated VPC CIDRs for this workload's own topology (see
	// ensureImagePullInstance). Distinct range from any other suite's probe
	// VPCs so a leaked resource from a failed run is easy to attribute.
	imagePullVPCCIDR          = "10.212.0.0/16"
	imagePullWorkerSubnetCIDR = "10.212.1.0/24"
	imagePullPubSubnetCIDR    = "10.212.2.0/24"

	// predastoreBaseDir mirrors scripts/storage-repro/probe.sh's BASE_PATH:
	// the whole on-disk tree predastore stores segments under on the node.
	// du -s --block-size=1 reports ALLOCATED blocks, not apparent size, so
	// the sparse badger value-log holes are not billed as real bytes (see
	// probe.sh's remote_allocated_bytes for the same reasoning).
	predastoreBaseDir = "/var/lib/spinifex/predastore"

	// imagePullTargetEnv selects which disk the pull's bytes land on. See
	// imagePullTarget's doc for the two accepted values and what each means.
	imagePullTargetEnv = "SPINIFEX_STORAGEGROWTH_TARGET"
	// imagePullTargetVolume is the original workload: a fresh, empty,
	// independently-created data volume. Default when the env var is unset.
	imagePullTargetVolume = "volume"
	// imagePullTargetRoot targets the instance's own root volume instead —
	// see imagePullTarget's doc.
	imagePullTargetRoot = "root"

	// imagePullRootDevice is the root volume's guest-visible device name in
	// root mode. service_impl.go's parseVolumeParams defaults to exactly this
	// string when BlockDeviceMappings[0].DeviceName is unset on a
	// RunInstances request, and DescribeInstances reports the same name back
	// for a virtio-blk-pci root volume (see
	// docs/development/archive/bugs/fix-blockdevicemappings-devicename.md),
	// so this one constant is used both to request the mapping and to
	// re-identify it afterward.
	imagePullRootDevice = "/dev/vda"

	// imagePullPullCyclesEnv selects how many pull-then-free cycles the guest
	// script drives — see imagePullPullCycles's doc for what a value above 1
	// changes and why it is root-mode only.
	imagePullPullCyclesEnv = "SPINIFEX_STORAGEGROWTH_PULL_CYCLES"
)

// imagePullGuestScript is written to the guest as /tmp/run-pull.sh and run
// under systemd-run (see launchImagePullUnit) so it survives the SSH session
// that launched it — the same reason single/datapath_test.go's HTTP server
// uses systemd-run rather than bare nohup: `nohup ... &` alone can race the
// launching session's exit and be reaped with it.
//
// It installs docker.io from the distro archive (no extra APT repo or GPG
// key needed, and the guest already has real internet egress via the
// workload VPC's IGW-routed public subnet — see ensureImagePullInstance),
// redirects storage onto the mounted volume BEFORE starting the daemons,
// then pulls the pinned image. The trap on EXIT fires on every exit path
// (success, any `exit N`, or an unhandled signal), so the exit-code marker
// file it writes is a single place the sampling loop can always eventually
// read a definitive status from.
//
// Two storage roots are redirected, not one. docker.io on current Ubuntu
// ships dockerd talking to a separate system containerd (containerd.service),
// and it is containerd — not dockerd — that extracts layers onto disk via its
// own "root" (default /var/lib/containerd), independently of dockerd's
// data-root in /etc/docker/daemon.json. A first version of this script only
// redirected dockerd's data-root and failed a live run with ENOSPC extracting
// a layer into /var/lib/containerd/tmpmounts on the AMI's small root disk,
// despite the 20GiB target volume sitting empty: data-root alone redirects
// where dockerd stores image references, not where the bytes actually land.
const imagePullGuestScript = `#!/bin/bash
set -u
trap 'echo $? > /tmp/run-pull.exitcode' EXIT

echo running > /tmp/run-pull.status

export DEBIAN_FRONTEND=noninteractive
apt-get update -y || exit 10
apt-get install -y docker.io || exit 11

systemctl stop docker.service docker.socket containerd.service 2>/dev/null || true
mkdir -p %[1]s/docker %[1]s/containerd /etc/docker /etc/containerd

containerd config default > /etc/containerd/config.toml || exit 12
sed -i 's#^root = .*#root = "%[1]s/containerd"#' /etc/containerd/config.toml || exit 13
grep -q "^root = \"%[1]s/containerd\"" /etc/containerd/config.toml || exit 14

cat > /etc/docker/daemon.json <<'JSON'
{"data-root": "%[1]s/docker"}
JSON

systemctl start containerd.service || exit 15
systemctl start docker.service || exit 16

echo pulling > /tmp/run-pull.status
docker pull %[2]s || exit 17

echo done > /tmp/run-pull.status
`

// imagePullGuestScriptRoot is the root-mode counterpart of imagePullGuestScript.
// It installs docker and pulls image the exact same way, but deliberately
// skips both redirects above: dockerd and containerd are left pointed at
// their stock locations (/var/lib/docker, /var/lib/containerd), so every byte
// the pull writes lands on the instance's own root disk rather than a mounted
// volume. That is the entire reason root mode exists — see TestImagePullGrowth's
// doc for why the root disk, not the data volume, is the surface the real
// incident filled. Same status/exitcode file protocol as
// imagePullGuestScript, so the existing polling/verification logic in
// imagePullUnitStatus reads it unchanged. Exit codes 10, 11 and 17 are kept
// aligned with imagePullGuestScript's numbering for the steps both scripts
// share; 12-16 (the redirect steps) are simply unused here.
const imagePullGuestScriptRoot = `#!/bin/bash
set -u
trap 'echo $? > /tmp/run-pull.exitcode' EXIT

echo running > /tmp/run-pull.status

export DEBIAN_FRONTEND=noninteractive
apt-get update -y || exit 10
apt-get install -y docker.io || exit 11

echo pulling > /tmp/run-pull.status
docker pull %[1]s || exit 17

echo done > /tmp/run-pull.status
`

// imagePullGuestScriptRootCycles is imagePullGuestScriptRoot's multi-cycle
// counterpart, used only when SPINIFEX_STORAGEGROWTH_PULL_CYCLES is set above
// 1 (see imagePullPullCycles). Same install steps and the same root-disk
// target (no redirect — see imagePullGuestScriptRoot's doc for why root mode
// skips it), but instead of a single `docker pull ... || exit 17`, it drives
// N pull-then-free attempts: on a disk too small for the image, an early
// cycle fills the disk and the pull fails, and the script then frees the
// guest-side space the same way containerd's own image GC would on a real
// cluster — drop the image reference and prune whatever partial/dangling
// content the failed pull left behind — before the next cycle retries.
// `docker system prune -af` is used for the free step rather than calling
// `ctr` (containerd's own CLI) directly: dockerd's containerd namespace
// ("moby" by default) and ctr's exact prune-subcommand surface are not
// something this change can verify are stable across the containerd version
// the docker.io apt package pulls in, whereas `docker system prune` is a
// documented, stable dockerd client command that — because dockerd asks
// containerd to drop the same underlying content — produces the same effect.
// The point of freeing the space at all is that the NEXT cycle's pull then
// rewrites the same freed guest block addresses: that address reuse is what
// makes an append-only backend store grow by ~a full disk each cycle while
// guest usage itself stays flat, which is the phenomenon this mode exists to
// reproduce: the retry loop a real EKS GPU worker hit when its root disk was
// too small for the image to ever fit.
//
// A per-cycle marker line is both echoed to stdout (journalctl -u
// mulga-imagepull captures it for a human reading the failure log) and
// appended to /tmp/run-pull.cycles, a stable file the Go side reads once
// after the loop settles rather than trying to tail a live unit's stdout
// (see readImagePullCycles). The current cycle number is also kept in
// /tmp/run-pull.cycle, overwritten every iteration, so the periodic sampling
// loop already running on the Go side can tag each sample with the cycle in
// flight when it was taken (see imagePullCurrentCycle).
//
// Unlike the single-pull scripts, an individual cycle's pull failure never
// collapses onto the script's own exit code: a full disk mid-cycle is the
// expected, measured phenomenon here, not a script fault, and a test that
// t.Fatalf'd on it would discard exactly the data this mode exists to
// collect. The EXIT trap's code — and therefore imagePullUnitStatus's
// failed/done result — reports only whether the N-cycle loop itself ran to
// completion; exit 10/11 (the apt-get steps) are the only failures that
// still abort the whole run, kept aligned with imagePullGuestScriptRoot's
// numbering for the steps the two scripts share.
const imagePullGuestScriptRootCycles = `#!/bin/bash
set -u
trap 'echo $? > /tmp/run-pull.exitcode' EXIT

echo running > /tmp/run-pull.status

export DEBIAN_FRONTEND=noninteractive
apt-get update -y || exit 10
apt-get install -y docker.io || exit 11

: > /tmp/run-pull.cycles

for i in $(seq 1 %[2]d); do
  echo $i > /tmp/run-pull.cycle
  echo "pulling (cycle $i/%[2]d)" > /tmp/run-pull.status

  docker pull %[1]s
  rc=$?
  used=$(df --output=used -B1 / | tail -n1)
  echo "CYCLE n=$i exit=$rc guest_used=$used elapsed=$SECONDS" | tee -a /tmp/run-pull.cycles

  if [ "$rc" -ne 0 ]; then
    docker rmi -f %[1]s >/dev/null 2>&1 || true
    docker system prune -af >/dev/null 2>&1 || true
  fi
done

echo done > /tmp/run-pull.status
`

// imagePullSample is one point in the time series captured throughout the
// pull, not just at the start and end. Correlating these four quantities at
// the SAME instant is the entire reason this workload measures host-side at
// all — TestNPassOverwrite measures nothing host-side, deferring entirely to
// external before/after snapshot tooling, because a before/after snapshot
// cannot show whether backend growth tracked guest writes linearly or spiked
// disproportionately partway through.
type imagePullSample struct {
	ElapsedSecs        float64 `json:"elapsed_secs"`
	GuestUsedBytes     int64   `json:"guest_used_bytes"`
	HostAllocatedBytes int64   `json:"host_allocated_bytes"`
	NbdkitRSSKiB       int64   `json:"nbdkit_rss_kib"`
	CheckpointBytes    int64   `json:"checkpoint_bytes"`
	// CheckpointLastModified is blocks.live.bin's S3 LastModified at this
	// sample, RFC3339Nano, empty if the object does not exist yet (no drain
	// has fired). SaveLiveCheckpointCtx always rewrites this one fixed key in
	// place, so a change in this timestamp between two samples is direct
	// evidence a drain happened in between — see imagePullWorkload's doc for
	// why this is the only drain signal available to this workload.
	CheckpointLastModified string `json:"checkpoint_last_modified,omitempty"`
	// CheckpointProbeErr is non-empty when the on-node HeadObject probe failed
	// for a reason other than the object legitimately not existing yet — an auth
	// rejection or a transport error. CheckpointBytes is forced to -1 alongside
	// it so a broken probe is recorded as void rather than masquerading as a
	// genuine zero reading: a dead instrument must never look like "no growth".
	CheckpointProbeErr string `json:"checkpoint_probe_err,omitempty"`
	// Cycle is the pull-then-free cycle in flight when this sample was taken
	// (see imagePullPullCycles), 1-indexed. Zero, and omitted, outside cycles
	// mode: the guest script never writes /tmp/run-pull.cycle at all there
	// (see imagePullGuestScriptRoot/imagePullGuestScript), so there is
	// nothing to attribute the sample to beyond the run as a whole.
	Cycle int `json:"cycle,omitempty"`
}

// imagePullCycle is one pull-then-free attempt's outcome in cycles mode (see
// imagePullPullCycles), parsed from one "CYCLE n=... exit=... guest_used=...
// elapsed=..." marker line imagePullGuestScriptRootCycles appends to
// /tmp/run-pull.cycles. Populated only when PullCycles > 1; empty otherwise.
type imagePullCycle struct {
	Cycle int `json:"cycle"`
	// ExitCode is the guest's raw docker-pull exit status for this cycle, not
	// a derived classification. It cannot be told apart from any other pull
	// failure (a registry timeout, a bad digest) any more than
	// imagePullWorkload.PullExitCode can in single-pull mode — see that
	// field's doc for why no enospc flag is reported instead of inferring one
	// from docker's stderr wording. A full disk is inferred, if at all, the
	// same way: GuestUsedBytes pinned near the volume's capacity across
	// several cycles is evidence, scored by the measurement step reading
	// this record, not by this workload fabricating a classification it
	// cannot actually observe.
	ExitCode int `json:"exit_code"`
	// GuestUsedBytes is the guest's own df reading taken by the script
	// immediately after this cycle's pull attempt settled, before the
	// free-and-retry step ran — the high-water mark this cycle reached.
	GuestUsedBytes int64 `json:"guest_used_bytes"`
	// ElapsedSecs is bash's own $SECONDS at the point this cycle's marker was
	// written, i.e. wall-clock time since the guest script itself started —
	// not since the Go-side sampling loop started, which begins a moment
	// earlier (see runImagePull).
	ElapsedSecs float64 `json:"elapsed_secs"`
	// PullCompleted is ExitCode == 0, recorded as its own field rather than
	// left for a consumer to re-derive, matching
	// imagePullWorkload.PullCompleted's reasoning.
	PullCompleted bool `json:"pull_completed"`
}

// imagePullDrainCountNote is a fixed explanation of a signal this workload
// cannot report, carried verbatim in every record — see
// imagePullWorkload.DrainCountAvailable for why it is recorded rather than
// omitted. A const rather than an inline literal because it describes
// viperblock's telemetry, not anything a given run did, so nothing about a
// run can change it.
const imagePullDrainCountNote = "viperblock/telemetry exposes no per-drain or checkpoint-write counter: " +
	"only backend-IO, WAL-op, and cache-lookup metrics are recorded, none keyed to " +
	"distinguish a checkpoint write from an ordinary chunk write. A precise count needs " +
	"either a viperblock-side counter (out of scope for this workload) or predastore " +
	"access-log parsing for PUT .../checkpoints/blocks.live.bin, which requires debug " +
	"logging predastore does not run with by default. Each sample's " +
	"checkpoint_last_modified is the available mitigation: since SaveLiveCheckpointCtx " +
	"always rewrites the fixed key <volume>/checkpoints/blocks.live.bin in place, a " +
	"change in that timestamp between two samples is evidence a drain occurred between " +
	"them, and lower-bounds the drain count at this sample interval."

// imagePullWorkload records what one invocation of the image-pull workload
// did, plus the sampled time series, for an external measurement step to
// read. Mirrors npassWorkload's contract (same result-path env var, same
// guest_used_bytes/volume_id/retained field names) with fields npassWorkload
// has no use for (passes, extent, per-pass sha) replaced by fields specific
// to a single, uncontrolled-pattern write.
//
// npass rewrites one fixed extent every pass, which pins the block map's size
// and makes the per-pass backend cost provably linear in N — useful for
// isolating the unreferenced-live leak, but structurally incapable of testing
// whether checkpoint-rewrite cost is quadratic in bytes written, because the
// block map it reserialises on every drain never grows. This workload writes
// DISTINCT, monotonically growing data instead (an image pull neither
// controls nor repeats its own byte pattern), so it is the first shape able
// to answer that question: SaveLiveCheckpointCtx reserialises the entire
// block map on every drain, so if checkpoint cost scales with block-map size,
// the samples here should show host_allocated_bytes and checkpoint_bytes
// growing super-linearly against guest_used_bytes as the pull proceeds, not
// just being offset from it by a constant multiple.
type imagePullWorkload struct {
	Image string `json:"image"`
	// Target is imagePullTargetVolume or imagePullTargetRoot — which disk the
	// pull actually landed on. See imagePullTarget's doc for what each means;
	// an external measurement step needs this to know whether VolumeID names
	// a freshly-created data volume or the instance's own root volume.
	Target    string `json:"target"`
	VolumeGiB int    `json:"volume_gib"`
	VolumeID  string `json:"volume_id"`
	// Retained reports whether the volume outlived the test process, exactly
	// as npassWorkload.Retained does — a measurement taken after DeleteVolume
	// purges the volume's objects sees no leak whether or not one occurred.
	Retained           bool              `json:"retained"`
	GuestUsedBytes     int64             `json:"guest_used_bytes"`
	Samples            []imagePullSample `json:"samples"`
	SampleIntervalSecs int               `json:"sample_interval_secs"`
	// PullDurationSecs is time to completion, or time to failure when
	// PullCompleted is false.
	PullDurationSecs float64 `json:"pull_duration_secs"`
	// PullCompleted reports whether the pull ran to completion. False is a
	// recorded outcome, not a void run: an image too large for its volume
	// fills the disk, and the runtime's evict-and-re-pull churn that follows
	// is the uncontrolled rewrite pattern this workload exists to reproduce,
	// so the samples leading up to that failure ARE the measurement. A record
	// exists at all only when the run captured real samples and a usable
	// denominator — writeImagePullResult owns that boundary, and its absence
	// is how a consumer learns a point measured nothing.
	PullCompleted bool `json:"pull_completed"`
	// PullStatus and PullExitCode are the guest script's own status marker and
	// exit code as of the last poll ("pulling" + "17" for a failed pull,
	// "done" + "0" for a clean one). Recorded so a consumer can tell an
	// outright pull failure from a run that merely hit imagePullMaxDuration
	// with the pull still making progress; the latter leaves the exit code
	// empty, since the guest script's trap never fired.
	//
	// Neither field distinguishes a full disk from any other pull failure.
	// The guest script collapses every docker pull error onto exit 17 (see
	// imagePullGuestScript), so ENOSPC, a registry timeout and a bad digest
	// are indistinguishable here, and no ENOSPC flag is reported rather than
	// inferring one from docker's stderr wording, which is not a stable
	// contract. The evidence a consumer needs is already in the record
	// without it: guest_used_bytes pinned at volume_gib across the tail of
	// samples is a full disk, and scoring that is the measurement step's job,
	// not this workload's — the same division of labour the package doc sets
	// out, and the same reasoning as DrainCountAvailable below.
	PullStatus   string `json:"pull_status,omitempty"`
	PullExitCode string `json:"pull_exit_code,omitempty"`
	// PullCycles is the configured SPINIFEX_STORAGEGROWTH_PULL_CYCLES value
	// this run used — 1 for the original single-pull behaviour. Recorded
	// alongside Cycles so a consumer can tell "ran once, that's the whole
	// record" (PullCycles==1, Cycles empty) apart from "configured for N
	// cycles but Cycles came back short" (a run that hit
	// imagePullMaxDuration, or an infra fault, partway through the loop).
	PullCycles int `json:"pull_cycles"`
	// Cycles is cycles mode's per-cycle breakdown, one entry per pull-then-
	// free attempt imagePullGuestScriptRootCycles's marker file recorded.
	// Empty when PullCycles == 1: single-pull mode's guest script never
	// writes /tmp/run-pull.cycles at all, so there is nothing to read.
	Cycles []imagePullCycle `json:"cycles,omitempty"`
	// DrainCountAvailable is always false today. Recorded explicitly, rather
	// than omitted, so a measurement step reading this file learns that
	// without needing this test's source: viperblock/telemetry/metrics.go
	// exposes backend-IO, WAL-op, and cache-lookup counters, none keyed to
	// distinguish a checkpoint write from an ordinary chunk write, so no
	// drain/checkpoint-write count is reachable from outside viperblock
	// itself. Per the workload's brief, no such counter was added — that
	// repo is owned by another agent. A precise count needs either a
	// viperblock-side counter, or predastore access-log parsing for
	// `PUT .../checkpoints/blocks.live.bin`, which requires debug logging
	// predastore does not run with by default. CheckpointLastModified in each
	// sample is the mitigation available without either: a change between
	// two samples lower-bounds the drain count at this sample interval.
	DrainCountAvailable bool   `json:"drain_count_available"`
	DrainCountNote      string `json:"drain_count_note"`
}

// TestImagePullGrowth pulls a public, digest-pinned container image onto a
// guest disk — sampling guest/host/backend state throughout — then reports
// what it did for an external measurement step to score. See
// imagePullWorkload's doc for why this shape, not npass's, is the one that
// can test whether checkpoint-rewrite cost is quadratic in bytes written.
//
// SPINIFEX_STORAGEGROWTH_TARGET selects which disk the bytes land on; see
// imagePullTarget's doc. Default imagePullTargetVolume mounts a dedicated,
// freshly-created data volume and redirects the guest runtime onto it — the
// original shape, unchanged by root mode's addition. imagePullTargetRoot
// instead lets the pull land on the instance's own root volume, which, unlike
// the freshly-created data volume, is cloned from an AMI snapshot.
//
// That clone/snapshot ancestry was root mode's original hypothesis, and it is
// refuted: measured against the same image set, the two targets amplify
// identically (live chunk bytes grow at 1.0144x guest bytes on root against
// 1.0128x on volume — 0.16% apart), so ancestry costs nothing. Root mode earns
// its place for a different reason: a real ~13GB-pull-to-~150GB-usage report on
// an EKS GPU worker filled the node's ROOT disk, and only root mode can size
// that disk small enough for the pull to run out of space, which is the
// condition the growth actually needs.
//
// Unlike npass, this instance does not use the shared default VPC: guest SSH
// and the pull itself both need real internet egress, and
// harness.InstancePublicSSHHost hard-fails on an instance with no public IP,
// so ensureImagePullInstance builds its own dedicated topology.
func TestImagePullGrowth(t *testing.T) {
	fix := requireStorageGrowthFixture(t)
	harness.Phase(t, "Storage Growth — Container Image Pull (checkpoint-rewrite quadratic-cost probe)")

	image := imagePullImage()
	sampleInterval := imagePullSampleInterval()
	retain := npassRetainVolume()
	target := imagePullTarget()
	cycles := imagePullPullCycles()
	// Cycles mode is only implemented for the root target: the churn it drives
	// needs a guest script that leaves docker's storage on the root disk, and
	// imagePullGuestScriptRootCycles is the only such script. Nothing about the
	// leak it measures is root-specific — an append-only store grows by the
	// bytes rewritten regardless of which volume takes them — so a volume-mode
	// cycles script would be a valid control if one is ever written. Fail loud
	// rather than silently running a single pull under a cycles-mode config.
	if cycles > 1 && target != imagePullTargetRoot {
		t.Fatalf("%s=%d requires %s=%s: cycles mode is implemented only by the root-target guest script",
			imagePullPullCyclesEnv, cycles, imagePullTargetEnv, imagePullTargetRoot)
	}

	var (
		instanceID string
		tgt        harness.SSHTarget
		volID      string
		volGiB     int
	)
	switch target {
	case imagePullTargetRoot:
		volGiB = imagePullRootGiB()
		var inst *ec2.Instance
		instanceID, tgt, inst = ensureImagePullInstance(t, fix, func(ami, instType, keyName, subnetID, sgID string) string {
			return launchImagePullRootInstance(t, fix, ami, instType, keyName, subnetID, sgID, volGiB, retain)
		})
		volID = imagePullRootVolumeID(t, inst, retain)
		// RunInstances created this volume implicitly, so nothing has tagged
		// it: createImagePullVolume tags only the data volume it creates
		// itself, and root mode bypasses harness.EnsureInstance's tagging.
		// It can only be tagged once its id is known, hence here rather than
		// in launchImagePullRootInstance.
		tagImagePullRunResource(t, fix, volID, retain,
			"the teardown sweep and the storage-repro reclaim pass both discover volumes only by this tag, and retain "+
				"sets DeleteOnTermination=false so nothing else reclaims this one — untagged it leaks permanently, "+
				"holding its chunks live on the backing store and skewing every later run's baseline")
	default:
		volGiB = imagePullVolumeGiB()
		instanceID, tgt, _ = ensureImagePullInstance(t, fix, func(ami, instType, keyName, subnetID, sgID string) string {
			return harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
				AMIID:        ami,
				InstanceType: instType,
				KeyName:      keyName,
				SubnetID:     subnetID,
				SGID:         sgID,
			})
		})
		az := harness.DiscoverDefaultAZ(t, fix.Harness)
		volID = createImagePullVolume(t, fix, az, volGiB, retain)
	}

	harness.Detail(t, "image", image, "target", target, "volume_gib", volGiB, "instance", instanceID,
		"volume", volID, "retain_volume", retain, "sample_interval_secs", sampleInterval, "pull_cycles", cycles)

	// The record carries everything already known about the run, and
	// runImagePull fills the measured fields into it in place rather than
	// returning them, so that the write below still sees whatever was captured
	// when the pull fails. A failing pull is not a harness fault whose data can
	// be discarded: an image that does not fit its volume is the phenomenon
	// under test, and t.Fatalf unwinds via runtime.Goexit, which never reaches
	// a statement following the call that failed.
	w := &imagePullWorkload{
		Image:               image,
		Target:              target,
		VolumeGiB:           volGiB,
		VolumeID:            volID,
		Retained:            retain,
		SampleIntervalSecs:  sampleInterval,
		PullCycles:          cycles,
		DrainCountAvailable: false,
		DrainCountNote:      imagePullDrainCountNote,
	}
	// defer rather than t.Cleanup: runtime.Goexit runs both, but a test's own
	// defers run during its unwind, strictly before any cleanup. The record
	// therefore lands while the topology is still intact, ahead of the
	// terminate-and-wait cleanup ensureImagePullInstance registers — so a run
	// killed during a slow teardown has already written its data.
	defer writeImagePullResult(t, w)

	runImagePull(t, fix, tgt, instanceID, volID, image, volGiB, sampleInterval, target, cycles, w)

	harness.Detail(t, "guest_used_bytes", w.GuestUsedBytes, "samples", len(w.Samples),
		"pull_duration_secs", fmt.Sprintf("%.1f", w.PullDurationSecs), "cycles_recorded", len(w.Cycles))
}

// ensureImagePullInstance launches a dedicated VPC/subnet topology for this
// workload rather than reusing ensureWorkloadInstance's shared default VPC.
//
// It builds the topology via harness.CreateWorkerEgress — the same helper
// the eks and gpu suites use for private-worker egress — even though this
// instance does not sit in the private worker subnet CreateWorkerEgress is
// designed for: it launches directly in CreateWorkerEgress's returned public
// subnet instead (after flipping MapPublicIpOnLaunch, which CreateSubnet
// defaults false), getting a real public IP and direct IGW egress — the same
// mechanism every default-VPC suite already relies on for guest SSH — without
// the extra NAT-Gateway hop CreateWorkerEgress's own private worker subnet
// would add for no benefit here. The worker subnet CreateWorkerEgress
// requires as an argument is otherwise unused by this suite; the NAT Gateway
// and EIP it provisions are therefore pure topology overhead for this
// particular instance; a leaner build could skip CreateWorkerEgress entirely
// in favour of a single MapPublicIpOnLaunch + IGW + route table, as
// tests/e2e/iam/imds_test.go's imdsAttachInternet does.
//
// The instance launch itself is factored out into launch, which receives the
// resolved AMI/instance-type/key/subnet/SG and returns the new instance ID.
// volume mode's launch calls harness.EnsureInstance unchanged; root mode's
// needs BlockDeviceMappings on the RunInstances request to size and retain
// the root volume, which harness.InstanceSpec has no field for (and this
// suite never edits tests/e2e/harness — see the package doc), so it calls
// fix.AWS.EC2.RunInstances directly instead (launchImagePullRootInstance).
func ensureImagePullInstance(t *testing.T, fix *Fixture, launch func(ami, instType, keyName, subnetID, sgID string) string) (instanceID string, tgt harness.SSHTarget, inst *ec2.Instance) {
	t.Helper()
	instType, arch := imagePullInstanceType(t, fix)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))

	vpcID := harness.CreateVPC(t, fix.AWS, imagePullVPCCIDR)
	t.Cleanup(func() { harness.DeleteVPC(t, fix.AWS, vpcID) })
	workerSubnetID := harness.CreateSubnet(t, fix.AWS, vpcID, imagePullWorkerSubnetCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, fix.AWS, workerSubnetID) })

	eg := harness.CreateWorkerEgress(t, fix.AWS, vpcID, workerSubnetID, imagePullPubSubnetCIDR)
	t.Cleanup(func() { harness.DeleteWorkerEgress(t, fix.AWS, eg) })

	// CreateSubnet defaults MapPublicIpOnLaunch to false; the workload
	// instance needs a real public IP (InstancePublicSSHHost hard-fails
	// without one), so it is flipped on the public subnet CreateWorkerEgress
	// already routed straight to the IGW.
	_, err := fix.AWS.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(eg.PubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoErrorf(t, err, "modify-subnet-attribute MapPublicIpOnLaunch on %s", eg.PubSubnetID)

	sgID := imagePullDefaultSG(t, fix.AWS, vpcID)
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)

	instanceID = launch(ami, instType, keyName, eg.PubSubnetID, sgID)
	// The launch path's own cleanup (harness.EnsureInstance, for volume mode)
	// issues TerminateInstances without waiting for the terminal state, which
	// races the network cleanups above: a live run hit exactly this —
	// DeleteWorkerEgress tried to delete eg.PubSubnetID while the instance's
	// ENI was still attached (still "shutting-down"), failing with
	// DependencyViolation. t.Cleanup is LIFO, so registering this wait here —
	// after launch — makes it run BEFORE any terminate cleanup launch itself
	// registered and every network teardown below it, guaranteeing the ENI is
	// gone first. Terminating again here is idempotent with whatever launch's
	// own cleanup does later, and is the ONLY terminate cleanup for root
	// mode's direct RunInstances call, which registers none of its own.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "terminated")
	})
	inst = harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d", host, port)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}, inst
}

// imagePullDefaultSG returns the auto-created default security group for
// vpcID. Mirrors tests/e2e/iam/imds_test.go's imdsDefaultSG, which is
// unexported in another package and therefore not reachable from here.
func imagePullDefaultSG(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoErrorf(t, err, "describe default SG for %s", vpcID)
	require.Lenf(t, out.SecurityGroups, 1, "vpc %s default SG", vpcID)
	return aws.StringValue(out.SecurityGroups[0].GroupId)
}

// imagePullInstanceType selects the guest for the image-pull repro. The
// synthetic npass suite uses a nano (512MB) to pack many suites onto one node,
// but a real multi-GB image pull onto 512MB OOM-thrashes containerd long before
// any disk-driven ENOSPC, and 512MB against a concurrent-write pull is exactly
// the guest-memory-pressure regime that produces the separate siv-482 lost-write
// corruption — conflating two faults in one measurement. This mode instead picks
// an instance with headroom well above the pull's working set, so the run
// measures the storage leak and not memory pressure.
//
// SPINIFEX_STORAGEGROWTH_INSTANCE_TYPE overrides the choice outright.
// Otherwise the smallest advertised type at or above SPINIFEX_STORAGEGROWTH_MIN_MEM_MIB
// (default 16384, matching the ~16GB EKS GPU worker the incident was reported on)
// is used — smallest-above-floor so it gets headroom without grabbing a type the
// node cannot seat.
func imagePullInstanceType(t *testing.T, fix *Fixture) (instanceType, arch string) {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "DescribeInstanceTypes")

	archOf := func(it *ec2.InstanceTypeInfo) string {
		if it.ProcessorInfo == nil || len(it.ProcessorInfo.SupportedArchitectures) == 0 {
			return ""
		}
		return aws.StringValue(it.ProcessorInfo.SupportedArchitectures[0])
	}

	if override := os.Getenv("SPINIFEX_STORAGEGROWTH_INSTANCE_TYPE"); override != "" {
		for _, it := range out.InstanceTypes {
			if aws.StringValue(it.InstanceType) == override {
				a := archOf(it)
				require.NotEmptyf(t, a, "instance type %s advertises no architecture", override)
				return override, a
			}
		}
		t.Fatalf("SPINIFEX_STORAGEGROWTH_INSTANCE_TYPE=%s not advertised by this cluster", override)
	}

	minMemMiB := int64(16384)
	if v := os.Getenv("SPINIFEX_STORAGEGROWTH_MIN_MEM_MIB"); v != "" {
		parsed, perr := strconv.ParseInt(v, 10, 64)
		require.NoErrorf(t, perr, "parse SPINIFEX_STORAGEGROWTH_MIN_MEM_MIB=%q", v)
		minMemMiB = parsed
	}

	var best *ec2.InstanceTypeInfo
	var bestMem int64
	for _, it := range out.InstanceTypes {
		if it.MemoryInfo == nil || it.MemoryInfo.SizeInMiB == nil || archOf(it) == "" {
			continue
		}
		mem := aws.Int64Value(it.MemoryInfo.SizeInMiB)
		if mem < minMemMiB {
			continue
		}
		if best == nil || mem < bestMem {
			best, bestMem = it, mem
		}
	}
	require.NotNilf(t, best, "no advertised instance type has >= %d MiB memory", minMemMiB)
	return aws.StringValue(best.InstanceType), archOf(best)
}

// createImagePullVolume creates the volume the pull runs against, sized by
// sizeGiB (unlike npass's fixed 1 GiB extent, this workload's footprint is
// the whole point of the measurement, so its size must be large enough to
// hold a real pulled image).
//
// Mirrors createWorkloadVolume's reasoning for not using harness.EnsureVolume
// (memoized on (az, size), and its cleanup unconditionally purges every
// object under the volume's prefix — exactly the leak evidence a retained
// run needs to keep).
func createImagePullVolume(t *testing.T, fix *Fixture, az string, sizeGiB int, retain bool) string {
	t.Helper()

	// e2e:allow-create
	out, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(int64(sizeGiB)),
	})
	require.NoError(t, err, "CreateVolume in %s", az)
	volID := aws.StringValue(out.VolumeId)
	require.NotEmpty(t, volID, "CreateVolume returned no volume id")

	harness.WaitForVolumeState(t, fix.AWS, volID, "available")

	if runID := os.Getenv(harness.RunIDEnv); runID != "" {
		if _, terr := fix.AWS.EC2.CreateTags(&ec2.CreateTagsInput{
			Resources: []*string{aws.String(volID)},
			Tags:      []*ec2.Tag{{Key: aws.String(npassRunTagKey), Value: aws.String(runID)}},
		}); terr != nil {
			t.Logf("storagegrowth: tagging %s with %s=%s: %v", volID, npassRunTagKey, runID, terr)
		}
	}

	if retain {
		t.Logf("storagegrowth: retaining %s past process exit so the backing store can be measured with its objects still live", volID)
		return volID
	}
	t.Cleanup(func() {
		if _, derr := fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(volID)}); derr != nil {
			t.Logf("storagegrowth: deleting %s: %v", volID, derr)
		}
	})
	return volID
}

// launchImagePullRootInstance launches the instance with an explicitly sized
// root volume (BlockDeviceMappings[0]) rather than going through
// harness.EnsureInstance, whose InstanceSpec has no field for
// BlockDeviceMappings — see service_impl.go's parseVolumeParams, which reads
// BlockDeviceMappings[0].Ebs.VolumeSize/DeleteOnTermination and
// BlockDeviceMappings[0].DeviceName straight off the RunInstances request.
// The AMI's default root disk is sized for booting, not for holding a
// multi-GB image pull; ENOSPC on the root disk is exactly the failure this
// mode exists to rule in or out, so undersizing it here would corrupt the
// measurement rather than merely failing the test.
//
// DeleteOnTermination is always set explicitly — never left to default — so
// root mode never depends on parseVolumeParams' current default (true)
// continuing to hold. When retain is set it is turned off: the root volume
// otherwise dies with the instance at the end of the test (see
// ensureImagePullInstance's terminate cleanup), destroying the very volume
// the post-run scan needs to read.
//
// Not memoized like harness.EnsureInstance: every call here wants its own
// freshly, explicitly sized root volume, so sharing one across differently
// configured calls would be wrong, not just wasteful.
func launchImagePullRootInstance(t *testing.T, fix *Fixture, ami, instType, keyName, subnetID, sgID string, rootGiB int, retain bool) string {
	t.Helper()

	// e2e:allow-create
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(ami),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(subnetID),
		SecurityGroupIds: []*string{aws.String(sgID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String(imagePullRootDevice),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize:          aws.Int64(int64(rootGiB)),
					DeleteOnTermination: aws.Bool(!retain),
				},
			},
		},
	})
	require.NoError(t, err, "RunInstances with sized root volume")
	require.NotEmptyf(t, out.Instances, "RunInstances returned 0 instances")
	instanceID := aws.StringValue(out.Instances[0].InstanceId)
	require.NotEmpty(t, instanceID, "RunInstances returned no instance id")

	// The instance carries the run tag for the same reason the root volume
	// does, plus one twist that makes it load-bearing rather than a nicety
	// under retain. SweepRunResources sweeps instances before volumes and
	// finds each solely by tag, and DeleteVolume refuses a volume still held
	// by a live instance (VolumeInUse). So an untagged instance strands even a
	// correctly tagged root volume: the sweep never terminates the holder, and
	// the volume delete then fails behind it. Both tags are required for the
	// sweep to reclaim a retained root volume, so both are fatal under retain.
	tagImagePullRunResource(t, fix, instanceID, retain,
		"the teardown sweep finds instances only by this tag and must terminate this one before it can delete the "+
			"root volume attached to it (DeleteVolume refuses a volume held by a live instance), so an untagged "+
			"instance leaves even a correctly tagged root volume unreclaimable behind it")

	return instanceID
}

// tagImagePullRunResource stamps the run tag on a resource root mode created
// outside the harness's own Ensure* helpers, which is the only thing that
// makes it reclaimable: harness.SweepRunResources discovers every resource
// class solely by DescribeX filtered on tag:e2e:run, and the storage-repro
// reclaim pass matches the same tag. Untagged, neither can ever see it.
//
// whyFatal names what specifically breaks when the tag does not land, and is
// only used under retain, where the failure is fatal. That asymmetry is the
// point: without retain, DeleteOnTermination=true and the terminate cleanup in
// ensureImagePullInstance reclaim everything on both the pass and the t.Fatal
// path, so the tag is only a backstop against a process that dies without
// running cleanups at all — worth logging, not worth failing a measurement
// over. With retain, the tag is the *sole* reclamation path, and an unreclaimed
// volume is not a disk-space nuisance but a measurement fault: its chunks stay
// live on the backing store, so every later run starts from a dirtier baseline
// than the one before, and this harness exists to measure a ratio against an
// identical baseline. A run that cannot guarantee its own cleanup must fail
// loudly rather than silently poison the next run's denominator.
//
// The key is npassRunTagKey, this package's restatement of the harness's
// unexported runTagKey — the harness exports no tagging helper at all
// (tagRunResources is likewise unexported), and this suite never edits
// tests/e2e/harness (see the package doc in main_test.go).
func tagImagePullRunResource(t *testing.T, fix *Fixture, resourceID string, retain bool, whyFatal string) {
	t.Helper()
	// No run id means no sweep is coming, so there is no tag for one to match
	// on and nothing to fail about — reclaiming a retained resource is then
	// the operator's own business, as createImagePullVolume's retain log says.
	runID := os.Getenv(harness.RunIDEnv)
	if runID == "" {
		return
	}

	_, err := fix.AWS.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(resourceID)},
		Tags:      []*ec2.Tag{{Key: aws.String(npassRunTagKey), Value: aws.String(runID)}},
	})
	if retain {
		require.NoErrorf(t, err, "tagging %s with %s=%s, which retain makes mandatory: %s",
			resourceID, npassRunTagKey, runID, whyFatal)
		return
	}
	if err != nil {
		t.Logf("storagegrowth: tagging %s with %s=%s: %v", resourceID, npassRunTagKey, runID, err)
	}
}

// imagePullRootVolumeID resolves root mode's VolumeID from inst's own
// BlockDeviceMappings — the same DescribeInstances-sourced *ec2.Instance
// ensureImagePullInstance already waited for at "running" — rather than the
// RunInstances response, because that is what item 6 of this mode's contract
// requires: an external tool scans the reported volume by ID afterward, so a
// placeholder or guessed ID would make it silently scan nothing.
//
// Every failure path here is fatal, not logged-and-continued: this is a
// measurement harness, and a root-mode run that cannot prove which volume it
// wrote to, or that the volume will survive to be scanned, must not produce a
// result record for something else to trust.
func imagePullRootVolumeID(t *testing.T, inst *ec2.Instance, retain bool) string {
	t.Helper()
	instanceID := aws.StringValue(inst.InstanceId)
	require.NotEmptyf(t, inst.BlockDeviceMappings, "instance %s: DescribeInstances reported no BlockDeviceMappings — root mode cannot identify which volume the pull landed on", instanceID)

	for _, bdm := range inst.BlockDeviceMappings {
		if aws.StringValue(bdm.DeviceName) != imagePullRootDevice {
			continue
		}
		require.NotNilf(t, bdm.Ebs, "instance %s: root device %s BlockDeviceMapping has no Ebs entry", instanceID, imagePullRootDevice)
		volID := aws.StringValue(bdm.Ebs.VolumeId)
		require.NotEmptyf(t, volID, "instance %s: root device %s Ebs entry has no VolumeId", instanceID, imagePullRootDevice)
		if retain {
			require.Falsef(t, aws.BoolValue(bdm.Ebs.DeleteOnTermination),
				"instance %s: retain requested but root volume %s still reports DeleteOnTermination=true — it would be destroyed at instance termination before the post-run scan can read it",
				instanceID, volID)
		}
		return volID
	}
	t.Fatalf("instance %s: no BlockDeviceMapping for root device %s among %d mapping(s) — root volume id is not discoverable", instanceID, imagePullRootDevice, len(inst.BlockDeviceMappings))
	return ""
}

// runImagePull, in volume mode (target == imagePullTargetVolume), attaches
// volID, formats and mounts it persistently, redirects the guest's container
// runtime onto that mount, pulls image in the background, and samples
// guest/host/backend state on an interval until the pull completes. In root
// mode (target == imagePullTargetRoot) volID already names the instance's own
// root volume (see imagePullRootVolumeID): there is nothing to attach, format,
// or redirect, and the guest samples its own root filesystem instead of the
// mount.
//
// The measured fields land in w as they are captured, rather than in a record
// returned at the end, so a pull that fails partway still leaves its samples
// with the caller — see TestImagePullGrowth for why that data must survive.
//
// cycles > 1 (root mode only — see TestImagePullGrowth's guard) drives
// imagePullGuestScriptRootCycles instead of the single-pull scripts; the
// sampling loop below tags each sample with the cycle in flight
// (imagePullCurrentCycle), and once the guest script's own loop settles,
// readImagePullCycles fills w.Cycles/w.PullCompleted from its marker file.
func runImagePull(t *testing.T, fix *Fixture, tgt harness.SSHTarget, instanceID, volID, image string, volGiB, sampleIntervalSecs int, target string, cycles int, w *imagePullWorkload) {
	t.Helper()

	// dfPath is where the guest-side denominator is measured. Volume mode
	// samples the mounted, redirected volume; root mode has no mount to
	// sample and instead reads the instance's own root filesystem directly.
	dfPath := imagePullMount
	if target == imagePullTargetRoot {
		dfPath = "/"
		harness.Step(t, "pulling %s directly onto instance %s's root volume %s (no attach, no redirect)", image, instanceID, volID)
	} else {
		harness.Step(t, "attaching %s (%dGiB) and pulling %s onto it", volID, volGiB, image)

		before := harness.GuestDiskSet(t, tgt)
		harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, imagePullDevice)
		dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)
		defer harness.DetachVolumeWait(t, fix.AWS, volID)

		mkfsScript := fmt.Sprintf(
			"sudo mkfs.ext4 -F -L %s /dev/%s && sudo mkdir -p %s && sudo mount /dev/%s %s",
			imagePullVolumeLabel, dev, imagePullMount, dev, imagePullMount)
		out, err := harness.GuestExec(tgt, mkfsScript)
		require.NoErrorf(t, err, "mkfs+mount /dev/%s: %s", dev, strings.TrimSpace(out))
		defer func() {
			// Stop the daemons first: both hold files open under the mount (it is
			// their redirected storage root), so an unmount attempted while either
			// is still running fails "target is busy" — observed on a live run
			// after a failed pull left docker/containerd running.
			_, _ = harness.GuestExec(tgt, "sudo systemctl stop docker.service docker.socket containerd.service 2>/dev/null")
			if uout, uerr := harness.GuestExec(tgt, fmt.Sprintf("sudo umount %s", imagePullMount)); uerr != nil {
				t.Logf("storagegrowth: unmount %s: %v: %s", imagePullMount, uerr, strings.TrimSpace(uout))
			}
		}()
	}

	launchImagePullUnit(t, tgt, image, target, cycles)

	ssh := harness.NewPeerSSH()

	w.Samples = make([]imagePullSample, 0, 64)
	start := time.Now()
	interval := time.Duration(sampleIntervalSecs) * time.Second

	// pullErr holds why the pull did not complete, deferring the failure until
	// after the final sample below. Failing inside the loop instead would skip
	// that sample and lose the settled post-failure state, which on a disk that
	// filled up is the most informative point in the series.
	var pullErr string
	for {
		s := captureImagePullSample(t, tgt, ssh, fix.Env.WANHost, volID, start, dfPath)
		if cycles > 1 {
			s.Cycle = imagePullCurrentCycle(t, tgt)
		}
		w.Samples = append(w.Samples, s)
		harness.Detail(t, "sample_elapsed_s", fmt.Sprintf("%.1f", s.ElapsedSecs),
			"guest_used", s.GuestUsedBytes, "host_allocated", s.HostAllocatedBytes,
			"nbdkit_rss_kib", s.NbdkitRSSKiB, "checkpoint_bytes", s.CheckpointBytes, "cycle", s.Cycle)

		status, exitCode, done, failed := imagePullUnitStatus(t, tgt)
		w.PullStatus, w.PullExitCode = status, exitCode
		if failed {
			pullErr = fmt.Sprintf("image pull unit failed (status=%s exitcode=%s)", status, exitCode)
			break
		}
		if done {
			// In cycles mode, "done" only means imagePullGuestScriptRootCycles's
			// own N-cycle loop ran to completion — an individual cycle's pull
			// failure never reaches this exit code (see that script's doc), so
			// it says nothing about whether the FINAL pull attempt actually
			// succeeded. readImagePullCycles below sets PullCompleted from the
			// last recorded cycle instead, once its marker file can be read.
			if cycles <= 1 {
				w.PullCompleted = true
			}
			break
		}
		if time.Since(start) > imagePullMaxDuration {
			pullErr = fmt.Sprintf("image pull did not complete within %s (status=%s)", imagePullMaxDuration, status)
			break
		}
		time.Sleep(interval)
	}
	// One final sample right after the pull settles, so the last data point
	// reflects settled state rather than whatever the last periodic tick
	// happened to catch mid-write. Taken on the failure path too: after a disk
	// fills, this is the reading that pins guest_used_bytes at the volume's
	// capacity, which is what tells a consumer the pull ran out of room.
	finalSample := captureImagePullSample(t, tgt, ssh, fix.Env.WANHost, volID, start, dfPath)
	if cycles > 1 {
		finalSample.Cycle = imagePullCurrentCycle(t, tgt)
	}
	w.Samples = append(w.Samples, finalSample)
	w.PullDurationSecs = time.Since(start).Seconds()
	w.GuestUsedBytes = w.Samples[len(w.Samples)-1].GuestUsedBytes

	// Cycles mode's per-cycle breakdown is read once here, after the guest
	// script's loop has settled (or the run gave up waiting on it above) —
	// not polled every tick like the status/exitcode marker files, since
	// /tmp/run-pull.cycles is only complete once the loop that appends to it
	// has stopped running.
	if cycles > 1 {
		readImagePullCycles(t, tgt, w)
	}

	// A cycle-mode pull failure never reaches pullErr (see the "done" branch
	// above): imagePullGuestScriptRootCycles's own script-level exit code only
	// reflects an infra fault (apt-get) or the run exceeding
	// imagePullMaxDuration, both of which remain fatal here exactly as in
	// single-pull mode. An individual cycle ENOSPCing is not a harness fault —
	// it is the measurement — so it never sets pullErr in the first place.
	if pullErr != "" {
		logOut, _ := harness.GuestExec(tgt, "journalctl -u mulga-imagepull --no-pager 2>/dev/null | tail -n 100")
		t.Fatalf("%s:\n%s", pullErr, logOut)
	}

	require.Positivef(t, w.GuestUsedBytes, "guest reported 0 used bytes on %s after pulling %s — the denominator every downstream ratio divides by is unusable", dfPath, image)

	// verifyImagePullLandedOnVolume asserts the mounted volume's own runtime
	// state directories exist, which only applies to volume mode — root mode
	// never mounts anything at imagePullMount for it to inspect.
	if target != imagePullTargetRoot {
		verifyImagePullLandedOnVolume(t, tgt)
	}
}

// launchImagePullUnit writes the target-appropriate guest script to the guest
// as /tmp/run-pull.sh and starts it as a detached systemd transient unit.
// Volume mode uses imagePullGuestScript (redirects onto the mount); root mode
// uses imagePullGuestScriptRoot (no redirect, writes land on the root disk
// directly), or imagePullGuestScriptRootCycles instead when cycles > 1 (see
// TestImagePullGrowth's guard: cycles mode is root-only). The script is
// base64-encoded rather than heredoc'd directly into the SSH command:
// imagePullGuestScript contains its own heredoc (for /etc/docker/daemon.json)
// and shell metacharacters, and base64 avoids having to reason about two
// layers of shell quoting (the local ssh command string and the remote shell
// it runs) nesting correctly.
func launchImagePullUnit(t *testing.T, tgt harness.SSHTarget, image, target string, cycles int) {
	t.Helper()
	script := fmt.Sprintf(imagePullGuestScript, imagePullMount, image)
	switch {
	case target == imagePullTargetRoot && cycles > 1:
		script = fmt.Sprintf(imagePullGuestScriptRootCycles, image, cycles)
	case target == imagePullTargetRoot:
		script = fmt.Sprintf(imagePullGuestScriptRoot, image)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(script))

	writeCmd := fmt.Sprintf(
		"echo %s | base64 -d | sudo tee /tmp/run-pull.sh >/dev/null && sudo chmod +x /tmp/run-pull.sh",
		encoded)
	out, err := harness.GuestExec(tgt, writeCmd)
	require.NoErrorf(t, err, "write run-pull.sh: %s", strings.TrimSpace(out))

	launchCmd := "sudo rm -f /tmp/run-pull.status /tmp/run-pull.exitcode; " +
		`sudo systemd-run --unit=mulga-imagepull --description="storagegrowth image pull" -- /bin/bash /tmp/run-pull.sh`
	out, err = harness.GuestExec(tgt, launchCmd)
	require.NoErrorf(t, err, "launch run-pull.sh via systemd-run: %s", strings.TrimSpace(out))
}

// imagePullUnitStatus reports the background pull's progress by reading the
// exit-code marker file the guest script's EXIT trap always writes, rather
// than querying systemd unit state directly: the trap fires on every exit
// path, so the marker is a single place that is always eventually populated,
// whereas ActiveState/SubState carry transient values that would need their
// own state machine to interpret correctly here.
//
// exitCode is the marker's raw contents, empty until the trap has fired. It is
// returned rather than folded into failed because the code itself is recorded
// in the result — see imagePullWorkload.PullExitCode for what it can and
// cannot tell a consumer apart.
func imagePullUnitStatus(t *testing.T, tgt harness.SSHTarget) (status, exitCode string, done, failed bool) {
	t.Helper()
	out, err := harness.GuestExec(tgt, "cat /tmp/run-pull.status 2>/dev/null || echo unknown")
	status = strings.TrimSpace(out)
	if err != nil {
		// A transient SSH hiccup mid-pull is not itself a failure signal;
		// the next tick tries again.
		return status, "", false, false
	}
	rc, rcErr := harness.GuestExec(tgt, "cat /tmp/run-pull.exitcode 2>/dev/null || true")
	rc = strings.TrimSpace(rc)
	if rcErr == nil && rc != "" {
		return status, rc, true, rc != "0"
	}
	return status, "", false, false
}

// imagePullCurrentCycle reads the guest's current-cycle marker
// (/tmp/run-pull.cycle, overwritten once per iteration by
// imagePullGuestScriptRootCycles), so the periodic sampling loop in
// runImagePull can tag each sample with the cycle in flight when it was
// taken. Returns 0 (the zero value, and therefore omitted — see
// imagePullSample.Cycle) if the file does not exist yet or the read fails;
// missing a cycle tag on one sample does not make that sample unusable, so
// this is silent rather than logged or fatal — every tick already logs
// enough via harness.Detail's own "cycle" field for a human to notice a
// pattern of zeros if the marker file is never appearing at all.
func imagePullCurrentCycle(t *testing.T, tgt harness.SSHTarget) int {
	t.Helper()
	out, err := harness.GuestExec(tgt, "cat /tmp/run-pull.cycle 2>/dev/null || true")
	if err != nil {
		return 0
	}
	n, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil {
		return 0
	}
	return n
}

// imagePullCycleMarkerRE matches one "CYCLE n=<i> exit=<code>
// guest_used=<bytes> elapsed=<secs>" line imagePullGuestScriptRootCycles
// appends to /tmp/run-pull.cycles once per pull-then-free attempt.
var imagePullCycleMarkerRE = regexp.MustCompile(`^CYCLE n=(\d+) exit=(\d+) guest_used=(\d+) elapsed=(\d+)`)

// readImagePullCycles reads the guest's cycle-marker file after
// imagePullGuestScriptRootCycles's N-cycle loop settles, and fills
// w.Cycles and w.PullCompleted from it. A missing or unparsable file is
// logged, not fatal: cycles mode's void gate is the same one every mode
// uses (writeImagePullResult's no-samples-or-non-positive-guest-used-bytes
// check), and losing the per-cycle breakdown does not make the samples
// already captured in w.Samples unusable.
func readImagePullCycles(t *testing.T, tgt harness.SSHTarget, w *imagePullWorkload) {
	t.Helper()
	out, err := harness.GuestExec(tgt, "cat /tmp/run-pull.cycles 2>/dev/null || true")
	if err != nil {
		t.Logf("storagegrowth: read /tmp/run-pull.cycles: %v", err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := imagePullCycleMarkerRE.FindStringSubmatch(line)
		if m == nil {
			t.Logf("storagegrowth: unparsable cycle marker line %q", line)
			continue
		}
		n, _ := strconv.Atoi(m[1])
		exitCode, _ := strconv.Atoi(m[2])
		used, _ := strconv.ParseInt(m[3], 10, 64)
		elapsed, _ := strconv.ParseFloat(m[4], 64)
		w.Cycles = append(w.Cycles, imagePullCycle{
			Cycle:          n,
			ExitCode:       exitCode,
			GuestUsedBytes: used,
			ElapsedSecs:    elapsed,
			PullCompleted:  exitCode == 0,
		})
	}
	// PullCompleted mirrors single-pull mode's meaning (did the last pull
	// attempt actually succeed), not "did the script finish its loop" — see
	// the "done" branch in runImagePull's sampling loop for why those are
	// different questions in cycles mode.
	if len(w.Cycles) > 0 {
		w.PullCompleted = w.Cycles[len(w.Cycles)-1].PullCompleted
	}
}

// captureImagePullSample takes one guest/host/backend reading. dfPath is
// where the guest-side denominator is sampled from — imagePullMount in
// volume mode, "/" in root mode (see runImagePull). Every failure here is
// logged, not fatal: a single bad reading mid-pull (a busy SSH session, a du
// that raced a write) must not abort a run that would otherwise complete, and
// a sparse sample series is still useful — only an entirely empty one is not,
// which the caller's final require.Positivef on the guest side guards
// against.
func captureImagePullSample(t *testing.T, tgt harness.SSHTarget, ssh *harness.PeerSSH, wanHost, volID string, start time.Time, dfPath string) imagePullSample {
	t.Helper()
	s := imagePullSample{ElapsedSecs: time.Since(start).Seconds()}

	if out, err := harness.GuestExec(tgt, fmt.Sprintf("df --output=used -B1 %s", dfPath)); err != nil {
		t.Logf("storagegrowth: sample guest df on %s: %v: %s", dfPath, err, strings.TrimSpace(out))
	} else if v, perr := parseLastIntField(out); perr != nil {
		t.Logf("storagegrowth: parse guest df output %q: %v", out, perr)
	} else {
		s.GuestUsedBytes = v
	}

	if v, err := hostPredastoreAllocatedBytes(ssh, wanHost); err != nil {
		t.Logf("storagegrowth: host du via PeerSSH on %s: %v", wanHost, err)
	} else {
		s.HostAllocatedBytes = v
	}

	rssCtx, rssCancel := context.WithTimeout(context.Background(), 15*time.Second)
	rssOut, rssErr := ssh.Run(rssCtx, wanHost, `ps -eo rss,comm | awk '$2 ~ /nbdkit/ {s+=$1} END {printf "%d", s+0}'`)
	rssCancel()
	if rssErr != nil {
		t.Logf("storagegrowth: nbdkit RSS via PeerSSH on %s: %v", wanHost, rssErr)
	} else if v, perr := strconv.ParseInt(strings.TrimSpace(string(rssOut)), 10, 64); perr != nil {
		t.Logf("storagegrowth: parse nbdkit RSS output %q: %v", rssOut, perr)
	} else {
		s.NbdkitRSSKiB = v
	}

	// HeadObject the live-checkpoint key on the node itself. The gateway identity
	// the operator box signs with has no S3 read grant on this bucket (it answers
	// 403), so the only credential that works is predastore's own, which lives on
	// the node in predastore.toml. Reading it there over PeerSSH keeps the secret
	// on the node: it is used inline to invoke the AWS CLI, unset immediately, and
	// never crosses back to the operator box.
	key := fmt.Sprintf("%s/checkpoints/blocks.live.bin", volID)
	region := imagePullGetenv("SPINIFEX_AWS_REGION", "ap-southeast-2")
	headCtx, headCancel := context.WithTimeout(context.Background(), 20*time.Second)
	headOut, headErr := ssh.Run(headCtx, wanHost, fmt.Sprintf(imagePullCheckpointHeadScript, region, key))
	headCancel()
	if headErr != nil {
		// A transport failure is a broken probe, not an absent checkpoint: void
		// the reading so it can never be read back as a genuine zero.
		s.CheckpointBytes = -1
		s.CheckpointProbeErr = fmt.Sprintf("head via PeerSSH: %v", headErr)
		t.Logf("storagegrowth: checkpoint head via PeerSSH on %s: %v", wanHost, headErr)
		return s
	}
	line := strings.TrimSpace(string(headOut))
	switch {
	case strings.HasPrefix(line, "OK\t"):
		// OK <content-length> <last-modified>, tab-separated by the probe script.
		parts := strings.Split(line, "\t")
		if v, perr := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); perr == nil {
			s.CheckpointBytes = v
		} else {
			s.CheckpointBytes = -1
			s.CheckpointProbeErr = fmt.Sprintf("parse checkpoint length %q: %v", parts[1], perr)
		}
		if len(parts) >= 3 {
			if tm, terr := time.Parse(time.RFC3339, strings.TrimSpace(parts[2])); terr == nil {
				s.CheckpointLastModified = tm.UTC().Format(time.RFC3339Nano)
			}
		}
	case line == "NOTFOUND":
		// Expected before the first drain has fired: the checkpoint object does
		// not exist yet, so CheckpointBytes legitimately stays 0.
	default:
		// The probe classified an auth or other failure (a 403 the on-node creds
		// should have prevented). Void the reading rather than emit a false 0.
		s.CheckpointBytes = -1
		s.CheckpointProbeErr = strings.TrimPrefix(strings.TrimPrefix(line, "ERR"), "\t")
		t.Logf("storagegrowth: checkpoint head on %s: %s", wanHost, line)
	}

	return s
}

// verifyImagePullLandedOnVolume asserts the data-root redirect actually took
// effect: the mounted volume must contain the runtime's own state
// directories, not just an empty filesystem next to a daemon that silently
// fell back to /var/lib/docker on the root disk.
func verifyImagePullLandedOnVolume(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	out, err := harness.GuestExec(tgt, fmt.Sprintf("sudo ls -A %s", imagePullMount))
	require.NoErrorf(t, err, "list %s: %s", imagePullMount, strings.TrimSpace(out))
	entries := strings.Fields(out)
	require.NotEmptyf(t, entries, "%s is empty after the pull — the container runtime's data-root redirect did not take effect", imagePullMount)
	harness.Detail(t, "data_root_entries", strings.Join(entries, ","))
}

// imagePullCheckpointHeadScript HEADs the live-checkpoint object on the node
// using predastore's own [[auth]] credentials from predastore.toml, because the
// gateway identity the operator box holds cannot read this bucket. It emits one
// classified line on stdout and always exits 0, so the classification — not an
// SSH exit status — drives the caller: "OK\t<len>\t<lastmod>" on success,
// "NOTFOUND" when the object does not exist yet, or "ERR\t<detail>" on an auth
// or transport failure. predastore.toml is root-owned, so the read goes
// through passwordless sudo (the node's SSH user has it, same as the du/ps
// samples above). The credentials are read into shell locals, passed inline to
// a single aws invocation, then unset — never echoed, never leaving the node.
// Format args: 1=region, 2=object key.
const imagePullCheckpointHeadScript = `set -u
cfg=/etc/spinifex/predastore/predastore.toml
ak=$(sudo -n awk '/^\[\[auth\]\]/{f=1} f && /^access_key_id/{gsub(/^[^"]*"|"[^"]*$/,""); print; exit}' "$cfg")
sk=$(sudo -n awk '/^\[\[auth\]\]/{f=1} f && /^secret_access_key/{gsub(/^[^"]*"|"[^"]*$/,""); print; exit}' "$cfg")
if [ -z "$ak" ] || [ -z "$sk" ]; then printf 'ERR\tno predastore credentials in %%s\n' "$cfg"; exit 0; fi
out=$(AWS_ACCESS_KEY_ID="$ak" AWS_SECRET_ACCESS_KEY="$sk" AWS_CA_BUNDLE=/etc/spinifex/ca.pem \
  aws --endpoint-url https://127.0.0.1:8443 --region '%[1]s' s3api head-object \
  --bucket predastore --key '%[2]s' --query '[ContentLength,LastModified]' --output text 2>&1)
rc=$?
unset ak sk
if [ "$rc" -eq 0 ]; then printf 'OK\t%%s\n' "$out";
elif printf '%%s' "$out" | grep -qiE '404|not found'; then printf 'NOTFOUND\n';
else printf 'ERR\t%%s\n' "$(printf '%%s' "$out" | tr '\n' ' ')"; fi
`

// imagePullGetenv returns the named environment variable, or def when unset.
// A trivial local copy of harness's unexported getenv (env.go), which this
// suite does not import from because tests/e2e/harness is deliberately never
// edited by this suite (see the package doc in main_test.go).
func imagePullGetenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseLastIntField extracts the last whitespace-separated integer field from
// out, tolerating a leading header line (df prints one; du does not, but the
// same parse works for both).
func parseLastIntField(out string) (int64, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("no output")
	}
	return strconv.ParseInt(fields[len(fields)-1], 10, 64)
}

// hostPredastoreAllocatedBytes reads predastoreBaseDir's total allocated
// bytes on the node at wanHost, over PeerSSH. This is the one measurement
// that actually works against a live deployment: it needs no S3 credential
// at all, only SSH access to the node, unlike a per-volume S3 listing of the
// backend bucket (see assertNPassBackendByteDelta in storage_growth_test.go
// for why that path is unusable here).
//
// The number returned is NODE-WIDE, not attributable to any single volume --
// du has no concept of which volume a chunk object belongs to, it only sees
// bytes on disk. Callers that need a per-workload signal must take two
// readings and use the delta, and must be able to argue nothing else was
// writing to the node in between.
func hostPredastoreAllocatedBytes(ssh *harness.PeerSSH, wanHost string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := ssh.Run(ctx, wanHost, fmt.Sprintf("sudo du -s --block-size=1 %s 2>/dev/null | cut -f1", predastoreBaseDir))
	if err != nil {
		return 0, fmt.Errorf("du %s via PeerSSH: %w", predastoreBaseDir, err)
	}
	v, perr := parseLastIntField(string(out))
	if perr != nil {
		return 0, fmt.Errorf("parse du output %q: %w", out, perr)
	}
	return v, nil
}

// writeImagePullResult persists the workload record at the path named by
// npassResultPathEnv (SPINIFEX_STORAGEGROWTH_RESULT_PATH) — the same env var
// npass uses, per the shared result contract. See writeNPassResult for why an
// unset path only logs, and why a set path treats every failure as fatal.
//
// This is the single arbiter of which runs produce a result at all, and it is
// deliberately not the same question as whether the test passed. A run whose
// pull failed but which captured real samples is VALID and is written: an
// image too large for its volume is the phenomenon under test, and discarding
// its data would throw away the sweep's most interesting points. A run that
// captured nothing — the guest never came up, SSH never worked, every df
// reading failed — is VOID, and writes no file, because a consumer distinguishes
// the two by the file's presence alone.
//
// w is a pointer so a deferred call to this function observes the record as it
// finally stood, not a copy frozen when the defer was registered.
func writeImagePullResult(t *testing.T, w *imagePullWorkload) {
	t.Helper()
	data, err := json.MarshalIndent(w, "", "  ")
	require.NoError(t, err, "marshal image-pull workload record")

	path := os.Getenv(npassResultPathEnv)
	if path == "" {
		harness.Detail(t, "workload_record", string(data))
		return
	}

	// The void gate. No samples means nothing was ever measured, and a
	// non-positive denominator is the same unusable number the run's own
	// require.Positivef rejects — every downstream ratio divides by it. Writing
	// either would hand the measurement step a plausible, wrong point instead of
	// the absent one that correctly voids it. t.Errorf, not require: this runs
	// deferred, typically while the test is already unwinding from the failure
	// that caused the void, and the test is red either way.
	if len(w.Samples) == 0 || w.GuestUsedBytes <= 0 {
		t.Errorf("storagegrowth: refusing to write %s from a run that measured nothing "+
			"(%d samples, guest_used_bytes=%d) — the denominator every downstream ratio divides by is "+
			"unusable, and an absent result is how the measurement step learns this point is void",
			path, len(w.Samples), w.GuestUsedBytes)
		return
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "result dir for %s", path)
	require.NoError(t, os.WriteFile(path, data, 0o644), "write %s", path)
	harness.Detail(t, "workload_result", path, "pull_completed", w.PullCompleted)
}

// imagePullImage returns the image to pull, overridable via
// SPINIFEX_STORAGEGROWTH_IMAGE. Must be pinned by digest: a floating tag
// would let the image drift between runs, confounding any comparison across
// sweep points that is supposed to hold the workload constant.
func imagePullImage() string {
	if v := os.Getenv("SPINIFEX_STORAGEGROWTH_IMAGE"); v != "" {
		return v
	}
	return imagePullDefaultImage
}

// imagePullVolumeGiB returns the workload volume's size, overridable via
// SPINIFEX_STORAGEGROWTH_VOLUME_GIB. Unlike npass's fixed 1 GiB extent, this
// must be large enough to actually hold a pulled image plus its extracted
// layers.
func imagePullVolumeGiB() int {
	return envPositiveIntOr("SPINIFEX_STORAGEGROWTH_VOLUME_GIB", 20)
}

// imagePullSampleInterval returns the sampling interval in seconds,
// overridable via SPINIFEX_STORAGEGROWTH_SAMPLE_SECS.
func imagePullSampleInterval() int {
	return envPositiveIntOr("SPINIFEX_STORAGEGROWTH_SAMPLE_SECS", 15)
}

// imagePullTarget returns which disk the pull's bytes land on, overridable
// via SPINIFEX_STORAGEGROWTH_TARGET. imagePullTargetVolume (default) is the
// original workload: a fresh, empty, independently-created data volume.
// imagePullTargetRoot instead targets the instance's own root volume, which is
// cloned from an AMI snapshot. That ancestry was the original reason for the
// mode and is refuted (the two targets amplify within 0.16% of each other);
// root mode is retained because only it can undersize the disk the pull writes
// to. See TestImagePullGrowth's doc.
//
// An unrecognized value is a configuration error and panics rather than
// silently falling back to volume mode, matching envPositiveIntOr/envBool's
// reasoning: a mistyped target would otherwise silently measure the wrong
// mode and attribute the result to the one that was intended.
func imagePullTarget() string {
	v := os.Getenv(imagePullTargetEnv)
	if v == "" {
		return imagePullTargetVolume
	}
	switch v {
	case imagePullTargetVolume, imagePullTargetRoot:
		return v
	default:
		panic(fmt.Sprintf("%s=%q is not one of %q, %q", imagePullTargetEnv, v, imagePullTargetVolume, imagePullTargetRoot))
	}
}

// imagePullRootGiB returns the root volume's size in root mode, overridable
// via SPINIFEX_STORAGEGROWTH_ROOT_GIB. The AMI's default root disk is sized
// for booting, not for holding a multi-GB image pull, so this must default
// well above imagePullVolumeGiB's default: undersizing it produces ENOSPC on
// the root disk, which is exactly the failure root mode exists to rule in or
// out, not a harness bug to work around by shrinking the pull.
func imagePullRootGiB() int {
	return envPositiveIntOr("SPINIFEX_STORAGEGROWTH_ROOT_GIB", 60)
}

// imagePullPullCycles returns how many pull-then-free cycles the guest
// script drives, overridable via SPINIFEX_STORAGEGROWTH_PULL_CYCLES. Default
// 1 is today's single-pull behaviour, fully unchanged (see
// imagePullGuestScriptRoot/imagePullGuestScript — neither is touched by this
// mode). A value above 1 switches root mode onto
// imagePullGuestScriptRootCycles instead, looping the pull-then-free sequence
// that reproduces containerd's own evict-and-retry churn on a disk too small
// for the image — see TestImagePullGrowth's doc for why that pattern, not a
// single failed pull, is the shape a real EKS GPU worker node hit. Rejected
// outside root mode by TestImagePullGrowth itself, not here: this function
// only parses the env var, it does not have target in scope to validate
// against.
func imagePullPullCycles() int {
	return envPositiveIntOr(imagePullPullCyclesEnv, 1)
}
