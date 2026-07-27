//go:build e2e

package single

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guestChurnSentinelLabel / guestChurnSentinelSizeMiB parameterise the
// durability sentinel written once in WriteSentinel and re-checked after
// every later perturbation. A 1 GiB volume is the AWS minimum; the 4 MiB
// random payload is large enough to be a meaningful checksum target while
// keeping the predastore round-trip fast.
const (
	guestChurnSentinelLabel   = "e2edur"
	guestChurnSentinelSizeMiB = 4
)

// guestChurnRounds returns how many times the full perturbation sequence
// (HotplugENI through Reboot) repeats against the same guest and sentinel,
// overridable via SPINIFEX_GUESTCHURN_ROUNDS. Defaults to 1, matching every
// former standalone test's single-pass behaviour; bump it for a soak run
// that repeatedly churns the guest to catch reconciler or durability
// flakiness a single pass would not surface.
func guestChurnRounds() int {
	return envPositiveIntOr("SPINIFEX_GUESTCHURN_ROUNDS", 1)
}

// runGuestChurnDurability merges six checks that used to boot their own
// singleton apiece — SSH/root-volume probe, ENI hotplug, the hot-plug
// reconciler, volume data durability, ModifyInstanceAttribute, and reboot —
// around one shared guest and one shared data-integrity sentinel.
//
// This merge is not just a time-saver: today VolumeDurability alone proves a
// sha256 survives one detach/reattach and one stop/start. Merged, the same
// sha is re-verified after ENI hotplug, a daemon restart, an instance-type
// change, a fresh stop/start, and a reboot — proving the write path holds up
// across every kind of churn this suite exercises, not just one of them.
//
// Stage order and gating:
//   - BootAndRootVolume boots (or adopts) the singleton and proves basic
//     SSH and root-volume sanity. Nothing below it can run against a guest
//     that never booted, so its failure is fatal for the whole scenario
//     (require in the ordinary Go-test sense, via a hard t.Fatalf gate).
//   - WriteSentinel creates the data volume, writes the sentinel, and proves
//     it survives one detach/reattach before any perturbation begins. Its
//     failure is likewise fatal: every later re-verification compares
//     against the sha captured here, so there is nothing to re-verify if
//     this stage never captured a good one.
//   - Every stage from HotplugENI onward is a perturbation of the running
//     guest; a re-verification of the sentinel sha immediately follows each
//     one, using require — a corrupted sentinel invalidates every later
//     perturbation's result, so continuing past a bad readback would only
//     produce misleading noise.
//   - HotplugENI, DaemonRestartReadoptsSlot, and DetachENI form one ENI
//     lifecycle: hotplug, prove the reconciler re-adopts the slot across a
//     daemon restart, then detach. They share one ENI rather than each
//     creating its own, which is what lets DaemonRestartReadoptsSlot prove
//     reconciliation against an ENI that was already fully verified live,
//     and what lets the final detach double as both "a working ENI detaches
//     cleanly" and "detach succeeds only if the reconciler re-adopted the
//     slot". OVN-gated (see below): if HotplugENI does not attach an ENI —
//     whether skipped or failed — DaemonRestartReadoptsSlot and DetachENI
//     do not run, since they have nothing to act on. This does NOT gate
//     ResizeInstanceType, SurvivesStopStart, or Reboot: those perturb the
//     guest and its data volume, not the secondary ENI, and must still run
//     (and still get their own sentinel re-verification) in an environment
//     with no OVN.
//   - ResizeInstanceType's own t.Cleanup restores the original instance
//     type and running state at the end of that stage specifically — not
//     deferred to the end of the round — so SurvivesStopStart and Reboot
//     always start from a clean, running, original-type instance regardless
//     of whether ResizeInstanceType itself passed.
//   - SurvivesStopStart's own body already re-reads and compares the
//     sentinel sha as its defining assertion, so no separate
//     re-verification call follows it — that would just be the same guest
//     exec repeated for nothing new.
//   - Reboot never touched the data volume in its original standalone form;
//     the sentinel re-verification after it is new coverage this merge
//     adds, not a preserved assertion.
//
// The whole sequence from HotplugENI through Reboot repeats for
// guestChurnRounds() rounds (default 1); each round gets its own ENI and its
// own instance-type round-trip so a soak run exercises fresh resource
// lifecycles rather than replaying stale IDs.
func runGuestChurnDurability(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Guest Churn Durability: sentinel survives ENI hotplug, daemon restart, type change, reboot, and stop/start")

	// scenarioT anchors cleanup that must outlive a single t.Run block — the
	// sentinel volume, in particular, must survive every stage and every
	// round below, not just the stage that created it.
	scenarioT := t

	az := needAZ(t, fix)
	origType, _ := needInstanceTypeArch(t, fix)
	_, keyPath := needKeyPair(t, fix)

	var instanceID string

	bootOK := t.Run("BootAndRootVolume", func(t *testing.T) {
		inst, rootVolumeID := needInstance(t, fix)
		instanceID = aws.StringValue(inst.InstanceId)

		host, port := harness.InstancePublicSSHHost(t, inst)
		harness.Detail(t, "ssh_host", host, "ssh_port", port)

		addr := net.JoinHostPort(host, strconv.Itoa(port))
		harness.Step(t, "waiting for SSH handshake %s", addr)
		waitForSSHHandshake(t, host, port, keyPath)
		_ = addr

		tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

		harness.Step(t, "ssh id")
		idOut := runSSH(t, tgt, "id")
		assert.Containsf(t, idOut, "ubuntu", "ssh id should report ubuntu\n%s", idOut)

		harness.Step(t, "lsblk root-volume cross-check vs API")
		guestGiB := harness.LsblkRootGiB(t, tgt)

		vols, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(rootVolumeID)},
		})
		require.NoError(t, err, "describe-volumes %s", rootVolumeID)
		require.NotEmpty(t, vols.Volumes, "no volume for %s", rootVolumeID)
		apiGiB := int(aws.Int64Value(vols.Volumes[0].Size))

		// lsblk rounds down — bash treats equality as required, but on backing
		// stores where the VM's view is a hair under the API size the rounding
		// loses 1 GiB. Allow ±1 to match the bash intent without flaking on it.
		diff := guestGiB - apiGiB
		if diff < 0 {
			diff = -diff
		}
		assert.LessOrEqualf(t, diff, 1,
			"root volume size mismatch: guest=%d GiB api=%d GiB", guestGiB, apiGiB)
		harness.Detail(t, "guest_gib", guestGiB, "api_gib", apiGiB)

		harness.Step(t, "ssh hostname")
		hn := strings.TrimSpace(runSSH(t, tgt, "hostname"))
		// Bash uses `spinifex-vm-<first 8 hex chars of instance ID>` and treats
		// a missing prefix as a non-fatal warning. Replicate the soft check via
		// t.Logf rather than asserting — the spx hostname format is not part of
		// the EC2 surface contract.
		shortID := strings.TrimPrefix(instanceID, "i-")
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		if strings.Contains(hn, shortID) {
			harness.Detail(t, "hostname", hn, "matches_short_id", shortID)
		} else {
			t.Logf("hostname %q does not contain short id %q (non-fatal)", hn, shortID)
		}
	})
	if !bootOK {
		t.Fatalf("BootAndRootVolume stage failed; skipping every later stage — the guest never booted")
	}

	var wantSha string
	writeOK := t.Run("WriteSentinel", func(t *testing.T) {
		tgt := resolveGuestChurnTarget(t, fix, instanceID, keyPath)

		harness.Step(t, "create-volume size=1 az=%s", az)
		// e2e:allow-create — the sentinel volume is the subject under test.
		createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(1),
		})
		require.NoError(t, err, "create-volume")
		volID := aws.StringValue(createOut.VolumeId)
		require.NotEmpty(t, volID, "CreateVolume returned empty VolumeId")
		harness.Detail(t, "volume", volID, "encrypted", aws.BoolValue(createOut.Encrypted))
		assert.Truef(t, aws.BoolValue(createOut.Encrypted),
			"CreateVolume.Encrypted should be true (on-by-default cluster master key); "+
				"false means the control-plane→viperblockd key wiring is broken")
		// Registered on scenarioT, not this stage's own t: the volume must
		// survive every stage and round below, and only get torn down when
		// the whole merged scenario ends.
		harness.RegisterVolumeTeardown(scenarioT, fix.AWS, volID)
		harness.WaitForVolumeState(t, fix.AWS, volID, "available", harness.WithPoll(500*time.Millisecond))

		before := harness.GuestDiskSet(t, tgt)
		harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, "/dev/sdf")
		dev := harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
		harness.Detail(t, "guest_dev", dev)
		wantSha = harness.GuestFormatWriteSentinel(t, tgt, dev, guestChurnSentinelLabel, guestChurnSentinelSizeMiB)
		harness.Detail(t, "sha256", wantSha)

		// Detach → reattach → re-read by device; sha must survive. Proves the
		// write itself round-tripped through the backend before any
		// perturbation below begins.
		harness.DetachVolumeWait(t, fix.AWS, volID)
		before = harness.GuestDiskSet(t, tgt)
		harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, "/dev/sdf")
		dev = harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
		gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/"+dev, guestChurnSentinelLabel)
		require.Equalf(t, wantSha, gotSha, "sha256 mismatch after detach/reattach")
		harness.Detail(t, "reattach_sha_ok", gotSha)
	})
	if !writeOK {
		t.Fatalf("WriteSentinel stage failed; skipping every perturbation stage — there is no sentinel to re-verify")
	}

	// verifySentinel re-reads the sentinel by ext4 label (robust to device
	// renumbering across the perturbations below) and requires it match the
	// sha captured in WriteSentinel. Always require: a corrupted sentinel
	// invalidates every later perturbation's result.
	verifySentinel := func(t *testing.T, stage string) {
		t.Helper()
		tgt := resolveGuestChurnTarget(t, fix, instanceID, keyPath)
		gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/disk/by-label/"+guestChurnSentinelLabel, guestChurnSentinelLabel)
		require.Equalf(t, wantSha, gotSha, "sentinel sha256 mismatch after %s", stage)
		harness.Detail(t, "sentinel_verified_after", stage, "sha256", gotSha)
	}

	rounds := guestChurnRounds()
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("Round%d", round), func(t *testing.T) {
			runGuestChurnRound(t, fix, instanceID, origType, keyPath, wantSha, verifySentinel)
		})
	}
}

