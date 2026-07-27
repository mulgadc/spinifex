//go:build e2e

package storagegrowth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// npassDevice is the guest-visible attach point requested for every
	// pass. The kernel may enumerate the actual device name differently
	// (vdc/vdd/...) after each attach, which is why every pass rediscovers
	// it via GuestDiskSet/WaitForNewGuestDisk rather than trusting this
	// value directly.
	npassDevice = "/dev/sdf"
	// npassVolumeSizeGiB is fixed. The workload only ever touches a small
	// extent of the volume, and every metric derived from this workload is
	// a ratio against the measured guest-side footprint, so the volume's
	// nominal size does not enter the result.
	npassVolumeSizeGiB = 1
	// npassSentinelLabel prefixes the ext4 label GuestFormatWriteSentinel
	// assigns. Suffixed with the pass count so a label collision cannot
	// silently let one invocation mount another's filesystem.
	npassSentinelLabel = "npassprobe"
	// npassDFMount is the guest mountpoint used to read the filesystem's
	// used-bytes denominator. GuestFormatWriteSentinel unmounts before it
	// returns, so the volume is remounted here purely to measure it.
	npassDFMount = "/tmp/npass-df"
	// npassRunTagKey mirrors the tag key the harness stamps on every
	// resource its Ensure* helpers create, so a volume this suite retains
	// past process exit stays reclaimable by the standard run-scoped
	// teardown sweep. The key is restated here rather than imported because
	// it is unexported in the harness, and that package is deliberately
	// never edited by this suite.
	npassRunTagKey = "e2e:run"
	// npassResultPathEnv names the file this workload writes its record to for
	// an external measurement step to read.
	//
	// It is a caller-supplied path rather than a file under the harness's
	// artifact directory, because that directory is a failure-diagnostics
	// convention: harness.ArtifactDir registers a cleanup that deletes it on
	// test PASS. A handoff written there would exist only for runs that failed
	// — precisely inverted from what a measurement step needs.
	npassResultPathEnv = "SPINIFEX_STORAGEGROWTH_RESULT_PATH"

	// npassBackendByteAccountingTolerance is the ceiling ratio of node-wide
	// predastore-allocated-byte growth to guest-written logical bytes a
	// healthy N-pass run may show.
	//
	// The floor is viperblock's own encoding overhead, not slack for a leak:
	// chunks are written under Reed-Solomon 2+1 erasure coding, which splits
	// each object into two data shards plus one parity shard of the same
	// size -- 1.5x the logical payload, not 3x replication -- and every
	// block is additionally framed with a 16-byte AES-GCM tag.
	//
	// That 1.5x floor is a precondition, not a universal constant. It holds
	// because this suite provisions RS(2,1): cmd/spinifex/cmd/templates/
	// predastore.toml's [rs] block defaults to data=2, parity=1. The same
	// template documents RS(3,2) as the production recommendation, whose
	// floor is 5/3 = ~1.667x and would leave only ~12% headroom under the
	// ceiling below instead of the ~25% intended. Recompute this value if the
	// suite is ever pointed at a different RS profile rather than assuming
	// the band still holds.
	//
	// A healthy, fully-GC'd volume settles at ~1.5x and does not go
	// materially above it. The tolerance widens that floor by 25% (to 1.875x) to
	// absorb two effects that are real but not leaks: every chunk pays a
	// fixed header regardless of how many blocks it packs (pulling a
	// sparse volume's ratio up), and the tail chunk of any write run is
	// under-packed by construction (a chunk flushes on full or on drain,
	// so the last one of a run is real, unavoidable overhead). Both shrink
	// as a proportion of the total as the workload's footprint grows, so a
	// large workload should sit close to the bare floor and a small one
	// gets the extra headroom it actually needs.
	//
	// This does not attempt to catch the slow, bounded residual that a
	// future partial-chunk compaction pass would close -- only the
	// unbounded-growth shape the storage-growth incident produced, where
	// a leak multiplies with every overwrite pass instead of settling.
	//
	// Applying this floor to a node-wide `du` delta (rather than a per-volume
	// S3 listing, see assertNPassBackendByteDelta) is only sound because the
	// delta is measured tightly around this workload's own passes on a
	// dedicated, single-volume, serial environment -- nothing else on the
	// node is expected to write to predastore's store in that window. Any
	// concurrent writer (another suite, another volume, a background GC/
	// compaction pass) would inflate or shrink the delta for reasons that
	// have nothing to do with this workload, and the ratio below would no
	// longer mean what it claims to.
	npassBackendByteAccountingTolerance = 1.875
)

