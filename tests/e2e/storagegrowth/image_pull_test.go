//go:build e2e

package storagegrowth

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
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
	// predastoreBucket is the fixed bucket name predastore's S3 API serves
	// every volume's objects under.
	predastoreBucket = "predastore"
	// predastoreHealthPort mirrors harness's unexported constant of the same
	// name (tests/e2e/harness/multinode_cluster.go): predastore's S3 API is
	// served directly on the node at this port, not proxied through the
	// spinifex AWS gateway on :9999.
	predastoreHealthPort = 8443
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
}

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
	Image     string `json:"image"`
	VolumeGiB int    `json:"volume_gib"`
	VolumeID  string `json:"volume_id"`
	// Retained reports whether the volume outlived the test process, exactly
	// as npassWorkload.Retained does — a measurement taken after DeleteVolume
	// purges the volume's objects sees no leak whether or not one occurred.
	Retained           bool              `json:"retained"`
	GuestUsedBytes     int64             `json:"guest_used_bytes"`
	Samples            []imagePullSample `json:"samples"`
	SampleIntervalSecs int               `json:"sample_interval_secs"`
	PullDurationSecs   float64           `json:"pull_duration_secs"`
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
// mounted viperblock volume — the guest runtime's data-root is redirected
// onto the mount before the pull starts — sampling guest/host/backend state
// throughout, then reports what it did for an external measurement step to
// score. See imagePullWorkload's doc for why this shape, not npass's, is the
// one that can test whether checkpoint-rewrite cost is quadratic in bytes
// written.
//
// Unlike npass, this instance does not use the shared default VPC: guest SSH
// and the pull itself both need real internet egress, and
// harness.InstancePublicSSHHost hard-fails on an instance with no public IP,
// so ensureImagePullInstance builds its own dedicated topology.
func TestImagePullGrowth(t *testing.T) {
	fix := requireStorageGrowthFixture(t)
	harness.Phase(t, "Storage Growth — Container Image Pull (checkpoint-rewrite quadratic-cost probe)")

	image := imagePullImage()
	volGiB := imagePullVolumeGiB()
	sampleInterval := imagePullSampleInterval()
	retain := npassRetainVolume()

	instanceID, tgt := ensureImagePullInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := createImagePullVolume(t, fix, az, volGiB, retain)

	harness.Detail(t, "image", image, "volume_gib", volGiB, "instance", instanceID,
		"volume", volID, "retain_volume", retain, "sample_interval_secs", sampleInterval)

	w := runImagePull(t, fix, tgt, instanceID, volID, image, volGiB, sampleInterval)
	w.Retained = retain
	writeImagePullResult(t, w)

	harness.Detail(t, "guest_used_bytes", w.GuestUsedBytes, "samples", len(w.Samples),
		"pull_duration_secs", fmt.Sprintf("%.1f", w.PullDurationSecs))
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
func ensureImagePullInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
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

	instanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     eg.PubSubnetID,
		SGID:         sgID,
	})
	// EnsureInstance's own cleanup issues TerminateInstances without waiting
	// for the terminal state, which races the network cleanups above: a live
	// run hit exactly this — DeleteWorkerEgress tried to delete eg.PubSubnetID
	// while the instance's ENI was still attached (still "shutting-down"),
	// failing with DependencyViolation. t.Cleanup is LIFO, so registering this
	// wait here — after EnsureInstance — makes it run BEFORE EnsureInstance's
	// own terminate cleanup and every network teardown below it, guaranteeing
	// the ENI is gone first. Terminating again here is idempotent with
	// whatever EnsureInstance's cleanup does later.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "terminated")
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d", host, port)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
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

