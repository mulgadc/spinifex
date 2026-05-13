package gateway_ec2_instance

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runningStatus(id, az string) *ec2.InstanceStatus {
	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(az),
		InstanceId:       aws.String(id),
		InstanceState:    &ec2.InstanceState{Code: aws.Int64(16), Name: aws.String("running")},
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status: aws.String("ok"),
			Details: []*ec2.InstanceStatusDetails{{
				Name: aws.String("reachability"), Status: aws.String("passed"),
			}},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String("ok"),
			Details: []*ec2.InstanceStatusDetails{{
				Name: aws.String("reachability"), Status: aws.String("passed"),
			}},
		},
	}
}

func respondJSON(t *testing.T, msg *nats.Msg, payload any) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(data))
}

func TestDescribeInstanceStatus_SingleNode(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{
				runningStatus("i-001", "us-east-1a"),
				runningStatus("i-002", "us-east-1a"),
			},
		})
	})
	require.NoError(t, err)

	out, err := DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, nc, 1, "123456789012", "us-east-1a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 2)
}

func TestDescribeInstanceStatus_TwoNodesDedup(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// First node returns i-001 (running)
	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-001", "us-east-1a")},
		})
	})
	require.NoError(t, err)

	// Second node also reports i-001 — should dedup
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	_, err = nc2.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-001", "us-east-1a")},
		})
	})
	require.NoError(t, err)

	require.NoError(t, nc.Flush())
	require.NoError(t, nc2.Flush())

	out, err := DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, nc, 2, "123456789012", "us-east-1a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-001", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_OneNodeErrorOthersData(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Node 1: data
	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-good", "az-a")},
		})
	})
	require.NoError(t, err)

	// Node 2: error
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		require.NoError(t, msg.Respond(utils.GenerateErrorPayload("InvalidParameterValue")))
	})
	require.NoError(t, err)

	require.NoError(t, nc.Flush())
	require.NoError(t, nc2.Flush())

	out, err := DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, nc, 2, "123456789012", "az-a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-good", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_AllNodesError(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		require.NoError(t, msg.Respond(utils.GenerateErrorPayload("InvalidParameterValue")))
	})
	require.NoError(t, err)

	_, err = DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, nc, 1, "123456789012", "az-a")
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

func TestDescribeInstanceStatus_AllNodesTimeoutReturnsEmpty(t *testing.T) {
	_, nc := startTestNATSServer(t)

	out, err := DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, nc, 0, "123456789012", "az-a")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_IncludeAllAddsStoppedAsNotApplicable(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Fan-out responder: a running instance
	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-running", "az-a")},
		})
	})
	require.NoError(t, err)

	// Stopped KV responder
	_, err = nc.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{
				ReservationId: aws.String("r-stop"),
				Instances: []*ec2.Instance{{
					InstanceId: aws.String("i-stopped"),
					State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
				}},
			}},
		})
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstanceStatusInput{IncludeAllInstances: aws.Bool(true)}
	out, err := DescribeInstanceStatus(input, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 2)

	statusByID := make(map[string]*ec2.InstanceStatus)
	for _, s := range out.InstanceStatuses {
		statusByID[*s.InstanceId] = s
	}

	running := statusByID["i-running"]
	require.NotNil(t, running)
	assert.Equal(t, "ok", *running.InstanceStatus.Status)
	assert.Equal(t, "passed", *running.InstanceStatus.Details[0].Status)

	stopped := statusByID["i-stopped"]
	require.NotNil(t, stopped)
	assert.Equal(t, "stopped", *stopped.InstanceState.Name)
	assert.Equal(t, "not-applicable", *stopped.InstanceStatus.Status)
	assert.Equal(t, "not-applicable", *stopped.SystemStatus.Status)
	assert.Equal(t, "not-applicable", *stopped.InstanceStatus.Details[0].Status)
	assert.Equal(t, "az-a", *stopped.AvailabilityZone)
}

