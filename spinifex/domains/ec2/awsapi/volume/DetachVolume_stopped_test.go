package volume

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type subjectLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *subjectLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, s)
}

func (l *subjectLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func respondJSON(t *testing.T, msg *nats.Msg, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(data))
}

// The record decides the path. A stopped instance has no QMP to unplug from, so
// its record is edited and the volume released, and ec2.cmd.<id> is never used.
func TestDetachVolume_StoppedInstanceEditsTheRecordThenReleasesTheVolume(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	log := &subjectLog{}

	stoppedRecord := &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{
		Instances: []*ec2.Instance{{InstanceId: aws.String("i-stopped")}},
	}}}
	handlers := map[string]func(*nats.Msg){
		"ec2.DescribeStoppedInstances": func(m *nats.Msg) { respondJSON(t, m, stoppedRecord) },
		detachFromStoppedSubject: func(m *nats.Msg) {
			log.add(detachFromStoppedSubject)
			respondJSON(t, m, &ec2.VolumeAttachment{
				VolumeId: aws.String("vol-data"), InstanceId: aws.String("i-stopped"), Device: aws.String("/dev/sdf"),
			})
		},
		forceDetachSubject: func(m *nats.Msg) {
			log.add(forceDetachSubject)
			respondJSON(t, m, &ec2.VolumeAttachment{
				VolumeId: aws.String("vol-data"), InstanceId: aws.String("i-stopped"), State: aws.String("detached"),
			})
		},
		"ec2.cmd.i-stopped": func(m *nats.Msg) {
			log.add("ec2.cmd.i-stopped")
			_ = m.Respond(nil)
		},
	}
	for subject, h := range handlers {
		sub, err := nc.Subscribe(subject, h)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, nc.Flush())

	out, err := DetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId:   aws.String("vol-data"),
		InstanceId: aws.String("i-stopped"),
	}, nc, "000000000001")
	require.NoError(t, err)

	assert.Equal(t, "detached", aws.StringValue(out.State))
	assert.Equal(t, []string{detachFromStoppedSubject, forceDetachSubject}, log.get(),
		"record first, then the volume; the host path is never used for a stopped instance")
}

// NoResponders alone must not be read as "stopped": a dead or partitioned node
// gives the same answer, and editing the record then would race a live QEMU.
func TestDetachVolume_NoRespondersAloneDoesNotMeanStopped(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	log := &subjectLog{}

	for _, subject := range []string{detachFromStoppedSubject, forceDetachSubject} {
		sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
			log.add(subject)
			_ = m.Respond(nil)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	sub, err := nc.Subscribe("ec2.DescribeStoppedInstances", func(m *nats.Msg) {
		respondJSON(t, m, &ec2.DescribeInstancesOutput{})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	_, err = DetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId:   aws.String("vol-data"),
		InstanceId: aws.String("i-silent-node"),
	}, nc, "000000000001")

	require.Error(t, err)
	assert.Empty(t, log.get(), "no record edit and no control-plane release without a stopped record")
}