// runGuestChurnRound drives one pass of HotplugENI through Reboot against
// the already-booted guest and already-written sentinel.
func runGuestChurnRound(t *testing.T, fix *Fixture, instanceID, origType, keyPath, wantSha string, verifySentinel func(t *testing.T, stage string)) {
	// roundT anchors ENI cleanup: the ENI created in HotplugENI must survive
	// DaemonRestartReadoptsSlot and DetachENI, i.e. outlive its own creating
	// subtest, so its cleanup is registered on the round's own t rather than
	// the HotplugENI subtest's t.
	roundT := t
	c := fix.AWS

	var eniID, eniMAC, attachmentID string
	eniAttached := false
	detachedByStage := false

	t.Run("HotplugENI", func(t *testing.T) {
		harness.SkipIfNoOVN(t)
		requireSSHHealthy(t)

		tgt := resolveGuestChurnTarget(t, fix, instanceID, keyPath)
		def := harness.EnsureDefaultVPC(t, fix.Harness)
		require.NotEmpty(t, def.SubnetID, "default subnet ID required")
		require.NotEmpty(t, def.SGID, "default SG ID required")
		harness.AuthorizeSSHIngress(t, c, def.SGID)

		// Snapshot the guest's MAC inventory before the hotplug so we detect
		// the brand-new interface rather than match a NIC that already existed.
		before := guestMACSet(t, tgt)
		harness.Detail(t, "guest_macs_before", strings.Join(setKeys(before), ","))

		harness.Step(t, "create-network-interface subnet=%s", def.SubnetID)
		// e2e:allow-create — the secondary ENI is the subject hot-plugged under test.
		cniOut, err := c.EC2.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
			SubnetId:    aws.String(def.SubnetID),
			Groups:      []*string{aws.String(def.SGID)},
			Description: aws.String("e2e-eni-hotplug"),
		})
		require.NoError(t, err, "create-network-interface")
		require.NotNil(t, cniOut.NetworkInterface, "create-network-interface returned nil ENI")
		eniID = aws.StringValue(cniOut.NetworkInterface.NetworkInterfaceId)
		eniMAC = strings.ToLower(aws.StringValue(cniOut.NetworkInterface.MacAddress))
		eniIP := aws.StringValue(cniOut.NetworkInterface.PrivateIpAddress)
		require.NotEmpty(t, eniID, "ENI NetworkInterfaceId empty")
		require.NotEmpty(t, eniMAC, "ENI MacAddress empty")
		require.NotEmpty(t, eniIP, "ENI PrivateIpAddress empty")
		harness.Detail(t, "eni", eniID, "eni_mac", eniMAC, "eni_ip", eniIP)

		// Delete cleanup runs last (LIFO); the detach cleanup registered next
		// must flip the ENI back to "available" before this fires.
		roundT.Cleanup(func() {
			if _, derr := c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			}); derr != nil && !harness.ErrorCodeIs(derr, "InvalidNetworkInterfaceID.NotFound") {
				t.Logf("WARNING: cleanup delete ENI %s: %v", eniID, derr)
			}
		})

		harness.Step(t, "attach-network-interface %s -> %s device-index=1", eniID, instanceID)
		attachOut, err := c.EC2.AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
			InstanceId:         aws.String(instanceID),
			DeviceIndex:        aws.Int64(1),
		})
		require.NoError(t, err, "attach-network-interface")
		attachmentID = aws.StringValue(attachOut.AttachmentId)
		require.NotEmpty(t, attachmentID, "AttachmentId empty")
		roundT.Cleanup(func() {
			if detachedByStage {
				return
			}
			if _, derr := c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
				AttachmentId: aws.String(attachmentID),
			}); derr != nil && !harness.ErrorCodeIs(derr, "InvalidAttachmentID.NotFound") {
				t.Logf("WARNING: cleanup detach ENI %s: %v", eniID, derr)
			}
			_ = waitForENIStatusSoft(c, eniID, "available", 2*time.Minute)
		})
		harness.Detail(t, "attachment", attachmentID)

		harness.Step(t, "wait ENI %s in-use attached to %s", eniID, instanceID)
		waitForENIAttached(t, c, eniID, instanceID)

		harness.Step(t, "assert guest NIC with MAC %s appears", eniMAC)
		if _, existed := before[eniMAC]; existed {
			t.Fatalf("ENI MAC %s was already present before attach — cannot prove a fresh hotplug", eniMAC)
		}
		harness.EventuallyErr(t, func() error {
			now := guestMACSet(t, tgt)
			if _, ok := now[eniMAC]; ok {
				return nil
			}
			return fmt.Errorf("ENI MAC %s not yet visible in guest (have: %s)",
				eniMAC, strings.Join(setKeys(now), ","))
		}, 90*time.Second, 3*time.Second)
		harness.Detail(t, "datapath", "guest_nic_hotplugged_ok")

		// Cross-check the control-plane secondary IP is the one assigned to
		// the hotplugged ENI. In-guest IP autoconfiguration of the secondary
		// NIC is OS/cloud-init dependent, so the IP assertion stays at the
		// API level.
		eni, err := describeENI(c, eniID)
		require.NoError(t, err, "describe ENI %s after attach", eniID)
		require.Equal(t, eniIP, aws.StringValue(eni.PrivateIpAddress),
			"attached ENI %s private IP drifted from create-time value", eniID)

		eniAttached = true
	})
	verifySentinel(t, "HotplugENI")

	if eniAttached {
		t.Run("DaemonRestartReadoptsSlot", func(t *testing.T) {
			tgt := resolveGuestChurnTarget(t, fix, instanceID, keyPath)

			// Restart the daemon; the guest must survive.
			harness.Step(t, "restart spinifex-daemon (guest QEMU survives)")
			restartSpinifexDaemon(t)
			waitForControlPlaneReady(t, c, instanceID)

			// Post-restart: ENI still attached + guest NIC intact.
			harness.Step(t, "assert ENI %s still attached after restart", eniID)
			waitForENIAttached(t, c, eniID, instanceID)
			require.Contains(t, guestMACSet(t, tgt), eniMAC,
				"guest NIC for MAC %s vanished across daemon restart — QEMU should survive", eniMAC)
		})
		verifySentinel(t, "DaemonRestartReadoptsSlot")

		t.Run("DetachENI", func(t *testing.T) {
			tgt := resolveGuestChurnTarget(t, fix, instanceID, keyPath)

			// Detach: succeeds only if the reconciler re-adopted the slot.
			harness.Step(t, "detach-network-interface %s (proves slot re-adoption)", attachmentID)
			detachAfterReconcile(t, c, attachmentID)
			waitForENIStatus(t, c, eniID, "available")
			detachedByStage = true

			// Guest NIC removal is a soft check: virtio-net unplug + udev
			// cleanup can lag the control-plane detach.
			harness.Step(t, "verify guest NIC for MAC %s disappears", eniMAC)
			if !guestNICDropped(t, tgt, eniMAC, 45*time.Second) {
				t.Logf("WARN: guest NIC for MAC %s still present after detach (udev lag?)", eniMAC)
			}

			harness.Step(t, "delete-network-interface %s", eniID)
			_, err := c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			})
			require.NoError(t, err, "delete-network-interface")
		})
		verifySentinel(t, "DetachENI")
	} else {
		t.Logf("no ENI attached this round (HotplugENI skipped or failed); skipping DaemonRestartReadoptsSlot and DetachENI")
	}

	t.Run("ResizeInstanceType", func(t *testing.T) {
		// Bash strips the ".nano" suffix and appends ".small" — same family,
		// more RAM at matching vCPU. Avoids xlarge (16 GiB) which the CI host
		// can't satisfy.
		if !strings.HasSuffix(origType, ".nano") {
			t.Fatalf("phase7b: expected discovered instance type to end with .nano, got %q", origType)
		}
		modifyType := strings.TrimSuffix(origType, ".nano") + ".small"
		harness.Detail(t, "from_type", origType, "to_type", modifyType)

		// Stop the instance first — ModifyInstanceAttribute on a running
		// instance is rejected. Restore (type + state) at the end of this
		// stage so SurvivesStopStart/Reboot see the canonical running
		// original-type instance regardless of whether this stage passed.
		harness.Step(t, "stop-instances %s (precondition for modify)", instanceID)
		_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "stop-instances")
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")
		t.Cleanup(func() {
			_, _ = fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
				InstanceIds: []*string{aws.String(instanceID)},
			})
			harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")
			_, _ = fix.AWS.EC2.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
				InstanceId:   aws.String(instanceID),
				InstanceType: &ec2.AttributeValue{Value: aws.String(origType)},
			})
			_, _ = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
				InstanceIds: []*string{aws.String(instanceID)},
			})
			harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
		})

		// Look up expected vCPUs / memory for the upsized type so we can
		// assert SSH-reported values match what the AWS surface advertises.
		typesOut, err := fix.AWS.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
		require.NoError(t, err, "describe-instance-types")
		var expectedVCPUs int64
		var expectedMemMiB int64
		for _, it := range typesOut.InstanceTypes {
			if aws.StringValue(it.InstanceType) != modifyType {
				continue
			}
			if it.VCpuInfo != nil {
				expectedVCPUs = aws.Int64Value(it.VCpuInfo.DefaultVCpus)
			}
			if it.MemoryInfo != nil {
				expectedMemMiB = aws.Int64Value(it.MemoryInfo.SizeInMiB)
			}
			break
		}
		require.NotZero(t, expectedVCPUs, "%s missing VCpuInfo.DefaultVCpus", modifyType)
		require.NotZero(t, expectedMemMiB, "%s missing MemoryInfo.SizeInMiB", modifyType)
		harness.Detail(t, "expected_vcpus", expectedVCPUs, "expected_mem_mib", expectedMemMiB)

		harness.Step(t, "modify-instance-attribute %s type=%s", instanceID, modifyType)
		_, err = fix.AWS.EC2.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
			InstanceId:   aws.String(instanceID),
			InstanceType: &ec2.AttributeValue{Value: aws.String(modifyType)},
		})
		require.NoError(t, err, "modify-instance-attribute")

		// Verify describe-instances reflects the new type before we start.
		descOut, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "describe-instances")
		require.NotEmpty(t, descOut.Reservations, "no reservations for %s", instanceID)
		require.NotEmpty(t, descOut.Reservations[0].Instances, "no instances for %s", instanceID)
		gotType := aws.StringValue(descOut.Reservations[0].Instances[0].InstanceType)
		require.Equalf(t, modifyType, gotType,
			"ModifyInstanceAttribute did not stick: want %s got %s", modifyType, gotType)

		harness.Step(t, "start-instances %s", instanceID)
		_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "start-instances")

		runInst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

		// Re-discover SSH endpoint — qemu hostfwd may have rebound.
		host, port := harness.InstancePublicSSHHost(t, runInst)
		harness.Detail(t, "ssh_host", host, "ssh_port", port)

		waitForSSHReady(t, host, port, keyPath)
		tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

		harness.Step(t, "ssh nproc")
		nprocOut := strings.TrimSpace(runSSH(t, tgt, "nproc"))
		vmVCPUs, err := strconv.ParseInt(nprocOut, 10, 64)
		require.NoErrorf(t, err, "parse nproc output %q", nprocOut)
		require.Equalf(t, expectedVCPUs, vmVCPUs,
			"vCPU mismatch after modify: VM=%d expected=%d", vmVCPUs, expectedVCPUs)
		harness.Detail(t, "vm_vcpus", vmVCPUs)

		harness.Step(t, "ssh MemTotal")
		memOut := strings.TrimSpace(runSSH(t, tgt, "awk '/MemTotal/ {print $2}' /proc/meminfo"))
		vmMemKB, err := strconv.ParseInt(memOut, 10, 64)
		require.NoErrorf(t, err, "parse MemTotal output %q", memOut)
		vmMemMiB := vmMemKB / 1024
		// 15% margin for kernel-reserved memory, matching the bash threshold.
		expectedMemLow := expectedMemMiB * 85 / 100
		require.GreaterOrEqualf(t, vmMemMiB, expectedMemLow,
			"memory too low after modify: VM=%d MiB expected>=%d MiB (target %d MiB)",
			vmMemMiB, expectedMemLow, expectedMemMiB)
		harness.Detail(t, "vm_mem_mib", vmMemMiB, "threshold_mib", expectedMemLow)

		// Cleanup restores original type + running state, registered above.
	})
	verifySentinel(t, "ResizeInstanceType")

	t.Run("SurvivesStopStart", func(t *testing.T) {
		// Stop/start (forces re-mount after instance restart) → re-read by
		// label, since the device name can shift across the restart. This
		// re-read IS the stage's defining assertion, so no separate
		// verifySentinel call follows it.
		harness.Step(t, "stop-instances %s", instanceID)
		_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "stop-instances")
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

		harness.Step(t, "start-instances %s", instanceID)
		_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "start-instances")
		runInst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

		host, port := harness.InstancePublicSSHHost(t, runInst)
		waitForSSHReady(t, host, port, keyPath)
		tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
		gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/disk/by-label/"+guestChurnSentinelLabel, guestChurnSentinelLabel)
		require.Equalf(t, wantSha, gotSha, "sha256 mismatch after stop/start")
		harness.Detail(t, "stopstart_sha_ok", gotSha)
	})

	t.Run("Reboot", func(t *testing.T) {
		inst := describeGuestChurnInstance(t, fix, instanceID)
		host, port := harness.InstancePublicSSHHost(t, inst)
		waitForSSHReady(t, host, port, keyPath)

		// Capture pre-reboot private IP for the post-reboot identity check.
		preDesc, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "describe-instances (pre-reboot)")
		require.NotEmpty(t, preDesc.Reservations, "no reservations for %s", instanceID)
		require.NotEmpty(t, preDesc.Reservations[0].Instances, "no instances for %s", instanceID)
		preRebootIP := aws.StringValue(preDesc.Reservations[0].Instances[0].PrivateIpAddress)
		harness.Detail(t, "pre_reboot_private_ip", preRebootIP)

		harness.Step(t, "reboot-instances %s", instanceID)
		_, err = fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "reboot-instances")

		// Bash polls 10× at 1s checking the state stays "running" — EC2's
		// reboot semantics don't transition the instance state at all.
		for i := 0; i < 10; i++ {
			out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(instanceID)},
			})
			require.NoError(t, err, "describe-instances during reboot poll %d", i)
			require.NotEmpty(t, out.Reservations[0].Instances, "instance disappeared during reboot")
			state := aws.StringValue(out.Reservations[0].Instances[0].State.Name)
			require.Equalf(t, "running", state,
				"instance unexpectedly left running state during reboot: %s", state)
			time.Sleep(1 * time.Second)
		}

		// SSH endpoint may have rebound after the guest restart (qemu hostfwd
		// can shift), so re-discover.
		descPost, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoError(t, err, "describe-instances (post-reboot)")
		require.NotEmpty(t, descPost.Reservations[0].Instances, "no instances post-reboot")
		postInst := descPost.Reservations[0].Instances[0]
		host, port = harness.InstancePublicSSHHost(t, postInst)
		waitForSSHReady(t, host, port, keyPath)

		tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
		harness.Step(t, "ssh uptime")
		uptimeOut := strings.TrimSpace(runSSH(t, tgt, "cat /proc/uptime | cut -d. -f1"))
		uptimeSecs, err := strconv.ParseInt(uptimeOut, 10, 64)
		require.NoErrorf(t, err, "parse uptime output %q", uptimeOut)
		require.LessOrEqualf(t, uptimeSecs, int64(120),
			"guest uptime %ds is > 120s — reboot may not have occurred", uptimeSecs)
		harness.Detail(t, "uptime_secs", uptimeSecs)

		postRebootIP := aws.StringValue(postInst.PrivateIpAddress)
		assert.Equalf(t, preRebootIP, postRebootIP,
			"PrivateIpAddress changed across reboot: %s -> %s", preRebootIP, postRebootIP)

		// Leave instance running — later stages/rounds expect the singleton
		// to still be up.
	})
	verifySentinel(t, "Reboot")
}