// npassWorkload records what one invocation of the N-pass workload did. It is
// the whole contract with whatever measures the backing store afterwards,
// beyond this suite's own coarse in-process gate (assertNPassBackendByteDelta).
//
// The guest-side quantities below are the ones only a guest can know; a
// live-versus-dead breakdown of the backend's byte cost -- a distinction no
// host-side `du` can make -- is measured out of band by a tool that can read
// predastore's own accounting directly. Coupling this test to that tool would
// also break a standalone spinifex checkout, where it does not exist.
//
// GuestUsedBytes is measured, never inferred. It is the filesystem's own
// used-bytes count after the write, so it includes ext4 metadata as well as
// the sentinel payload — which is the honest logical footprint, and is
// materially larger than the sentinel alone on a freshly-mkfs'd volume.
// Deriving it from the sentinel size instead would understate the
// denominator and report amplification that is really just filesystem
// metadata.
type npassWorkload struct {
	Passes    int `json:"passes"`
	ExtentMiB int `json:"extent_mib"`
	// VolumeID lets the measurement step attribute backend objects to this
	// workload, and lets an operator reclaim a retained volume by hand.
	VolumeID string `json:"volume_id"`
	// Retained reports whether the volume outlived the test process. The
	// measurement step must know this: deleting the volume purges its
	// objects from the backing store, so a measurement taken after cleanup
	// sees no leak regardless of whether one occurred.
	Retained         bool    `json:"retained"`
	GuestUsedBytes   int64   `json:"guest_used_bytes"`
	GuestUsedPerPass []int64 `json:"guest_used_per_pass_bytes"`
	// SHA256PerPass is the digest written and read back on each pass. Each
	// pass writes fresh random bytes, so these differ by construction; they
	// are recorded as evidence that every pass genuinely rewrote the extent
	// rather than short-circuiting.
	SHA256PerPass []string `json:"sha256_per_pass"`
}

// TestNPassOverwrite rewrites one logical extent N times via complete attach →
// format+write+fsync → detach cycles, which force viperblock's drain/close path
// on every pass, then reports what it did for an external measurement step to
// score.
//
// The workload is the microscope for the unreferenced-live leak: the guest-side
// footprint is identical on every pass, while the total bytes ever written scale
// with N. If superseded chunk objects are never deleted, N passes cost N copies;
// if they are reclaimed, N passes cost one.
//
// What this test asserts on its own is only what the guest can see: that every
// pass round-tripped its data through the backend intact, and that the footprint
// stayed constant across passes. Both are worth checking independently of any
// measurement — a drifting footprint would invalidate every ratio derived from
// this workload, and a failed round-trip would mean the passes are not doing what
// the sweep assumes. That makes this a legitimate standalone test in a plain
// spinifex checkout, where no measurement tooling exists.
//
// A sequential write-once is deliberately not used here: it supersedes nothing,
// so it cannot exercise this fault at all (it probes block-map memory growth
// instead). The container-image-pull workload — the shape originally reported,
// whose overwrite pattern is uncontrolled — is left for a follow-up. The shapes
// are not interchangeable.
func TestNPassOverwrite(t *testing.T) {
	fix := requireStorageGrowthFixture(t)
	harness.Phase(t, "Storage Growth — N-Pass Volume Overwrite (unreferenced-live chunk leak)")

	passes := npassPasses()
	extentMiB := npassExtentMiB()
	retain := npassRetainVolume()

	instanceID, tgt := ensureWorkloadInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := createWorkloadVolume(t, fix, az, retain)

	harness.Detail(t, "passes", passes, "extent_mib", extentMiB, "instance", instanceID,
		"volume", volID, "retain_volume", retain)

	// Baseline predastore's node-wide allocated bytes immediately before the
	// passes start, and read it again immediately after they finish, so the
	// delta below brackets this workload's own writes as tightly as
	// possible. See assertNPassBackendByteDelta for why this must be a
	// delta rather than an absolute snapshot, and why the window must stay
	// tight.
	ssh := harness.NewPeerSSH()
	before, err := hostPredastoreAllocatedBytes(ssh, fix.Env.WANHost)
	require.NoErrorf(t, err, "backend byte accounting: baseline node du before passes")

	w := runNPassOverwrite(t, fix, tgt, instanceID, volID, passes, extentMiB)
	w.Retained = retain
	writeNPassResult(t, w)

	harness.Detail(t, "passes_completed", w.Passes, "guest_used_bytes", w.GuestUsedBytes,
		"guest_used_per_pass", fmt.Sprint(w.GuestUsedPerPass))

	after, err := hostPredastoreAllocatedBytes(ssh, fix.Env.WANHost)
	require.NoErrorf(t, err, "backend byte accounting: node du after passes")

	// This is the fail-first demonstration for the storage-growth leak: if
	// superseded chunks from the earlier passes are never reclaimed, the
	// backend footprint grows by N times the final pass's guest-used bytes
	// rather than settling at the erasure floor, and the delta below catches
	// that shape without needing vbrefscan's out-of-band scoring pass. It
	// complements, not replaces, the RETAIN_VOLUME external-measurement
	// path: that path attributes *which* chunks are garbage on a volume kept
	// alive for vbrefscan; this one is a pass/fail gate that runs on every
	// invocation, retained or not.
	assertNPassBackendByteDelta(t, before, after, w.GuestUsedBytes)
}

