package volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record decides the path: a stopped instance has no QMP to plug into, so
// the attach goes to the instance service and ec2.cmd.<id> is never used.
func TestAttachVolume_StoppedInstanceUsesTheRecordPath(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	log := &subjectLog{}

	stoppedRecord := &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{
		Instances: []*ec2.Instance{{InstanceId: aws.String("i-stopped")}},
	}}}
	handlers := map[string]func(*nats.Msg){
		"ec2.DescribeStoppedInstances": func(m *nats.Msg) { respondJSON(t, m, stoppedRecord) },
		attachToStoppedSubject: func(m *nats.Msg) {
			log.add(attachToStoppedSubject)
			respondJSON(t, m, &ec2.VolumeAttachment{
				VolumeId: aws.String("vol-data"), InstanceId: aws.String("i-stopped"),
				Device: aws.String("/dev/sdf"), State: aws.String("attached"),
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

	out, err := AttachVolume(context.Background(), &ec2.AttachVolumeInput{
		VolumeId:   aws.String("vol-data"),
		InstanceId: aws.String("i-stopped"),
		Device:     aws.String("/dev/sdf"),
	}, nc, "000000000001")
	require.NoError(t, err)

	assert.Equal(t, "attached", aws.StringValue(out.State))
	assert.Equal(t, []string{attachToStoppedSubject}, log.get(),
		"only the record path runs for a stopped instance")
}