// runImagePull attaches volID, formats and mounts it persistently, redirects
// the guest's container runtime onto that mount, pulls image in the
// background, and samples guest/host/backend state on an interval until the
// pull completes.
func runImagePull(t *testing.T, fix *Fixture, tgt harness.SSHTarget, instanceID, volID, image string, volGiB, sampleIntervalSecs int) imagePullWorkload {
	t.Helper()
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

	launchImagePullUnit(t, tgt, image)

	predaCli, err := newImagePullPredastoreS3(fix.Env.WANHost)
	require.NoError(t, err, "predastore s3 client")
	ssh := harness.NewPeerSSH()

	samples := make([]imagePullSample, 0, 64)
	start := time.Now()
	interval := time.Duration(sampleIntervalSecs) * time.Second

	for {
		s := captureImagePullSample(t, tgt, ssh, predaCli, fix.Env.WANHost, volID, start)
		samples = append(samples, s)
		harness.Detail(t, "sample_elapsed_s", fmt.Sprintf("%.1f", s.ElapsedSecs),
			"guest_used", s.GuestUsedBytes, "host_allocated", s.HostAllocatedBytes,
			"nbdkit_rss_kib", s.NbdkitRSSKiB, "checkpoint_bytes", s.CheckpointBytes)

		status, done, failed := imagePullUnitStatus(t, tgt)
		if failed {
			logOut, _ := harness.GuestExec(tgt, "journalctl -u mulga-imagepull --no-pager 2>/dev/null | tail -n 100")
			t.Fatalf("image pull unit failed (status=%s):\n%s", status, logOut)
		}
		if done {
			break
		}
		if time.Since(start) > imagePullMaxDuration {
			logOut, _ := harness.GuestExec(tgt, "journalctl -u mulga-imagepull --no-pager 2>/dev/null | tail -n 100")
			t.Fatalf("image pull did not complete within %s (status=%s):\n%s", imagePullMaxDuration, status, logOut)
		}
		time.Sleep(interval)
	}
	// One final sample right after the completion marker appears, so the last
	// data point reflects settled state rather than whatever the last
	// periodic tick happened to catch mid-write.
	samples = append(samples, captureImagePullSample(t, tgt, ssh, predaCli, fix.Env.WANHost, volID, start))

	finalUsed := samples[len(samples)-1].GuestUsedBytes
	require.Positivef(t, finalUsed, "guest reported 0 used bytes on %s after pulling %s — the denominator every downstream ratio divides by is unusable", imagePullMount, image)

	verifyImagePullLandedOnVolume(t, tgt)

	return imagePullWorkload{
		Image:               image,
		VolumeGiB:           volGiB,
		VolumeID:            volID,
		GuestUsedBytes:      finalUsed,
		Samples:             samples,
		SampleIntervalSecs:  sampleIntervalSecs,
		PullDurationSecs:    time.Since(start).Seconds(),
		DrainCountAvailable: false,
		DrainCountNote: "viperblock/telemetry exposes no per-drain or checkpoint-write counter: " +
			"only backend-IO, WAL-op, and cache-lookup metrics are recorded, none keyed to " +
			"distinguish a checkpoint write from an ordinary chunk write. A precise count needs " +
			"either a viperblock-side counter (out of scope for this workload) or predastore " +
			"access-log parsing for PUT .../checkpoints/blocks.live.bin, which requires debug " +
			"logging predastore does not run with by default. Each sample's " +
			"checkpoint_last_modified is the available mitigation: since SaveLiveCheckpointCtx " +
			"always rewrites the fixed key <volume>/checkpoints/blocks.live.bin in place, a " +
			"change in that timestamp between two samples is evidence a drain occurred between " +
			"them, and lower-bounds the drain count at this sample interval.",
	}
}