// assertNPassBackendByteDelta asserts that predastore's node-wide allocated
// bytes grew, across the N-pass workload, by no more than
// npassBackendByteAccountingTolerance times guestWrittenBytes.
//
// This measures via `du` on the node (hostPredastoreAllocatedBytes) rather
// than listing the volume's own S3 chunk prefix. A per-volume S3 listing is
// what this suite originally used and is what vbrefscan itself reads, but it
// is not usable here: the internal bucket EBS volume chunks live under is
// owned by a system account distinct from the tenant-scoped identity this
// harness authenticates as, predastore enforces a same-account
// bucket-ownership check independent of and on top of any IAM policy grant,
// and cross-account bucket-policy or ACL grants are not implemented -- every
// listing attempt fails with AccessDenied regardless of what IAM policy the
// caller holds. There is also no usable filesystem fallback: predastore's
// distributed backend stores objects in badger plus shard nodes, not a tree
// keyed by object name, so there is nothing under predastoreBaseDir to
// attribute to a single volume by path.
//
// `du` over predastoreBaseDir is therefore the finest-grained measurement
// actually available, and it is NODE-WIDE: it cannot distinguish this
// volume's bytes from any other write happening on the same node. The before/
// after delta (rather than an absolute reading) is what makes the number
// mean anything at all, and even the delta is only valid because this
// workload runs serially against a single volume on a dedicated environment
// -- nothing else is expected to write to predastore's store in the window
// between the two samples. This approach would be invalid under concurrency:
// a second suite or a background compaction pass writing to the same node in
// that window would be silently attributed to this workload's ratio.
func assertNPassBackendByteDelta(t *testing.T, before, after, guestWrittenBytes int64) {
	t.Helper()
	require.Positivef(t, guestWrittenBytes,
		"backend byte accounting: guestWrittenBytes must be positive, got %d -- the denominator every ratio divides by is unusable",
		guestWrittenBytes)

	delta := after - before
	ratio := float64(delta) / float64(guestWrittenBytes)
	harness.Detail(t, "backend_allocated_bytes_before", before, "backend_allocated_bytes_after", after,
		"backend_allocated_bytes_delta", delta, "guest_written_bytes", guestWrittenBytes,
		"ratio", fmt.Sprintf("%.3f", ratio))

	require.LessOrEqualf(t, ratio, npassBackendByteAccountingTolerance,
		"backend byte accounting: node predastore allocated bytes grew %d across the N-pass workload, %.2fx guest-written %d bytes -- exceeds the %.3fx RS-erasure-floor tolerance; backend growth looks unbounded, not settled at the floor",
		delta, ratio, guestWrittenBytes, npassBackendByteAccountingTolerance)
}

