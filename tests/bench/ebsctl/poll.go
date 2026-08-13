//go:build e2e && bench

package ebsctl

import (
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Settle-poll budgets. Bounded well under harness's own fatal Wait* defaults
// (5-10min) because a stuck settle here must not stall the whole benchmark —
// it becomes a SettleTimeouts count instead, which is itself a measurement.
const (
	pollInterval          = 500 * time.Millisecond
	volumeSettleTimeout   = 2 * time.Minute
	snapshotSettleTimeout = 3 * time.Minute
	attachSettleTimeout   = 2 * time.Minute
)

// pollVolumeState polls DescribeVolumes for id until State == target or
// timeout elapses. Non-fatal by design — unlike harness.WaitForVolumeState
// (which calls t.Fatalf), a slow settle here is a data point, not a harness
// bug, and must not abort the run.
func pollVolumeState(c *harness.AWSClient, id, target string, timeout time.Duration) (elapsed time.Duration, timedOut bool) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(id)}})
		if err == nil && len(out.Volumes) > 0 && aws.StringValue(out.Volumes[0].State) == target {
			return time.Since(start), false
		}
		if time.Now().After(deadline) {
			return time.Since(start), true
		}
		time.Sleep(pollInterval)
	}
}

// pollVolumeGone polls until DescribeVolumes reports id absent
// (InvalidVolume.NotFound) or state "deleted", or timeout elapses.
func pollVolumeGone(c *harness.AWSClient, id string, timeout time.Duration) (elapsed time.Duration, timedOut bool) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(id)}})
		if err != nil && harness.ErrorCodeIs(err, "InvalidVolume.NotFound") {
			return time.Since(start), false
		}
		if err == nil && (len(out.Volumes) == 0 || aws.StringValue(out.Volumes[0].State) == "deleted") {
			return time.Since(start), false
		}
		if time.Now().After(deadline) {
			return time.Since(start), true
		}
		time.Sleep(pollInterval)
	}
}

// pollSnapshotState polls DescribeSnapshots for id until State == target
// (typically "completed") or timeout.
func pollSnapshotState(c *harness.AWSClient, id, target string, timeout time.Duration) (elapsed time.Duration, timedOut bool) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		out, err := c.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{SnapshotIds: []*string{aws.String(id)}})
		if err == nil && len(out.Snapshots) > 0 && aws.StringValue(out.Snapshots[0].State) == target {
			return time.Since(start), false
		}
		if time.Now().After(deadline) {
			return time.Since(start), true
		}
		time.Sleep(pollInterval)
	}
}

// pollSnapshotGone polls until DescribeSnapshots reports id absent
// (InvalidSnapshot.NotFound), or timeout.
func pollSnapshotGone(c *harness.AWSClient, id string, timeout time.Duration) (elapsed time.Duration, timedOut bool) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		_, err := c.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{SnapshotIds: []*string{aws.String(id)}})
		if err != nil && harness.ErrorCodeIs(err, "InvalidSnapshot.NotFound") {
			return time.Since(start), false
		}
		if time.Now().After(deadline) {
			return time.Since(start), true
		}
		time.Sleep(pollInterval)
	}
}

// tagResource best-effort tags id (volume, snapshot, or instance) so orphans
// left by a killed run can be found and swept later. Not timed as part of
// any operation's latency and not fatal on failure — the resource still
// functions untagged, it just won't show up in a tag-based sweep.
func tagResource(c *harness.AWSClient, id string, tags []*ec2.Tag) {
	if len(tags) == 0 {
		return
	}
	_, _ = c.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(id)},
		Tags:      tags,
	})
}
