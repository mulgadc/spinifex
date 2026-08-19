//go:build e2e

package single

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const earlyRebootRecoveryTimeout = 3 * time.Minute

// runEarlyRebootLiveness reboots a fresh guest as soon as cloud-init.target is
// observable. The control plane must either reject a stuck reset or boot again.
func runEarlyRebootLiveness(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — early reboot resumes or reports an API error")

	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	harness.Step(t, "launch fresh guest for early reboot")
	instanceID := launchBaselineInstance(t, fix, amiID, instType, keyName, vpc.SubnetID, []string{vpc.SGID})
	inst := describeSingletonInstance(t, fix, instanceID)
	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	bootID := strings.TrimSpace(runSSH(t, tgt, "cat /proc/sys/kernel/random/boot_id"))
	require.NotEmpty(t, bootID, "fresh guest returned an empty boot ID")
	waitForCloudInitTarget(t, tgt)

	harness.Step(t, "reboot-instances %s immediately after cloud-init.target", instanceID)
	_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	if err != nil {
		var apiErr awserr.Error
		require.ErrorAs(t, err, &apiErr, "reboot failure must be an AWS API error")
		assert.Equal(t, "InternalError", apiErr.Code(), "a liveness failure must surface as InternalError")
		harness.Detail(t, "reboot_outcome", "api_error", "code", apiErr.Code())
		return
	}

	deadline := time.Now().Add(earlyRebootRecoveryTimeout)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		out, sshErr := harness.RunGuestSSH(ctx, tgt, "cat /proc/sys/kernel/random/boot_id")
		cancel()
		if sshErr == nil {
			newBootID := strings.TrimSpace(string(out))
			if newBootID != "" && newBootID != bootID {
				harness.Detail(t, "reboot_outcome", "resumed", "boot_id", newBootID)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("reboot API succeeded but guest boot ID did not change within %s", earlyRebootRecoveryTimeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func waitForCloudInitTarget(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		out, err := harness.RunGuestSSH(ctx, tgt, "systemctl is-active cloud-init.target")
		cancel()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cloud-init.target did not become active within 1m (last output %q, err: %v)", strings.TrimSpace(string(out)), err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