// describeGuestChurnInstance returns the current *ec2.Instance for
// instanceID. Every perturbation this scenario drives can change the
// instance's public hostfwd endpoint (type change, reboot, stop/start), so
// stages re-describe rather than trust an *ec2.Instance captured earlier.
func describeGuestChurnInstance(t *testing.T, fix *Fixture, instanceID string) *ec2.Instance {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "describe-instances %s", instanceID)
	require.NotEmpty(t, out.Reservations, "no reservations for %s", instanceID)
	require.NotEmpty(t, out.Reservations[0].Instances, "no instances for %s", instanceID)
	return out.Reservations[0].Instances[0]
}

// resolveGuestChurnTarget re-describes instanceID and returns a fresh SSH
// target, waiting for the handshake to succeed. The public hostfwd port can
// rebind across every perturbation this scenario drives, so every stage
// re-discovers it rather than trusting a target captured before the
// perturbation.
func resolveGuestChurnTarget(t *testing.T, fix *Fixture, instanceID, keyPath string) harness.SSHTarget {
	t.Helper()
	inst := describeGuestChurnInstance(t, fix, instanceID)
	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	return harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// guestMACSet returns the set of lowercased MAC addresses currently present on
// the guest's network interfaces, read from sysfs so the list tracks the live
// kernel view (including hotplugged NICs). The all-zero loopback MAC is
// dropped so it never masks a real match.
func guestMACSet(t *testing.T, tgt harness.SSHTarget) map[string]struct{} {
	t.Helper()
	out, err := harness.GuestExec(tgt, "cat /sys/class/net/*/address")
	if err != nil {
		t.Fatalf("guest read MAC inventory: %v\n%s", err, out)
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		mac := strings.ToLower(strings.TrimSpace(line))
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		set[mac] = struct{}{}
	}
	return set
}

// setKeys returns the keys of a string set as a slice for log/Detail output.
func setKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// describeENI returns the single ENI record for eniID, erroring if absent.
func describeENI(c *harness.AWSClient, eniID string) (*ec2.NetworkInterface, error) {
	out, err := c.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	})
	if err != nil {
		return nil, fmt.Errorf("describe-network-interfaces %s: %w", eniID, err)
	}
	if len(out.NetworkInterfaces) == 0 {
		return nil, fmt.Errorf("ENI %s not found", eniID)
	}
	return out.NetworkInterfaces[0], nil
}