// launchImagePullUnit writes imagePullGuestScript to the guest and starts it
// as a detached systemd transient unit. The script is base64-encoded rather
// than heredoc'd directly into the SSH command: it contains its own heredoc
// (for /etc/docker/daemon.json) and shell metacharacters, and base64 avoids
// having to reason about two layers of shell quoting (the local ssh command
// string and the remote shell it runs) nesting correctly.
func launchImagePullUnit(t *testing.T, tgt harness.SSHTarget, image string) {
	t.Helper()
	script := fmt.Sprintf(imagePullGuestScript, imagePullMount, image)
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
func imagePullUnitStatus(t *testing.T, tgt harness.SSHTarget) (status string, done, failed bool) {
	t.Helper()
	out, err := harness.GuestExec(tgt, "cat /tmp/run-pull.status 2>/dev/null || echo unknown")
	status = strings.TrimSpace(out)
	if err != nil {
		// A transient SSH hiccup mid-pull is not itself a failure signal;
		// the next tick tries again.
		return status, false, false
	}
	rc, rcErr := harness.GuestExec(tgt, "cat /tmp/run-pull.exitcode 2>/dev/null || true")
	rc = strings.TrimSpace(rc)
	if rcErr == nil && rc != "" {
		return status, true, rc != "0"
	}
	return status, false, false
}

// captureImagePullSample takes one guest/host/backend reading. Every failure
// here is logged, not fatal: a single bad reading mid-pull (a busy SSH
// session, a du that raced a write) must not abort a run that would otherwise
// complete, and a sparse sample series is still useful — only an entirely
// empty one is not, which the caller's final require.Positivef on the guest
// side guards against.
func captureImagePullSample(t *testing.T, tgt harness.SSHTarget, ssh *harness.PeerSSH, predaCli *s3.S3, wanHost, volID string, start time.Time) imagePullSample {
	t.Helper()
	s := imagePullSample{ElapsedSecs: time.Since(start).Seconds()}

	if out, err := harness.GuestExec(tgt, fmt.Sprintf("df --output=used -B1 %s", imagePullMount)); err != nil {
		t.Logf("storagegrowth: sample guest df on %s: %v: %s", imagePullMount, err, strings.TrimSpace(out))
	} else if v, perr := parseLastIntField(out); perr != nil {
		t.Logf("storagegrowth: parse guest df output %q: %v", out, perr)
	} else {
		s.GuestUsedBytes = v
	}

	duCtx, duCancel := context.WithTimeout(context.Background(), 30*time.Second)
	duOut, duErr := ssh.Run(duCtx, wanHost, fmt.Sprintf("sudo du -s --block-size=1 %s 2>/dev/null | cut -f1", predastoreBaseDir))
	duCancel()
	if duErr != nil {
		t.Logf("storagegrowth: host du via PeerSSH on %s: %v", wanHost, duErr)
	} else if v, perr := parseLastIntField(string(duOut)); perr != nil {
		t.Logf("storagegrowth: parse host du output %q: %v", duOut, perr)
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

	key := fmt.Sprintf("%s/checkpoints/blocks.live.bin", volID)
	headCtx, headCancel := context.WithTimeout(context.Background(), 15*time.Second)
	head, headErr := predaCli.HeadObjectWithContext(headCtx, &s3.HeadObjectInput{
		Bucket: aws.String(predastoreBucket),
		Key:    aws.String(key),
	})
	headCancel()
	switch {
	case headErr == nil:
		s.CheckpointBytes = aws.Int64Value(head.ContentLength)
		if head.LastModified != nil {
			s.CheckpointLastModified = head.LastModified.UTC().Format(time.RFC3339Nano)
		}
	case isImagePullNotFound(headErr):
		// Expected before the first drain has fired; not logged as a warning.
	default:
		t.Logf("storagegrowth: head %s/%s: %v", predastoreBucket, key, headErr)
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

// newImagePullPredastoreS3 builds an S3 client pointed at predastore's own
// endpoint on host, port predastoreHealthPort — the gateway on :9999 does not
// proxy S3 object operations. Mirrors tests/e2e/harness/predastore.go's
// unexported newPredastoreS3, which is not reachable from this package;
// tests/e2e/harness is deliberately never edited by this suite (see the
// package doc in main_test.go).
func newImagePullPredastoreS3(host string) (*s3.S3, error) {
	if host == "" {
		return nil, errors.New("storagegrowth: empty predastore host")
	}
	endpoint := "https://" + net.JoinHostPort(host, strconv.Itoa(predastoreHealthPort))
	cfg := &aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(imagePullGetenv("SPINIFEX_AWS_REGION", "ap-southeast-2")),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
	opts := session.Options{Config: *cfg}
	if id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		cfg.Credentials = credentials.NewStaticCredentials(id, secret, "")
		opts.Config = *cfg
	} else {
		opts.SharedConfigState = session.SharedConfigEnable
		opts.Profile = imagePullGetenv("AWS_PROFILE", "spinifex")
	}
	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("storagegrowth: predastore s3 session: %w", err)
	}
	return s3.New(sess), nil
}

// isImagePullNotFound reports whether err is the "object does not exist"
// error predastore returns for a HeadObject on a missing key. HeadObject's S3
// contract returns a generic "NotFound" (mapped from a 404 with no body),
// unlike GetObject's more specific NoSuchKey, so both codes are accepted.
func isImagePullNotFound(err error) bool {
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		switch aerr.Code() {
		case s3.ErrCodeNoSuchKey, "NotFound":
			return true
		}
	}
	return false
}

// imagePullGetenv returns the named environment variable, or def when unset.
// A trivial local copy of harness's unexported getenv (env.go), which this
// suite does not import from for the same reason as newImagePullPredastoreS3.
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

// writeImagePullResult persists the workload record at the path named by
// npassResultPathEnv (SPINIFEX_STORAGEGROWTH_RESULT_PATH) — the same env var
// npass uses, per the shared result contract. See writeNPassResult for why an
// unset path only logs, and why a set path treats every failure as fatal.
func writeImagePullResult(t *testing.T, w imagePullWorkload) {
	t.Helper()
	data, err := json.MarshalIndent(w, "", "  ")
	require.NoError(t, err, "marshal image-pull workload record")

	path := os.Getenv(npassResultPathEnv)
	if path == "" {
		harness.Detail(t, "workload_record", string(data))
		return
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "result dir for %s", path)
	require.NoError(t, os.WriteFile(path, data, 0o644), "write %s", path)
	harness.Detail(t, "workload_result", path)
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
