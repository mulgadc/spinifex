//go:build e2e && bench

package ebsctl

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// registerSnapshotTeardown deletes snapID at test cleanup. Mirrors
// harness.RegisterVolumeTeardown's already-gone-is-fine contract; harness has
// no snapshot equivalent, so this is the local one.
func registerSnapshotTeardown(t *testing.T, c *harness.AWSClient, snapID string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := c.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapID)})
		if err != nil && !harness.ErrorCodeIs(err, "InvalidSnapshot.NotFound") {
			t.Errorf("cleanup: LEAKED snapshot %s: %v", snapID, err)
		}
	})
}

// runSnapshotPhase creates one source volume, then times CreateSnapshot and
// DeleteSnapshot against it warmup+iterations times per worker. The source
// volume and every created snapshot get a t.Cleanup teardown registered
// immediately on creation, so a failure partway through still reclaims them.
func runSnapshotPhase(t *testing.T, c *harness.AWSClient, tags []*ec2.Tag, az string, sizeGiB int64, warmup, iterations, concurrency int) (createRes, deleteRes *OpResult, err error) {
	t.Helper()

	out, cerr := c.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(sizeGiB),
	})
	if cerr != nil {
		return nil, nil, fmt.Errorf("snapshot phase: create source volume: %w", cerr)
	}
	volID := aws.StringValue(out.VolumeId)
	harness.RegisterVolumeTeardown(t, c, volID)
	tagResource(c, volID, tags)

	if _, timedOut := pollVolumeState(c, volID, ec2.VolumeStateAvailable, volumeSettleTimeout); timedOut {
		return nil, nil, fmt.Errorf("snapshot phase: source volume %s never became available", volID)
	}

	colCreate := newCollector()
	var mu sync.Mutex
	var snapIDs []string
	parallelFor(concurrency, warmup, iterations, func(_, _ int, warm bool) {
		start := time.Now()
		out, err := c.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{VolumeId: aws.String(volID)})
		api := time.Since(start)
		if err != nil {
			if !warm {
				colCreate.record(api, nil, false, err)
			}
			return
		}

		snapID := aws.StringValue(out.SnapshotId)
		registerSnapshotTeardown(t, c, snapID)
		tagResource(c, snapID, tags)
		mu.Lock()
		snapIDs = append(snapIDs, snapID)
		mu.Unlock()

		settle, timedOut := pollSnapshotState(c, snapID, ec2.SnapshotStateCompleted, snapshotSettleTimeout)
		if !warm {
			var s *time.Duration
			if !timedOut {
				s = &settle
			}
			colCreate.record(api, s, timedOut, nil)
		}
	})

	colDelete := drainDelete(snapIDs, concurrency, warmup*concurrency, func(id string) (time.Duration, *time.Duration, bool, error) {
		start := time.Now()
		_, err := c.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)})
		api := time.Since(start)
		if err != nil {
			return api, nil, false, err
		}
		settle, timedOut := pollSnapshotGone(c, id, snapshotSettleTimeout)
		if timedOut {
			return api, nil, true, nil
		}
		return api, &settle, false, nil
	})

	return colCreate.result("CreateSnapshot"), colDelete.result("DeleteSnapshot"), nil
}