func TestDescribeInstanceStatus_RunningWinsOverStoppedDuringRace(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Fan-out: i-001 is running
	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{
			InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-001", "az-a")},
		})
	})
	require.NoError(t, err)

	// Stopped KV also has i-001 (race: stop transition hasn't fully cleared the KV)
	_, err = nc.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{
				ReservationId: aws.String("r-1"),
				Instances: []*ec2.Instance{{
					InstanceId: aws.String("i-001"),
					State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
				}},
			}},
		})
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstanceStatusInput{IncludeAllInstances: aws.Bool(true)}
	out, err := DescribeInstanceStatus(input, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "running", *out.InstanceStatuses[0].InstanceState.Name)
	assert.Equal(t, "ok", *out.InstanceStatuses[0].InstanceStatus.Status)
}

func TestDescribeInstanceStatus_NilInput(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{})
	})
	require.NoError(t, err)

	out, err := DescribeInstanceStatus(nil, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_ClosedConnection(t *testing.T) {
	_, nc := startTestNATSServer(t)

	closed, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	closed.Close()

	_, err = DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{}, closed, 1, "123456789012", "az-a")
	require.Error(t, err)
}

func TestBuildInstanceStatusFromInstance_PopulatesState(t *testing.T) {
	in := &ec2.Instance{
		InstanceId: aws.String("i-x"),
		State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
	}
	got := buildInstanceStatusFromInstance(in, "az-a")
	require.NotNil(t, got)
	assert.Equal(t, "i-x", *got.InstanceId)
	assert.Equal(t, int64(80), *got.InstanceState.Code)
	assert.Equal(t, "stopped", *got.InstanceState.Name)
	assert.Equal(t, "az-a", *got.AvailabilityZone)
	assert.Equal(t, "not-applicable", *got.InstanceStatus.Status)
	assert.Equal(t, "not-applicable", *got.SystemStatus.Status)
}

func TestBuildInstanceStatusFromInstance_FallbackStateName(t *testing.T) {
	in := &ec2.Instance{InstanceId: aws.String("i-x")}
	got := buildInstanceStatusFromInstance(in, "az-a")
	assert.Equal(t, "stopped", *got.InstanceState.Name)
}

// Regression: the stopped-instance handler unmarshals with DisallowUnknownFields,
// so forwarding the raw DescribeInstanceStatusInput JSON failed with ValidationError
// because of the IncludeAllInstances field. The gateway must project to
// DescribeInstancesInput before publishing.
func TestDescribeInstanceStatus_IncludeAllProjectsInputForStoppedHandler(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{})
	})
	require.NoError(t, err)

	var capturedReq ec2.DescribeInstancesInput
	var unmarshalErr error
	_, err = nc.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", func(msg *nats.Msg) {
		dec := json.NewDecoder(bytes.NewReader(msg.Data))
		dec.DisallowUnknownFields()
		unmarshalErr = dec.Decode(&capturedReq)
		respondJSON(t, msg, &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{
				Instances: []*ec2.Instance{{
					InstanceId: aws.String("i-stop"),
					State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
				}},
			}},
		})
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
		InstanceIds:         []*string{aws.String("i-stop")},
	}
	out, err := DescribeInstanceStatus(input, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	require.NoError(t, unmarshalErr, "projected input must decode cleanly under DisallowUnknownFields")
	require.Len(t, out.InstanceStatuses, 1)
	require.NotNil(t, capturedReq.InstanceIds)
	require.Len(t, capturedReq.InstanceIds, 1)
	assert.Equal(t, "i-stop", *capturedReq.InstanceIds[0])
}

func TestStoppedCompatibleFilters_StripsStatusOnlyNames(t *testing.T) {
	filters := []*ec2.Filter{
		{Name: aws.String("availability-zone"), Values: []*string{aws.String("az-a")}},
		{Name: aws.String("instance-state-code"), Values: []*string{aws.String("80")}},
		{Name: aws.String("instance-state-name"), Values: []*string{aws.String("stopped")}},
		{Name: aws.String("tag:Name"), Values: []*string{aws.String("foo")}},
	}
	got := stoppedCompatibleFilters(filters)
	require.Len(t, got, 2)
	names := []string{*got[0].Name, *got[1].Name}
	assert.ElementsMatch(t, []string{"instance-state-name", "tag:Name"}, names)
}

func TestStoppedCompatibleFilters_Empty(t *testing.T) {
	assert.Nil(t, stoppedCompatibleFilters(nil))
	assert.Nil(t, stoppedCompatibleFilters([]*ec2.Filter{}))
}

