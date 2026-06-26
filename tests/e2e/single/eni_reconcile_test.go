//go:build e2e

package single

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runENIHotplugReconcile proves the Sprint 3d hot-plug reconciler end to end:
// attach a secondary ENI, restart spinifex-daemon (the guest QEMU survives via
// KillMode=process), then assert the ENI stays attached and is still fully
// manageable — DescribeNetworkInterfaces reports in-use, the guest NIC is still
// present, and a detach succeeds. The detach is the load-bearing assertion: the
// hot-attach mutates the in-memory PCIe slot map, which may not have been
// persisted before the restart, so without the reconciler re-adopting the slot
// the detach would fail InvalidAttachmentID.NotFound. OVN-gated; restores the
// singleton (detach + delete) before returning.
func runENIHotplugReconcile(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — ENI Hot-Plug Reconciler Convergence (OVN)")
	harness.SkipIfNoOVN(t)
	requireSSHHealthy(t)

	c := fix.AWS
	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	_, keyPath := needKeyPair(t, fix)

	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.SubnetID, "default subnet ID required")
	require.NotEmpty(t, def.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, c, def.SGID)

	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	before := guestMACSet(t, tgt)

	// --- Attach a secondary ENI -------------------------------------------
	harness.Step(t, "create-network-interface subnet=%s", def.SubnetID)
	// e2e:allow-create — the secondary ENI is the subject under test.
	cniOut, err := c.EC2.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(def.SubnetID),
		Groups:      []*string{aws.String(def.SGID)},
		Description: aws.String("e2e-eni-reconcile"),
	})
	require.NoError(t, err, "create-network-interface")
	require.NotNil(t, cniOut.NetworkInterface, "create-network-interface returned nil ENI")
	eniID := aws.StringValue(cniOut.NetworkInterface.NetworkInterfaceId)
	eniMAC := strings.ToLower(aws.StringValue(cniOut.NetworkInterface.MacAddress))
	require.NotEmpty(t, eniID, "ENI NetworkInterfaceId empty")
	require.NotEmpty(t, eniMAC, "ENI MacAddress empty")
	harness.Detail(t, "eni", eniID, "eni_mac", eniMAC)

	t.Cleanup(func() {
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
	attachmentID := aws.StringValue(attachOut.AttachmentId)
	require.NotEmpty(t, attachmentID, "AttachmentId empty")
	detached := false
	t.Cleanup(func() {
		if detached {
			return
		}
		if _, derr := c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachmentID),
		}); derr != nil && !harness.ErrorCodeIs(derr, "InvalidAttachmentID.NotFound") {
			t.Logf("WARNING: cleanup detach ENI %s: %v", eniID, derr)
		}
		_ = waitForENIStatusSoft(c, eniID, "available", 2*time.Minute)
	})

	waitForENIAttached(t, c, eniID, instanceID)
	require.NotContains(t, before, eniMAC, "ENI MAC present before attach — cannot prove a fresh hotplug")
	harness.Step(t, "assert guest NIC with MAC %s appears", eniMAC)
	harness.EventuallyErr(t, func() error {
		if _, ok := guestMACSet(t, tgt)[eniMAC]; ok {
			return nil
		}
		return fmt.Errorf("ENI MAC %s not yet visible in guest", eniMAC)
	}, 90*time.Second, 3*time.Second)

	// --- Restart the daemon; the guest must survive ------------------------
	harness.Step(t, "restart spinifex-daemon (guest QEMU survives)")
	restartSpinifexDaemon(t)
	waitForControlPlaneReady(t, c, instanceID)

	// --- Post-restart: ENI still attached + guest NIC intact --------------
	harness.Step(t, "assert ENI %s still attached after restart", eniID)
	waitForENIAttached(t, c, eniID, instanceID)
	require.Contains(t, guestMACSet(t, tgt), eniMAC,
		"guest NIC for MAC %s vanished across daemon restart — QEMU should survive", eniMAC)

	// --- Detach: succeeds only if the reconciler re-adopted the slot ------
	harness.Step(t, "detach-network-interface %s (proves slot re-adoption)", attachmentID)
	detachAfterReconcile(t, c, attachmentID)
	waitForENIStatus(t, c, eniID, "available")
	detached = true

	harness.Step(t, "verify guest NIC for MAC %s disappears", eniMAC)
	if !guestNICDropped(t, tgt, eniMAC, 45*time.Second) {
		t.Logf("WARN: guest NIC for MAC %s still present after detach (udev lag?)", eniMAC)
	}

	harness.Step(t, "delete-network-interface %s", eniID)
	_, err = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	})
	require.NoError(t, err, "delete-network-interface")
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
