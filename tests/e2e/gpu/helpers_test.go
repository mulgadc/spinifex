//go:build e2e

package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// launchGPUInstance runs a single GPU instance using the fixture's instance
// type and AMI, registers a terminate-and-wait cleanup, and returns the
// instance ID. The caller receives the ID before the instance is running —
// call harness.WaitForInstanceState to block until ready.
func launchGPUInstance(t *testing.T, fix *Fixture, subnetID string, sgIDs []string, keyName string) string {
	t.Helper()
	sgs := make([]*string, len(sgIDs))
	for i, id := range sgIDs {
		sgs[i] = aws.String(id)
	}
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(fix.AMIID),
		InstanceType:     aws.String(fix.GPUInstanceType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(subnetID),
		SecurityGroupIds: sgs,
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "RunInstances")
	require.NotEmpty(t, out.Instances, "RunInstances returned no instances")
	id := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		harness.WaitForInstanceState(t, fix.AWS, id, "terminated")
	})
	return id
}

// waitForSSH polls until an SSH handshake to tgt succeeds or timeout elapses.
// GPU AMIs initialise the NVIDIA driver on first boot which can add latency.
func waitForSSH(t *testing.T, tgt harness.SSHTarget, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, "ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=5",
			"-o", "BatchMode=yes",
			"-p", strconv.Itoa(tgt.Port),
			"-i", tgt.KeyPath,
			tgt.User+"@"+tgt.Host,
			"true",
		).Run()
		cancel()
		if err == nil {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("SSH to %s:%d not ready after %s", tgt.Host, tgt.Port, timeout)
}

// sshRun executes cmd on tgt and returns trimmed stdout+stderr.
func sshRun(t *testing.T, tgt harness.SSHTarget, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User+"@"+tgt.Host,
		cmd,
	).CombinedOutput()
	require.NoError(t, err, "ssh %q: %s", cmd, out)
	return strings.TrimSpace(string(out))
}
