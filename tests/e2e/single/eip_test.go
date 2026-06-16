//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runInstanceEIP exercises the instance-level Elastic IP datapath end to end:
// AllocateAddress -> AssociateAddress to a running instance -> reach the guest
// over SSH via the EIP -> DisassociateAddress -> assert the EIP no longer
// reaches the guest -> ReleaseAddress. Proves the OVN DNAT/SNAT flows track the
// association lifecycle, not just the control-plane record.
//
// Uses a dedicated throwaway instance, never the shared singleton: associating
// an EIP rewrites the instance's public IP, so reusing the singleton would
// corrupt its public-IP datapath for every downstream test. Pool-mode + OVN
// gated — the EIP needs external IPAM and OVN flows to be observable.
func runInstanceEIP(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("instance EIP datapath requires pool-mode networking (external IPAM)")
	}
	harness.Phase(t, "Single — Instance Elastic IP Datapath")
	harness.SkipIfNoOVN(t)

	c := fix.AWS
	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)

	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.SubnetID, "default subnet ID required")
	require.NotEmpty(t, def.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, c, def.SGID)

	// --- Throwaway EIP subject --------------------------------------------
	harness.Step(t, "run-instances (EIP subject)")
	// e2e:allow-create — dedicated throwaway subject; reusing the singleton would corrupt its public IP.
	runOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(def.SubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances eip subject")
	require.NotEmpty(t, runOut.Instances, "run-instances returned no Instances")
	instID := aws.StringValue(runOut.Instances[0].InstanceId)
	require.NotEmpty(t, instID, "InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(instID)},
		})
		_ = waitForInstanceStateSoft(c, instID, "terminated", 5*time.Minute)
	})

	inst := harness.WaitForInstanceState(t, c, instID, "running")
	privIP := aws.StringValue(inst.PrivateIpAddress)
	require.NotEmpty(t, privIP, "EIP subject has no PrivateIpAddress")
	harness.Detail(t, "instance", instID, "boot_public_ip", aws.StringValue(inst.PublicIpAddress))

	// --- AllocateAddress ---------------------------------------------------
	harness.Step(t, "allocate-address (vpc)")
	// e2e:allow-create — the Elastic IP is the resource under test.
	eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
	require.NoError(t, err, "allocate-address")
	allocID := aws.StringValue(eipOut.AllocationId)
	eipIP := aws.StringValue(eipOut.PublicIp)
	require.NotEmpty(t, allocID, "AllocationId empty")
	require.NotEmpty(t, eipIP, "EIP PublicIp empty")
	eipReleased := false
	t.Cleanup(func() {
		if eipReleased {
			return
		}
		_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	})
	harness.Detail(t, "eip", eipIP, "alloc", allocID)

	// --- AssociateAddress --------------------------------------------------
	harness.Step(t, "associate-address %s -> %s", eipIP, instID)
	assocOut, err := c.EC2.AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId: aws.String(allocID),
		InstanceId:   aws.String(instID),
	})
	require.NoError(t, err, "associate-address")
	assocID := aws.StringValue(assocOut.AssociationId)
	require.NotEmpty(t, assocID, "AssociationId empty")
	disassociated := false
	t.Cleanup(func() {
		if disassociated {
			return
		}
		_, _ = c.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{AssociationId: aws.String(assocID)})
	})
	harness.Detail(t, "assoc", assocID)

	// --- Datapath: reach the guest via the EIP ----------------------------
	// OVN needs a beat to install the DNAT/SNAT flows after the association
	// publishes; poll the SSH handshake rather than asserting immediately.
	harness.Step(t, "ssh to guest via EIP %s", eipIP)
	if !trySSHReady(eipIP, 22, keyPath, sshReadyBudget) {
		harness.DumpVPCFlowDiagnostics(t, c, instID,
			fmt.Sprintf("EIP SSH timeout — eip=%s instance=%s", eipIP, instID),
			harness.VPCDiagnosticsOpts{
				ExternalIP:  eipIP,
				LogicalIP:   privIP,
				ArtifactDir: fix.ArtifactDir(t),
			})
		t.Fatalf("guest unreachable via EIP %s within %s (see diagnostics above)", eipIP, sshReadyBudget)
	}
	tgt := harness.SSHTarget{User: "ec2-user", Host: eipIP, Port: 22, KeyPath: keyPath}
	idOut := runSSH(t, tgt, "id")
	require.Containsf(t, idOut, "ec2-user", "ssh via EIP id did not report ec2-user\n%s", idOut)
	harness.Detail(t, "datapath", "eip_reachable_ok")

	// --- DisassociateAddress + assert unreachable -------------------------
	harness.Step(t, "disassociate-address %s", assocID)
	_, err = c.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{AssociationId: aws.String(assocID)})
	require.NoError(t, err, "disassociate-address")
	disassociated = true

	// The EIP no longer maps to the guest, but DNAT teardown can lag and a single
	// transient SSH error must not be read as teardown — require several
	// consecutive unreachable probes before declaring success.
	harness.Step(t, "verify guest unreachable via EIP %s after disassociate", eipIP)
	const wantUnreachable = 3
	consecutive := 0
	harness.EventuallyErr(t, func() error {
		if _, sErr := runSSHQuiet(tgt, "true"); sErr == nil {
			consecutive = 0
			return fmt.Errorf("EIP %s still reaches guest after disassociate", eipIP)
		}
		if consecutive++; consecutive < wantUnreachable {
			return fmt.Errorf("EIP %s unreachable %d/%d — confirming it is not a transient blip",
				eipIP, consecutive, wantUnreachable)
		}
		return nil
	}, 90*time.Second, 5*time.Second)

	// Rule out an instance-death false pass: the unreachability must be the EIP
	// teardown, not the subject VM having gone away mid-poll.
	harness.WaitForInstanceState(t, c, instID, "running", harness.WithTimeout(30*time.Second))
	harness.Detail(t, "datapath", "eip_unreachable_after_disassociate_ok")

	// --- ReleaseAddress ----------------------------------------------------
	harness.Step(t, "release-address %s", allocID)
	_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	require.NoError(t, err, "release-address")
	eipReleased = true
}
