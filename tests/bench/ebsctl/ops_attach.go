//go:build e2e && bench

package ebsctl

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// attachDeviceLetters gives each worker its own guest device letter so
// concurrent attaches to the shared boot instance don't collide. Indexing a
// string (not a byte(int) conversion) sidesteps any overflow-conversion
// concern for the workerID -> device-letter mapping.
const attachDeviceLetters = "fghijklmnopqrstuvwxyz"

// maxAttachDetachConcurrency bounds concurrency for the attach/detach phase:
// one distinct guest device letter is available per worker.
const maxAttachDetachConcurrency = len(attachDeviceLetters)

// runAttachDetachPhase boots one nano instance, pre-creates
// concurrency*(warmup+iterations) volumes (so CreateVolume latency doesn't
// pollute the attach/detach measurement), then times AttachVolume/DetachVolume
// against them. This is the newest server code (PublishVolume/UnpublishVolume)
// and the part of the provider boundary most likely to regress, so it is
// included by default — see -attach-detach to skip it.
func runAttachDetachPhase(t *testing.T, fix *harness.Fixture, c *harness.AWSClient, tags []*ec2.Tag, az string, sizeGiB int64, warmup, iterations, concurrency int) (attachRes, detachRes *OpResult, err error) {
	t.Helper()
	if concurrency > maxAttachDetachConcurrency {
		return nil, nil, fmt.Errorf("attach/detach phase: concurrency=%d exceeds the %d distinct guest "+
			"device letters (/dev/sdf../dev/sdz) this phase assigns one-per-worker", concurrency, maxAttachDetachConcurrency)
	}

	instanceType, arch := harness.DiscoverNanoInstanceType(t, fix)
	ami := harness.DiscoverUbuntuAMI(t, fix, arch)
	vpc := harness.EnsureDefaultVPC(t, fix)
	keyName, _ := harness.EnsureKeyPair(t, fix, t.TempDir())

	instanceID := harness.EnsureInstance(t, fix, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instanceType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
	})
	// EnsureInstance already registers terminate-on-cleanup.
	harness.WaitForInstanceState(t, c, instanceID, ec2.InstanceStateNameRunning)

	total := concurrency * (warmup + iterations)
	volIDs := make([]string, 0, total)
	for i := range total {
		out, cerr := c.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(sizeGiB),
		})
		if cerr != nil {
			return nil, nil, fmt.Errorf("attach/detach phase: pre-create volume %d/%d: %w", i+1, total, cerr)
		}
		id := aws.StringValue(out.VolumeId)
		harness.RegisterVolumeTeardown(t, c, id)
		tagResource(c, id, tags)
		if _, timedOut := pollVolumeState(c, id, ec2.VolumeStateAvailable, volumeSettleTimeout); timedOut {
			return nil, nil, fmt.Errorf("attach/detach phase: volume %s never became available", id)
		}
		volIDs = append(volIDs, id)
	}

	colAttach := newCollector()
	colDetach := newCollector()
	var next atomic.Int64

	parallelFor(concurrency, warmup, iterations, func(workerID, _ int, warm bool) {
		i := int(next.Add(1) - 1)
		if i >= len(volIDs) {
			return // pre-create failed to reach `total`; nothing left to attach.
		}
		volID := volIDs[i]
		device := "/dev/sd" + string(attachDeviceLetters[workerID])

		start := time.Now()
		_, err := c.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(volID),
			InstanceId: aws.String(instanceID),
			Device:     aws.String(device),
		})
		api := time.Since(start)
		if err != nil {
			if !warm {
				colAttach.record(api, nil, false, err)
			}
			return
		}
		settle, timedOut := pollVolumeState(c, volID, ec2.VolumeStateInUse, attachSettleTimeout)
		if !warm {
			var s *time.Duration
			if !timedOut {
				s = &settle
			}
			colAttach.record(api, s, timedOut, nil)
		}
		// A volume stuck attached (settle timeout) can't usefully be
		// detach-timed; the DeleteVolume teardown still force-detaches it.
		if timedOut {
			return
		}

		dstart := time.Now()
		_, derr := c.EC2.DetachVolume(&ec2.DetachVolumeInput{VolumeId: aws.String(volID)})
		dapi := time.Since(dstart)
		if derr != nil {
			if !warm {
				colDetach.record(dapi, nil, false, derr)
			}
			return
		}
		dsettle, dtimedOut := pollVolumeState(c, volID, ec2.VolumeStateAvailable, attachSettleTimeout)
		if !warm {
			var ds *time.Duration
			if !dtimedOut {
				ds = &dsettle
			}
			colDetach.record(dapi, ds, dtimedOut, nil)
		}
	})

	return colAttach.result("AttachVolume"), colDetach.result("DetachVolume"), nil
}