// runNPassOverwrite drives n complete format+write+fsync+detach cycles against
// one volume and returns the measured record of what happened.
//
// Every pass reformats and rewrites the same sentinel file, so each pass
// supersedes the previous pass's blocks entirely. The detach between passes is
// load-bearing rather than incidental: it forces a full drain/close, which is
// what makes each pass a complete, settled write rather than a partially
// buffered one.
func runNPassOverwrite(t *testing.T, fix *Fixture, tgt harness.SSHTarget, instanceID, volID string, n, extentMiB int) npassWorkload {
	t.Helper()
	harness.Step(t, "%d format-write-detach passes of %d MiB against %s", n, extentMiB, volID)

	label := fmt.Sprintf("%s%d", npassSentinelLabel, n)
	usedPerPass := make([]int64, 0, n)
	shaPerPass := make([]string, 0, n)

	for pass := 1; pass <= n; pass++ {
		before := harness.GuestDiskSet(t, tgt)
		harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, npassDevice)
		dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)

		written := harness.GuestFormatWriteSentinel(t, tgt, dev, label, extentMiB)
		usedPerPass = append(usedPerPass, measureGuestUsedBytes(t, tgt, dev))

		// Read the sentinel back through a fresh mount, after the write's own
		// mount has been dropped. This is what makes the pass a proven
		// round-trip rather than a read of still-cached bytes: if a pass's data
		// did not survive its own drain, the sweep's premise (that every pass
		// writes a complete, settled copy) is false and the numbers mean
		// nothing.
		readBack := harness.GuestReadSentinelSha(t, tgt, "/dev/"+dev, label)
		require.Equalf(t, written, readBack,
			"pass %d/%d: sentinel sha256 changed across unmount/remount (wrote %s, read %s) — the pass did not round-trip through the backend intact",
			pass, n, written, readBack)
		shaPerPass = append(shaPerPass, written)

		harness.DetachVolumeWait(t, fix.AWS, volID)
	}

	guestUsed := usedPerPass[len(usedPerPass)-1]
	require.Positivef(t, guestUsed, "guest reported 0 used bytes after writing %d MiB — the denominator every downstream ratio divides by is unusable", extentMiB)
	requireConstantFootprint(t, usedPerPass)

	return npassWorkload{
		Passes:           n,
		ExtentMiB:        extentMiB,
		VolumeID:         volID,
		GuestUsedBytes:   guestUsed,
		GuestUsedPerPass: usedPerPass,
		SHA256PerPass:    shaPerPass,
	}
}

