//go:build e2e

package multinode

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

// runClusterRestartResumesGuests is the software-update path: a coordinated
// shutdown drains every guest to stopped and seals its volumes, then the
// cluster comes back and each guest has to relaunch from that sealed state
// while its dependencies are themselves still starting.
//
// NodeFailure/NodeRecovery do not cover this. They stop the service units
// directly so guests keep running and the target's drain never fires, which
// makes recovery a QMP re-attach to a live process. Here the process is gone
// and the volume has to be re-opened, which is where a guest was lost: the
// relaunch raced a JetStream that had not finished starting, the lease claim
// timed out, and the failure was recorded as permanent.
func runClusterRestartResumesGuests(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Cluster Restart Resumes Guests")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 2,
		"a cluster restart needs a multi-node cluster, have %d", len(fix.Cluster.Nodes))

	trio := needInstanceTrio(t, fix)
	require.NotEmpty(t, trio, "the restart is only meaningful with guests to resume")

	harness.Step(t, "confirm all %d guests are running before the shutdown", len(trio))
	for _, id := range trio {
		harness.WaitForInstanceState(t, fix.AWS, id, "running")
	}

	// If an assertion below fails mid-restart the cluster must not be left
	// down for every later test in the package.
	t.Cleanup(func() {
		for _, n := range fix.Cluster.Nodes {
			harness.StartNode(t, n)
		}
	})

	harness.ClusterShutdownAndRestart(t, fix.Cluster, fix.Env)

	harness.Step(t, "every guest resumes to running without operator action")
	var failures []string
	for _, id := range trio {
		if err := waitInstanceRunningNotErrored(t, fix, id, 5*time.Minute); err != nil {
			failures = append(failures, err.Error())
		}
	}
	require.Emptyf(t, failures,
		"guests must resume after a coordinated restart; an instance left in error is a guest an operator has to recover by hand:\n%s",
		strings.Join(failures, "\n"))
}

// waitInstanceRunningNotErrored polls until id is running, failing early and
// with the recorded reason if it lands in error instead. Waiting out the full
// timeout on an instance already parked in error would hide why.
func waitInstanceRunningNotErrored(t *testing.T, fix *Fixture, id string, timeout time.Duration) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err != nil {
			// The gateway can still be settling immediately after the restart.
			last = err.Error()
			time.Sleep(5 * time.Second)
			continue
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			last = "not returned by DescribeInstances"
			time.Sleep(5 * time.Second)
			continue
		}

		inst := out.Reservations[0].Instances[0]
		state := aws.StringValue(inst.State.Name)
		last = state
		switch state {
		case "running":
			return nil
		case "error", "terminated":
			return fmt.Errorf("%s reached %q after the restart instead of running: %s",
				id, state, stateReason(inst))
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("%s did not resume within %s, last state %q", id, timeout, last)
}

// stateReason renders an instance's StateReason, which carries the recovery
// failure that explains a guest parked in error.
func stateReason(inst *ec2.Instance) string {
	if inst.StateReason == nil {
		return "no state reason recorded"
	}
	return fmt.Sprintf("%s: %s",
		aws.StringValue(inst.StateReason.Code), aws.StringValue(inst.StateReason.Message))
}
