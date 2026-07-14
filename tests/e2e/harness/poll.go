//go:build e2e

package harness

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// PollOpt tunes the default timeout / interval for a Wait* helper.
// Resource defaults: instance/volume = 5min, snapshot/image = 10min, 2s interval.
type PollOpt func(*pollCfg)

type pollCfg struct {
	timeout   time.Duration
	interval  time.Duration
	skipNodes map[string]struct{}
}

// WithTimeout overrides the resource-default timeout.
func WithTimeout(d time.Duration) PollOpt { return func(c *pollCfg) { c.timeout = d } }

// WithPoll overrides the resource-default polling interval.
func WithPoll(d time.Duration) PollOpt { return func(c *pollCfg) { c.interval = d } }

// WithSkipNodes excludes named nodes from a per-node cluster poll. Used when a
// test has intentionally stopped a node (e.g. StopNode) and must not poll it.
func WithSkipNodes(names ...string) PollOpt {
	return func(c *pollCfg) {
		if c.skipNodes == nil {
			c.skipNodes = make(map[string]struct{}, len(names))
		}
		for _, n := range names {
			c.skipNodes[n] = struct{}{}
		}
	}
}

func applyOpts(def pollCfg, opts ...PollOpt) pollCfg {
	for _, o := range opts {
		o(&def)
	}
	return def
}

// reapedInstanceStates are terminal states a live target (e.g. "running")
// can never reach; observing one means the launch was reaped.
var reapedInstanceStates = map[string]struct{}{
	ec2.InstanceStateNameShuttingDown: {},
	ec2.InstanceStateNameTerminated:   {},
}

// WaitForInstanceState polls DescribeInstances until State.Name == target.
// Returns the latest instance on success; t.Fatal on timeout.
func WaitForInstanceState(t *testing.T, c *AWSClient, id, target string, opts ...PollOpt) *ec2.Instance {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 5 * time.Minute, interval: 2 * time.Second}, opts...)
	var last *ec2.Instance
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe %s: %w", id, err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return fmt.Errorf("%s not found", id)
		}
		last = out.Reservations[0].Instances[0]
		lastState = aws.StringValue(last.State.Name)
		// A reaped instance can never reach a live target: fail fast with the
		// recorded reason instead of burning the full timeout.
		if _, reaped := reapedInstanceStates[lastState]; reaped {
			if _, targetReaped := reapedInstanceStates[target]; !targetReaped {
				reason := "no state reason recorded"
				if last.StateReason != nil {
					reason = aws.StringValue(last.StateReason.Message)
				}
				t.Fatalf("instance %s reaped to %s (want %s): %s", id, lastState, target, reason)
			}
		}
		if lastState == target {
			return nil
		}
		return fmt.Errorf("%s state=%s want=%s", id, lastState, target)
	}, cfg.timeout, cfg.interval)
	t.Logf("instance %s reached state %s", id, target)
	return last
}

// WaitForVolumeState polls DescribeVolumes until State == target. Common
// targets: "available", "in-use", "deleted".
func WaitForVolumeState(t *testing.T, c *AWSClient, id, target string, opts ...PollOpt) *ec2.Volume {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 5 * time.Minute, interval: 2 * time.Second}, opts...)
	var last *ec2.Volume
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe-volume %s: %w", id, err)
		}
		if len(out.Volumes) == 0 {
			return fmt.Errorf("%s not found", id)
		}
		last = out.Volumes[0]
		lastState = aws.StringValue(last.State)
		if lastState == target {
			return nil
		}
		return fmt.Errorf("%s state=%s want=%s", id, lastState, target)
	}, cfg.timeout, cfg.interval)
	t.Logf("volume %s reached state %s", id, target)
	return last
}

// WaitForSnapshotState polls DescribeSnapshots until State == target. Snapshot
// creation can be slow on busy nodes — default timeout 10min.
func WaitForSnapshotState(t *testing.T, c *AWSClient, id, target string, opts ...PollOpt) *ec2.Snapshot {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 2 * time.Second}, opts...)
	var last *ec2.Snapshot
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
			SnapshotIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe-snapshot %s: %w", id, err)
		}
		if len(out.Snapshots) == 0 {
			return fmt.Errorf("%s not found", id)
		}
		last = out.Snapshots[0]
		lastState = aws.StringValue(last.State)
		if lastState == target {
			return nil
		}
		if lastState == "error" {
			return fmt.Errorf("%s entered error state: %s", id, aws.StringValue(last.StateMessage))
		}
		return fmt.Errorf("%s state=%s want=%s", id, lastState, target)
	}, cfg.timeout, cfg.interval)
	t.Logf("snapshot %s reached state %s", id, target)
	return last
}

// WaitForImageState polls DescribeImages until State == target. AMI creation
// from a snapshot is the slowest path; default timeout 10min.
func WaitForImageState(t *testing.T, c *AWSClient, id, target string, opts ...PollOpt) *ec2.Image {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 2 * time.Second}, opts...)
	var last *ec2.Image
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{
			ImageIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe-image %s: %w", id, err)
		}
		if len(out.Images) == 0 {
			return fmt.Errorf("%s not found", id)
		}
		last = out.Images[0]
		lastState = aws.StringValue(last.State)
		if lastState == target {
			return nil
		}
		if lastState == "failed" {
			return fmt.Errorf("%s entered failed state: %s", id, aws.StringValue(last.StateReason.Message))
		}
		return fmt.Errorf("%s state=%s want=%s", id, lastState, target)
	}, cfg.timeout, cfg.interval)
	t.Logf("image %s reached state %s", id, target)
	return last
}
