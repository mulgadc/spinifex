//go:build e2e && bench

package ebsctl

import (
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runCreateVolume creates concurrency*(warmup+iterations) volumes of sizeGiB
// in az, timing CreateVolume's own latency and the settle time to
// "available" separately. Every created volume (including warm-up ones) gets
// a t.Cleanup teardown registered immediately via harness.RegisterVolumeTeardown,
// so a panic or Fatal partway through still reclaims whatever was created —
// and also gets its ID returned so a paired runDeleteVolume can measure
// deleting exactly this set instead of leaving cleanup to the test-end sweep.
func runCreateVolume(t *testing.T, c *harness.AWSClient, tags []*ec2.Tag, az string, sizeGiB int64, warmup, iterations, concurrency int) (*OpResult, []string) {
	t.Helper()
	col := newCollector()
	var mu sync.Mutex
	var ids []string

	parallelFor(concurrency, warmup, iterations, func(_, _ int, warm bool) {
		start := time.Now()
		out, err := c.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(sizeGiB),
		})
		api := time.Since(start)
		if err != nil {
			if !warm {
				col.record(api, nil, false, err)
			}
			return
		}

		id := aws.StringValue(out.VolumeId)
		harness.RegisterVolumeTeardown(t, c, id)
		tagResource(c, id, tags)
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()

		settle, timedOut := pollVolumeState(c, id, ec2.VolumeStateAvailable, volumeSettleTimeout)
		if !warm {
			var s *time.Duration
			if !timedOut {
				s = &settle
			}
			col.record(api, s, timedOut, nil)
		}
	})

	return col.result("CreateVolume"), ids
}

// runDeleteVolume deletes every id (already-tagged, already-teardown-registered
// by runCreateVolume — a duplicate delete here just races the t.Cleanup one
// harmlessly since both tolerate InvalidVolume.NotFound), timing DeleteVolume's
// own latency and the settle time to gone. The first warmup*concurrency
// completions are discarded as warm-up.
func runDeleteVolume(c *harness.AWSClient, ids []string, warmup, concurrency int) *OpResult {
	col := drainDelete(ids, concurrency, warmup*concurrency, func(id string) (time.Duration, *time.Duration, bool, error) {
		start := time.Now()
		_, err := c.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(id)})
		api := time.Since(start)
		if err != nil {
			return api, nil, false, err
		}
		settle, timedOut := pollVolumeGone(c, id, volumeSettleTimeout)
		if timedOut {
			return api, nil, true, nil
		}
		return api, &settle, false, nil
	})
	return col.result("DeleteVolume")
}

// runDescribeVolumes times two DescribeVolumes shapes against poolIDs: a
// single-volume-by-id lookup (round-robin over the pool) and a full
// unfiltered list. The provider path enumerates these differently, so they
// are measured — and reported — as separate operations, never combined.
func runDescribeVolumes(c *harness.AWSClient, poolIDs []string, warmup, iterations, concurrency int) (single, list *OpResult) {
	colSingle := newCollector()
	var idx int
	var idxMu sync.Mutex
	parallelFor(concurrency, warmup, iterations, func(_, _ int, warm bool) {
		idxMu.Lock()
		id := poolIDs[idx%len(poolIDs)]
		idx++
		idxMu.Unlock()

		start := time.Now()
		_, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(id)}})
		api := time.Since(start)
		if !warm {
			colSingle.record(api, nil, false, err)
		}
	})

	colList := newCollector()
	parallelFor(concurrency, warmup, iterations, func(_, _ int, warm bool) {
		start := time.Now()
		_, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{})
		api := time.Since(start)
		if !warm {
			colList.record(api, nil, false, err)
		}
	})

	return colSingle.result("DescribeVolumes.Single"), colList.result("DescribeVolumes.List")
}