// createWorkloadVolume creates the volume the passes run against and waits for
// it to become available.
//
// It does not use harness.EnsureVolume, for two reasons that both matter here.
// EnsureVolume memoizes on an (az, size) key, so two invocations wanting
// distinct volumes would silently share one. And its cleanup is unconditional,
// whereas this workload must be able to leave the volume in place: DeleteVolume
// enumerates the volume's whole object prefix and deletes every object under it,
// including the superseded chunks the leak consists of, so a backend measured
// after cleanup shows no leak whether or not one happened.
//
// A retained volume is tagged with the same run tag the harness stamps on its
// own resources, so the standard run-scoped teardown sweep reclaims it at the
// end of the run rather than leaving it stranded.
func createWorkloadVolume(t *testing.T, fix *Fixture, az string, retain bool) string {
	t.Helper()

	// The volume's own backing-store cost is the subject under test, so its
	// identity and lifetime cannot be delegated to the memoized helper.
	// e2e:allow-create
	out, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(npassVolumeSizeGiB),
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
			// Best-effort, matching the harness's own tagging: the volume still
			// works, it just will not be reclaimable by the id sweep.
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

// requireConstantFootprint fails when the guest-side used bytes drift
// across passes. Identical mkfs plus identical payload must produce an
// identical footprint; if it does not, the workload is not holding the
// denominator still and every ratio derived from it is meaningless. The
// tolerance covers ext4's lazy metadata initialisation settling between
// passes, not a genuine change in what was written.
func requireConstantFootprint(t *testing.T, used []int64) {
	t.Helper()
	if len(used) < 2 {
		return
	}
	lo, hi := used[0], used[0]
	for _, u := range used {
		lo = min(lo, u)
		hi = max(hi, u)
	}
	const tolerance = 0.05
	spread := float64(hi-lo) / float64(hi)
	require.LessOrEqualf(t, spread, tolerance,
		"guest footprint drifted %.1f%% across passes (min=%d max=%d, series=%v) — the denominator is not constant, so growth in backing store cannot be attributed to the leak",
		spread*100, lo, hi, used)
}

// ensureWorkloadInstance launches (or returns the memoized) instance the passes
// attach to, and waits for guest SSH. The instance is not part of what is being
// measured — only the volume attached to it is.
func ensureWorkloadInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d", host, port)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// measureGuestUsedBytes returns the attached filesystem's own used-bytes
// count — the measured logical footprint every downstream ratio divides by.
// It must be measured in-guest rather than derived from the payload size: the
// volume is mkfs'd on every pass, so ext4 metadata is a real and material part
// of what the guest actually put on the disk.
//
// GuestFormatWriteSentinel unmounts before returning, so the device is
// remounted here solely to read df, then unmounted again so the subsequent
// detach stays clean. The unmount runs even when df fails, otherwise a
// single bad reading would wedge every later pass behind a busy device.
//
// Caveat for interpreting the result: df counts blocks the filesystem has
// allocated, including inode tables that ext4 initialises lazily and may
// never actually write. Those cost nothing on the backend, so a multiple
// derived from this can sit slightly below the RS 2+1 floor. The load-bearing
// property is flatness across passes, which this does not affect.
func measureGuestUsedBytes(t *testing.T, tgt harness.SSHTarget, dev string) int64 {
	t.Helper()
	// df's output is not piped through tail: the exit status of a pipeline
	// is that of its LAST command, so `v=$(df ... | tail -n1)` reports
	// success even when df failed, leaving an empty value that reads as a
	// missing measurement rather than an error. df prints a "Used" header
	// above the value, and the parse below takes the last field, so the
	// pipe buys nothing and costs the error signal.
	script := fmt.Sprintf(
		"sudo mkdir -p %[1]s && sudo mount /dev/%[2]s %[1]s || exit 1; "+
			"out=$(df --output=used -B1 %[1]s); rc=$?; "+
			"sudo umount %[1]s; "+
			`[ $rc -eq 0 ] || { printf '%%s\n' "$out" >&2; exit $rc; }; `+
			`printf '%%s\n' "$out"`,
		npassDFMount, dev)
	out, err := harness.GuestExec(tgt, script)
	require.NoErrorf(t, err, "guest df on /dev/%s: %s", dev, strings.TrimSpace(out))

	fields := strings.Fields(out)
	require.NotEmptyf(t, fields, "guest df on /dev/%s produced no output: %q", dev, out)
	used, perr := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	require.NoErrorf(t, perr, "parse guest df output %q", out)
	return used
}

// writeNPassResult persists the workload record for an external measurement
// step, at the path named by npassResultPathEnv. A run with the variable unset
// has no such consumer — a plain CI run of this suite — and only logs the
// record.
//
// When the path IS set, every failure here is fatal rather than logged. A
// measurement step that cannot read this file has no measured denominator, and
// its only alternative is to infer one from the payload size — which is the
// exact mistake measuring the footprint in-guest exists to prevent, and which
// would silently produce a plausible, wrong multiple.
func writeNPassResult(t *testing.T, w npassWorkload) {
	t.Helper()
	data, err := json.MarshalIndent(w, "", "  ")
	require.NoError(t, err, "marshal workload record")

	path := os.Getenv(npassResultPathEnv)
	if path == "" {
		harness.Detail(t, "workload_record", string(data))
		return
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "result dir for %s", path)
	require.NoError(t, os.WriteFile(path, data, 0o644), "write %s", path)
	harness.Detail(t, "workload_result", path)
}

// npassPasses returns how many times the extent is rewritten, overridable via
// SPINIFEX_STORAGEGROWTH_PASSES. One invocation drives exactly one pass count:
// a sweep over N is driven externally, one invocation per point, because the
// backing store must be measured between points and that measurement lives
// outside this repo.
//
// The default is 4 — enough passes that a standalone run genuinely exercises
// supersession, without making a plain CI run tedious.
func npassPasses() int {
	return envPositiveIntOr("SPINIFEX_STORAGEGROWTH_PASSES", 4)
}

// npassExtentMiB returns the sentinel payload size written on each pass,
// overridable via SPINIFEX_STORAGEGROWTH_EXTENT_MIB. This sizes the workload
// only — the denominator is measured, not derived from it.
func npassExtentMiB() int {
	return envPositiveIntOr("SPINIFEX_STORAGEGROWTH_EXTENT_MIB", 16)
}

// npassRetainVolume reports whether the volume must outlive the test process,
// set via SPINIFEX_STORAGEGROWTH_RETAIN_VOLUME.
//
// An external measurement step needs this: DeleteVolume deletes every object
// under the volume's prefix, which includes the superseded chunks that are the
// leak, so a store measured after cleanup shows no leak whether or not one
// occurred. Retention is off by default so a standalone run cleans up after
// itself; the sweep that measures turns it on and reclaims by run tag at the
// end.
func npassRetainVolume() bool {
	return envBool("SPINIFEX_STORAGEGROWTH_RETAIN_VOLUME")
}

// envPositiveIntOr returns the named environment variable parsed as a positive
// int, or def when unset. A malformed or non-positive value is a configuration
// error and is reported as such rather than silently falling back to the
// default: a sweep point that quietly ran the wrong pass count would produce a
// plausible number attributed to the wrong N.
func envPositiveIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		panic(fmt.Sprintf("%s=%q is not a positive integer", key, v))
	}
	return n
}

// envBool reports whether the named environment variable is set to a truthy
// value. Anything unparseable is a configuration error rather than a silent
// false, so a mistyped retain flag cannot cause the measurement step to
// silently score a store whose evidence was already deleted.
func envBool(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		panic(fmt.Sprintf("%s=%q is not a boolean", key, v))
	}
	return b
}