func TestFilterStoppedStatuses_AvailabilityZoneMatch(t *testing.T) {
	in := []*ec2.InstanceStatus{
		{InstanceId: aws.String("i-1"), AvailabilityZone: aws.String("az-a"),
			InstanceState: &ec2.InstanceState{Code: aws.Int64(80)}},
	}
	filters := []*ec2.Filter{
		{Name: aws.String("availability-zone"), Values: []*string{aws.String("az-a")}},
	}
	got := filterStoppedStatuses(in, filters, "az-a")
	require.Len(t, got, 1)
}

func TestFilterStoppedStatuses_AvailabilityZoneMiss(t *testing.T) {
	in := []*ec2.InstanceStatus{
		{InstanceId: aws.String("i-1"), AvailabilityZone: aws.String("az-a"),
			InstanceState: &ec2.InstanceState{Code: aws.Int64(80)}},
	}
	filters := []*ec2.Filter{
		{Name: aws.String("availability-zone"), Values: []*string{aws.String("az-b")}},
	}
	got := filterStoppedStatuses(in, filters, "az-a")
	assert.Empty(t, got)
}

func TestFilterStoppedStatuses_StateCodeMatch(t *testing.T) {
	in := []*ec2.InstanceStatus{
		{InstanceId: aws.String("i-1"), AvailabilityZone: aws.String("az-a"),
			InstanceState: &ec2.InstanceState{Code: aws.Int64(80)}},
	}
	filters := []*ec2.Filter{
		{Name: aws.String("instance-state-code"), Values: []*string{aws.String("80")}},
	}
	got := filterStoppedStatuses(in, filters, "az-a")
	require.Len(t, got, 1)
}

func TestFilterStoppedStatuses_StateCodeMiss(t *testing.T) {
	in := []*ec2.InstanceStatus{
		{InstanceId: aws.String("i-1"), AvailabilityZone: aws.String("az-a"),
			InstanceState: &ec2.InstanceState{Code: aws.Int64(80)}},
	}
	filters := []*ec2.Filter{
		{Name: aws.String("instance-state-code"), Values: []*string{aws.String("16")}},
	}
	got := filterStoppedStatuses(in, filters, "az-a")
	assert.Empty(t, got)
}

func TestFilterStoppedStatuses_NoFiltersPassthrough(t *testing.T) {
	in := []*ec2.InstanceStatus{{InstanceId: aws.String("i-1")}}
	got := filterStoppedStatuses(in, nil, "az-a")
	assert.Equal(t, in, got)
}

func TestFilterStoppedStatuses_UnknownFilterIgnored(t *testing.T) {
	in := []*ec2.InstanceStatus{
		{InstanceId: aws.String("i-1"), AvailabilityZone: aws.String("az-a"),
			InstanceState: &ec2.InstanceState{Code: aws.Int64(80)}},
	}
	// Filter that the stopped-bucket already applies (instance-state-name)
	// should not also be enforced gateway-side — pass-through.
	filters := []*ec2.Filter{
		{Name: aws.String("instance-state-name"), Values: []*string{aws.String("running")}},
	}
	got := filterStoppedStatuses(in, filters, "az-a")
	require.Len(t, got, 1)
}

func TestDescribeInstanceStatus_IncludeAllWithAZFilterMatches(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{})
	})
	require.NoError(t, err)

	_, err = nc.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{{
				InstanceId: aws.String("i-stop"),
				State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
			}}}},
		})
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("az-a")}},
		},
	}
	out, err := DescribeInstanceStatus(input, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-stop", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_IncludeAllWithAZFilterMisses(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{})
	})
	require.NoError(t, err)

	_, err = nc.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{{
				InstanceId: aws.String("i-stop"),
				State:      &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
			}}}},
		})
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("az-other")}},
		},
	}
	out, err := DescribeInstanceStatus(input, nc, 1, "123456789012", "az-a")
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDedupStatuses_FirstWins(t *testing.T) {
	first := runningStatus("i-001", "az-a")
	second := &ec2.InstanceStatus{
		InstanceId:    aws.String("i-001"),
		InstanceState: &ec2.InstanceState{Code: aws.Int64(80), Name: aws.String("stopped")},
	}
	got := dedupStatuses([]*ec2.InstanceStatus{first, second})
	require.Len(t, got, 1)
	assert.Equal(t, "running", *got[0].InstanceState.Name)
}
