//go:build e2e

package single

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// vswitchdRecoveryBudget bounds how long a guest may stay dark after
// ovs-vswitchd restarts: one daemon reconcile interval, the re-install, and
// ovn-controller re-pushing br-int.
const vswitchdRecoveryBudget = 60 * time.Second

// openvswitchUnits are the systemd units that run ovs-vswitchd, by distro
// family: Debian/Ubuntu first, then RHEL/Oracle Linux.
var openvswitchUnits = []string{"openvswitch-switch.service", "openvswitch.service"}

// runIMDSDatapathSurvivesVswitchdRestart restarts ovs-vswitchd under a running
// guest and requires the guest's EIP, its IMDS and its br-imds flows to come
// back. ovs-vswitchd holds flows only in memory, and br-imds is fail-mode=secure,
// so before the daemon re-installed them every guest on the node went dark.
//
// It takes the whole node's datapath down, so it must never run in parallel.
func runIMDSDatapathSurvivesVswitchdRestart(t *testing.T, fix *Fixture) {
	if !fix.PublicPool {
		t.Skip("needs a pool of public addresses to allocate an Elastic IP from")
	}
	harness.SkipIfNoOVN(t)
	harness.Phase(t, "Single — a guest keeps its network and IMDS across an ovs-vswitchd restart")

	c := fix.AWS
	unit := openvswitchUnit(t)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, c, vpc.SGID)

	// Whatever happens below, the node must not be left without ovs-vswitchd.
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "systemctl", "start", unit).Run()
	})

	harness.Step(t, "launch guest")
	instanceID := launchBaselineInstance(t, fix, amiID, instType, keyName, vpc.SubnetID, []string{vpc.SGID})

	harness.Step(t, "allocate-address and associate it with %s", instanceID)
	eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")}) // e2e:allow-create — the Elastic IP is the ingress path that must survive the restart.
	require.NoError(t, err, "allocate-address")
	allocID, eipIP := aws.StringValue(eipOut.AllocationId), aws.StringValue(eipOut.PublicIp)
	require.NotEmpty(t, eipIP, "EIP PublicIp empty")
	t.Cleanup(func() {
		_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	})
	assocOut, err := c.EC2.AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId: aws.String(allocID),
		InstanceId:   aws.String(instanceID),
	})
	require.NoError(t, err, "associate-address")
	t.Cleanup(func() {
		_, _ = c.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{AssociationId: assocOut.AssociationId})
	})
	if !sshHealth.TryReady(eipIP, 22, keyPath, harness.SSHReadyBudget) {
		t.Fatalf("guest unreachable via EIP %s within %s", eipIP, harness.SSHReadyBudget)
	}
	tgt := harness.SSHTarget{User: "ubuntu", Host: eipIP, Port: 22, KeyPath: keyPath}

	require.NoError(t, guestIMDSAnswers(t, tgt, instanceID), "IMDS before any restart")
	baseline := imdsBridgeFlowCount(t)
	require.Positive(t, baseline, "br-imds has no flows with a guest running")
	harness.Detail(t, "eip", eipIP, "unit", unit, "br_imds_flows", baseline)

	for _, disruption := range []struct {
		name string
		argv []string
	}{
		{"systemctl restart " + unit, []string{"systemctl", "restart", unit}},
		// systemd restarts it; nothing runs the scripted flow save/restore.
		{"SIGKILL ovs-vswitchd", []string{"pkill", "-9", "-x", "ovs-vswitchd"}},
	} {
		harness.Step(t, "%s", disruption.name)
		start := time.Now()
		out, err := exec.Command("sudo", append([]string{"-n"}, disruption.argv...)...).CombinedOutput()
		require.NoErrorf(t, err, "%s: %s", disruption.name, strings.TrimSpace(string(out)))

		// One condition, so the time it reports is when everything was back.
		harness.EventuallyErr(t, func() error {
			if err := guestIMDSAnswers(t, tgt, instanceID); err != nil {
				return err
			}
			if n := imdsBridgeFlowCount(t); n != baseline {
				return fmt.Errorf("br-imds has %d flows, had %d before the restart", n, baseline)
			}
			return nil
		}, vswitchdRecoveryBudget, 2*time.Second)
		harness.Detail(t, "disruption", disruption.name, "recovered_ms", time.Since(start).Milliseconds())
	}
}

// guestIMDSAnswers reaches the guest over its EIP and has it fetch its own
// instance ID through IMDSv2, which crosses both halves of the br-imds datapath.
func guestIMDSAnswers(t *testing.T, tgt harness.SSHTarget, instanceID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const cmd = `T=$(curl -sf -m3 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 60" http://169.254.169.254/latest/api/token) && ` +
		`curl -sf -m3 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/instance-id`
	out, err := harness.RunGuestSSH(ctx, tgt, cmd)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != instanceID {
		return fmt.Errorf("IMDS instance-id = %q, want %q", got, instanceID)
	}
	return nil
}

// imdsBridgeFlowCount returns how many flows br-imds holds, or -1 while
// ovs-vswitchd is not answering.
func imdsBridgeFlowCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "ovs-ofctl", "--no-stats", "dump-flows", host.IMDSBridge).Output()
	if err != nil {
		return -1
	}
	return strings.Count(string(out), "cookie=")
}

// openvswitchUnit returns the unit that runs ovs-vswitchd on this host.
func openvswitchUnit(t *testing.T) string {
	t.Helper()
	for _, unit := range openvswitchUnits {
		if exec.Command("systemctl", "cat", unit).Run() == nil {
			return unit
		}
	}
	t.Fatalf("none of %v is installed", openvswitchUnits)
	return ""
}
