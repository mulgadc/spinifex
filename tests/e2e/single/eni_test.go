//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runENIHotplug exercises the secondary-ENI hotplug datapath end to end:
// CreateNetworkInterface in the singleton's subnet, AttachNetworkInterface at
// DeviceIndex 1 to the running guest, then assert the new virtio-net NIC
// surfaces inside the guest kernel carrying the ENI's MAC. Proves the QMP
// device_add path (gateway -> ec2.cmd.<id> daemon -> QEMU hot-plug) reaches
// the guest, not just the control plane. OVN-gated; reuses the singleton VM
// and restores it (detach + delete) before returning.
func runENIHotplug(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Secondary ENI Hotplug Datapath (OVN)")
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

	// SSH to the singleton via its public IP — the in-guest `ip link` snapshot
	// needs a live datapath. Fatal (with sticky-skip) if the handshake never
	// completes; an unreachable singleton is a datapath bug, not test flake.
	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	// Snapshot the guest's MAC inventory before the hotplug so we detect the
	// brand-new interface rather than match a NIC that already existed.
	before := guestMACSet(t, tgt)
	harness.Detail(t, "guest_macs_before", strings.Join(setKeys(before), ","))

	// --- CreateNetworkInterface -------------------------------------------
	harness.Step(t, "create-network-interface subnet=%s", def.SubnetID)
	// e2e:allow-create — the secondary ENI is the subject hot-plugged under test.
	cniOut, err := c.EC2.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(def.SubnetID),
		Groups:      []*string{aws.String(def.SGID)},
		Description: aws.String("e2e-eni-hotplug"),
	})
	require.NoError(t, err, "create-network-interface")
	require.NotNil(t, cniOut.NetworkInterface, "create-network-interface returned nil ENI")
	eniID := aws.StringValue(cniOut.NetworkInterface.NetworkInterfaceId)
	eniMAC := strings.ToLower(aws.StringValue(cniOut.NetworkInterface.MacAddress))
	eniIP := aws.StringValue(cniOut.NetworkInterface.PrivateIpAddress)
	require.NotEmpty(t, eniID, "ENI NetworkInterfaceId empty")
	require.NotEmpty(t, eniMAC, "ENI MacAddress empty")
	require.NotEmpty(t, eniIP, "ENI PrivateIpAddress empty")
	harness.Detail(t, "eni", eniID, "eni_mac", eniMAC, "eni_ip", eniIP)

	// Delete cleanup runs last (LIFO); the detach cleanup registered next must
	// flip the ENI back to "available" before this fires.
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
		}); derr != nil && !harness.ErrorCodeIs(derr, "InvalidNetworkInterfaceID.NotFound") {
			t.Logf("WARNING: cleanup delete ENI %s: %v", eniID, derr)
		}
	})

	// --- AttachNetworkInterface (hot-plug) --------------------------------
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
	harness.Detail(t, "attachment", attachmentID)

	// --- Control plane: ENI shows attached to the singleton ---------------
	harness.Step(t, "wait ENI %s in-use attached to %s", eniID, instanceID)
	waitForENIAttached(t, c, eniID, instanceID)

	// --- Datapath: new NIC surfaces in the guest carrying the ENI MAC -----
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

	// Cross-check the control-plane secondary IP is the one assigned to the
	// hotplugged ENI. In-guest IP autoconfiguration of the secondary NIC is
	// OS/cloud-init dependent, so the IP assertion stays at the API level.
	eni, err := describeENI(c, eniID)
	require.NoError(t, err, "describe ENI %s after attach", eniID)
	require.Equal(t, eniIP, aws.StringValue(eni.PrivateIpAddress),
		"attached ENI %s private IP drifted from create-time value", eniID)

	// --- Detach + assert the NIC drops out of the guest -------------------
	harness.Step(t, "detach-network-interface %s", attachmentID)
	_, err = c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String(attachmentID),
	})
	require.NoError(t, err, "detach-network-interface")
	waitForENIStatus(t, c, eniID, "available")
	detached = true

	// Guest NIC removal is a soft check: virtio-net unplug + udev cleanup can
	// lag the control-plane detach, and a lingering link doesn't break the EC2
	// contract the way a missing hotplug would. Poll briefly, warn if it stays.
	harness.Step(t, "verify guest NIC for MAC %s disappears", eniMAC)
	dropped := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := guestMACSet(t, tgt)[eniMAC]; !ok {
			dropped = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if dropped {
		harness.Detail(t, "datapath", "guest_nic_removed_ok")
	} else {
		t.Logf("WARN: guest NIC for MAC %s still present after detach (udev lag?)", eniMAC)
	}

	// --- DeleteNetworkInterface -------------------------------------------
	harness.Step(t, "delete-network-interface %s", eniID)
	_, err = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	})
	require.NoError(t, err, "delete-network-interface")
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