// waitForENIAttached polls until eniID reports Status=in-use with an
// Attachment bound to instanceID.
func waitForENIAttached(t *testing.T, c *harness.AWSClient, eniID, instanceID string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		eni, err := describeENI(c, eniID)
		if err != nil {
			return err
		}
		status := aws.StringValue(eni.Status)
		if eni.Attachment == nil {
			return fmt.Errorf("ENI %s status=%s has no attachment yet", eniID, status)
		}
		if got := aws.StringValue(eni.Attachment.InstanceId); got != instanceID {
			return fmt.Errorf("ENI %s attached to %q want %q", eniID, got, instanceID)
		}
		if status != "in-use" {
			return fmt.Errorf("ENI %s status=%s want in-use", eniID, status)
		}
		return nil
	}, 90*time.Second, 3*time.Second)
}

// waitForENIStatus polls until eniID reaches target status (e.g. "available").
func waitForENIStatus(t *testing.T, c *harness.AWSClient, eniID, target string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		eni, err := describeENI(c, eniID)
		if err != nil {
			return err
		}
		if got := aws.StringValue(eni.Status); got != target {
			return fmt.Errorf("ENI %s status=%s want=%s", eniID, got, target)
		}
		return nil
	}, 90*time.Second, 3*time.Second)
}

