package daemon

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerStillPresent(t *testing.T) {
	t.Parallel()
	inst := func(state string) *ec2.DescribeInstancesOutput {
		return &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{
			{State: &ec2.InstanceState{Name: aws.String(state)}},
		}}}}
	}
	assert.False(t, workerStillPresent(&ec2.DescribeInstancesOutput{}))
	assert.False(t, workerStillPresent(inst(ec2.InstanceStateNameTerminated)))
	assert.True(t, workerStillPresent(inst(ec2.InstanceStateNameShuttingDown)))
	assert.True(t, workerStillPresent(inst(ec2.InstanceStateNameRunning)))
	assert.True(t, workerStillPresent(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{{}}}}}))
}

// answerBucket serves one instance-bucket describe subject, answering only
// requests that name id so tests sharing the NATS server do not see each other.
func answerBucket(t *testing.T, nc *nats.Conn, subject, id, state string) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var in ec2.DescribeInstancesInput
		if json.Unmarshal(msg.Data, &in) != nil || len(in.InstanceIds) != 1 || aws.StringValue(in.InstanceIds[0]) != id {
			return
		}
		out := ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{
			ReservationId: aws.String("r-" + id),
			Instances:     []*ec2.Instance{{InstanceId: aws.String(id), State: &ec2.InstanceState{Name: aws.String(state)}}},
		}}}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestLiveWorkerInstances_NeedsNATS(t *testing.T) {
	t.Parallel()
	live, err := (&Daemon{}).LiveWorkerInstances(t.Context(), []string{"i-x"}, "111122223333")
	require.Error(t, err)
	assert.Nil(t, live)
}

func TestLiveWorkerInstances_BlankIDsAreSkipped(t *testing.T) {
	t.Parallel()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	live, err := (&Daemon{natsConn: nc}).LiveWorkerInstances(t.Context(), []string{"", ""}, "111122223333")

	require.NoError(t, err)
	assert.Empty(t, live)
}

func TestLiveWorkerInstances_ReportsOnlyWorkersNotYetTerminated(t *testing.T) {
	t.Parallel()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	answerBucket(t, nc, "ec2.DescribeStoppedInstances", "i-live-stopped", ec2.InstanceStateNameStopped)
	answerBucket(t, nc, "ec2.DescribeTerminatedInstances", "i-live-done", ec2.InstanceStateNameTerminated)

	live, err := (&Daemon{natsConn: nc}).LiveWorkerInstances(t.Context(),
		[]string{"i-live-stopped", "i-live-done"}, "111122223333")

	require.NoError(t, err)
	assert.Equal(t, []string{"i-live-stopped"}, live, "a stopped worker is not gone; a terminated one is")
}

func TestLiveWorkerInstances_UnprovableAbsenceIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	live, err := (&Daemon{natsConn: nc}).LiveWorkerInstances(t.Context(), []string{"i-nobody-answers"}, "111122223333")

	require.ErrorContains(t, err, "describe worker i-nobody-answers")
	assert.Nil(t, live, "no node answered, so the worker cannot be called gone")
}
