//go:build e2e

package harness

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// PollOpt tunes the default timeout / interval for a Wait* helper. Defaults
// are tuned per-resource based on the bash `sleep N; check` loops in
// run-e2e.sh — instance/volume = 5min, snapshot/image = 10min, all polled
// every 2s.
type PollOpt func(*pollCfg)

type pollCfg struct {
	timeout  time.Duration
	interval time.Duration
}

// WithTimeout overrides the resource-default timeout.
func WithTimeout(d time.Duration) PollOpt { return func(c *pollCfg) { c.timeout = d } }

// WithPoll overrides the resource-default polling interval.
func WithPoll(d time.Duration) PollOpt { return func(c *pollCfg) { c.interval = d } }

func applyOpts(def pollCfg, opts ...PollOpt) pollCfg {
	for _, o := range opts {
		o(&def)
	}
	return def
}

// WaitForInstanceState polls DescribeInstances until State.Name == target.
// Returns the latest *ec2.Instance on success; t.Fatal on timeout. Separate
// from lb.go's WaitForInstanceRunning so callers can wait for stopped /
// terminated / pending without one helper per state.
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