// waitForENIStatusSoft is the cleanup-time variant of waitForENIStatus: never
// calls t.Fatal, just polls best-effort within timeout.
func waitForENIStatusSoft(c *harness.AWSClient, eniID, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		eni, err := describeENI(c, eniID)
		if err == nil && aws.StringValue(eni.Status) == target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ENI %s did not reach %s within %s", eniID, target, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// restartSpinifexDaemon restarts the host spinifex-daemon unit. The single-node
// suite runs on the host with passwordless sudo (same precedent as harness diag
// capture), so a local systemctl restart reaches the daemon without SSH.
func restartSpinifexDaemon(t *testing.T) {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "systemctl", "restart", "spinifex-daemon").CombinedOutput()
	require.NoErrorf(t, err, "restart spinifex-daemon: %s", strings.TrimSpace(string(out)))
}

// waitForControlPlaneReady polls DescribeInstances until the daemon has
// reconnected after a restart and serves the singleton again.
func waitForControlPlaneReady(t *testing.T, c *harness.AWSClient, instanceID string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		if err != nil {
			return fmt.Errorf("describe-instances after restart: %w", err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return fmt.Errorf("instance %s not yet visible after restart", instanceID)
		}
		return nil
	}, 90*time.Second, 3*time.Second)
}

// detachAfterReconcile issues the detach, retrying while the reconciler's
// startup sweep is still re-adopting the slot. InvalidAttachmentID.NotFound is
// the symptom of an un-adopted slot and is safe to retry (no state changed);
// any other error is fatal.
func detachAfterReconcile(t *testing.T, c *harness.AWSClient, attachmentID string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		_, err := c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachmentID),
		})
		if err == nil {
			return nil
		}
		if harness.ErrorCodeIs(err, "InvalidAttachmentID.NotFound") {
			return fmt.Errorf("slot not yet re-adopted by reconciler: %w", err)
		}
		t.Fatalf("detach-network-interface %s: %v", attachmentID, err)
		return nil
	}, 60*time.Second, 3*time.Second)
}

// guestNICDropped polls briefly for the guest NIC carrying mac to disappear.
// Soft: virtio-net unplug + udev cleanup can lag the control-plane detach.
func guestNICDropped(t *testing.T, tgt harness.SSHTarget, mac string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := guestMACSet(t, tgt)[mac]; !ok {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}
