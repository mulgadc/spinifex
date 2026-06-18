package gateway_ec2_capacityreservation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

// validCreateInput is a request that passes static validation, so census-driven
// paths can be exercised by varying only the cluster's responses.
func validCreateInput() *ec2.CreateCapacityReservationInput {
	return &ec2.CreateCapacityReservationInput{
		InstanceType:     aws.String("t3.micro"),
		InstanceCount:    aws.Int64(2),
		AvailabilityZone: aws.String("az-1"),
		InstancePlatform: aws.String("Linux/UNIX"),
	}
}

// answerNodeStatus replies to spinifex.node.status with the given snapshots,
// letting a test stand up a synthetic census without a real daemon.
func answerNodeStatus(t *testing.T, nc *nats.Conn, nodes []types.NodeStatusResponse) {
	t.Helper()
	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, n := range nodes {
			data, _ := json.Marshal(n)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// Create — static validation

func TestValidateCreateCapacityReservationInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ec2.CreateCapacityReservationInput)
		wantErr string
	}{
		{"valid", func(*ec2.CreateCapacityReservationInput) {}, ""},
		{"nil instance type", func(in *ec2.CreateCapacityReservationInput) { in.InstanceType = nil }, awserrors.ErrorInvalidParameterValue},
		{"empty instance type", func(in *ec2.CreateCapacityReservationInput) { in.InstanceType = aws.String("") }, awserrors.ErrorInvalidParameterValue},
		{"zero count", func(in *ec2.CreateCapacityReservationInput) { in.InstanceCount = aws.Int64(0) }, awserrors.ErrorInvalidParameterValue},
		{"negative count", func(in *ec2.CreateCapacityReservationInput) { in.InstanceCount = aws.Int64(-1) }, awserrors.ErrorInvalidParameterValue},
		{"missing az", func(in *ec2.CreateCapacityReservationInput) { in.AvailabilityZone = nil }, awserrors.ErrorInvalidParameterValue},
		{"az id set", func(in *ec2.CreateCapacityReservationInput) { in.AvailabilityZoneId = aws.String("azid-1") }, awserrors.ErrorInvalidParameterValue},
		{"end date set", func(in *ec2.CreateCapacityReservationInput) { in.EndDate = aws.Time(time.Now()) }, awserrors.ErrorInvalidParameterValue},
		{"limited end date type", func(in *ec2.CreateCapacityReservationInput) { in.EndDateType = aws.String(ec2.EndDateTypeLimited) }, awserrors.ErrorInvalidParameterValue},
		{"unlimited end date type ok", func(in *ec2.CreateCapacityReservationInput) { in.EndDateType = aws.String(ec2.EndDateTypeUnlimited) }, ""},
		{"non-default tenancy", func(in *ec2.CreateCapacityReservationInput) { in.Tenancy = aws.String("dedicated") }, awserrors.ErrorInvalidParameterValue},
		{"default tenancy ok", func(in *ec2.CreateCapacityReservationInput) {
			in.Tenancy = aws.String(ec2.CapacityReservationTenancyDefault)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCreateInput()
			tt.mutate(in)
			err := ValidateCreateCapacityReservationInput(in)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestValidateCreateCapacityReservationInput_Nil(t *testing.T) {
	assert.EqualError(t, ValidateCreateCapacityReservationInput(nil), awserrors.ErrorInvalidParameterValue)
}

// DryRun short-circuits after validation, before any cluster round-trip.
func TestCreateCapacityReservation_DryRun(t *testing.T) {
	in := validCreateInput()
	in.DryRun = aws.Bool(true)
	_, err := CreateCapacityReservation(in, nil, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorDryRunOperation)
}

func TestCreateCapacityReservation_NilNATS(t *testing.T) {
	_, err := CreateCapacityReservation(validCreateInput(), nil, 1, testAccountID)
	assert.ErrorIs(t, err, utils.ErrClusterUnavailable)
}

// Create — census-driven paths

func TestCreateCapacityReservation_UnknownAZ(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, []types.NodeStatusResponse{
		{Node: "node-a", AZ: "az-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
	})
	_, err := CreateCapacityReservation(validCreateInput(), nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidAvailabilityZone)
}

func TestCreateCapacityReservation_UnknownType(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, []types.NodeStatusResponse{
		{Node: "node-a", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.small", Available: 4}}},
	})
	_, err := CreateCapacityReservation(validCreateInput(), nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidInstanceType)
}

func TestCreateCapacityReservation_InsufficientCapacity(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, []types.NodeStatusResponse{
		{Node: "node-a", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 1}}},
	})
	_, err := CreateCapacityReservation(validCreateInput(), nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInsufficientInstanceCapacity)
}

func TestCreateCapacityReservation_Success(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, []types.NodeStatusResponse{
		{Node: "node-a", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
	})

	created := &ec2.CapacityReservation{
		CapacityReservationId: aws.String("cr-0123456789abcdef0"),
		OwnerId:               aws.String(testAccountID),
		InstanceType:          aws.String("t3.micro"),
		AvailabilityZone:      aws.String("az-1"),
		TotalInstanceCount:    aws.Int64(2),
		State:                 aws.String(ec2.CapacityReservationStateActive),
	}
	sub, err := nc.Subscribe("ec2.CreateCapacityReservation.node-a", func(msg *nats.Msg) {
		data, _ := json.Marshal(created)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := CreateCapacityReservation(validCreateInput(), nc, 1, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.CapacityReservation)
	assert.Equal(t, "cr-0123456789abcdef0", aws.StringValue(out.CapacityReservation.CapacityReservationId))
}

// Create propagates the pinned daemon's error verbatim (e.g. a lost fit re-check).
func TestCreateCapacityReservation_DaemonRejection(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, []types.NodeStatusResponse{
		{Node: "node-a", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
	})
	sub, err := nc.Subscribe("ec2.CreateCapacityReservation.node-a", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInsufficientInstanceCapacity))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = CreateCapacityReservation(validCreateInput(), nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInsufficientInstanceCapacity)
}

// Describe — filtering

func reservation(id, instanceType, az string) *ec2.CapacityReservation {
	return &ec2.CapacityReservation{
		CapacityReservationId: aws.String(id),
		InstanceType:          aws.String(instanceType),
		AvailabilityZone:      aws.String(az),
		State:                 aws.String(ec2.CapacityReservationStateActive),
		Tenancy:               aws.String(ec2.CapacityReservationTenancyDefault),
		OwnerId:               aws.String(testAccountID),
	}
}

func TestFilterReservations_ByID(t *testing.T) {
	all := []*ec2.CapacityReservation{
		reservation("cr-1", "t3.micro", "az-1"),
		reservation("cr-2", "t3.small", "az-1"),
	}
	got := filterReservations(all, []string{"cr-2"}, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "cr-2", aws.StringValue(got[0].CapacityReservationId))
}

func TestFilterReservations_ByFilter(t *testing.T) {
	all := []*ec2.CapacityReservation{
		reservation("cr-1", "t3.micro", "az-1"),
		reservation("cr-2", "t3.small", "az-2"),
	}
	got := filterReservations(all, nil, map[string][]string{"availability-zone": {"az-2"}})
	require.Len(t, got, 1)
	assert.Equal(t, "cr-2", aws.StringValue(got[0].CapacityReservationId))
}

func TestFilterReservations_IDAndFilterAreANDed(t *testing.T) {
	all := []*ec2.CapacityReservation{
		reservation("cr-1", "t3.micro", "az-1"),
		reservation("cr-2", "t3.small", "az-2"),
	}
	got := filterReservations(all, []string{"cr-1"}, map[string][]string{"availability-zone": {"az-2"}})
	assert.Empty(t, got)
}

func TestFilterReservations_TagFilterNeverMatches(t *testing.T) {
	all := []*ec2.CapacityReservation{reservation("cr-1", "t3.micro", "az-1")}
	got := filterReservations(all, nil, map[string][]string{"tag:env": {"prod"}})
	assert.Empty(t, got)
}

func TestFilterReservations_Empty(t *testing.T) {
	assert.Empty(t, filterReservations(nil, nil, nil))
}

func TestDescribeCapacityReservations_InvalidFilter(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	input := &ec2.DescribeCapacityReservationsInput{
		Filters: []*ec2.Filter{{Name: aws.String("bogus"), Values: aws.StringSlice([]string{"x"})}},
	}
	_, err := DescribeCapacityReservations(input, nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeCapacityReservations_NilNATS(t *testing.T) {
	_, err := DescribeCapacityReservations(&ec2.DescribeCapacityReservationsInput{}, nil, 1, testAccountID)
	assert.ErrorIs(t, err, utils.ErrClusterUnavailable)
}

// Describe merges reservations across nodes and applies filters to the union.
func TestDescribeCapacityReservations_MergesAndFilters(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	node1 := ec2.DescribeCapacityReservationsOutput{CapacityReservations: []*ec2.CapacityReservation{reservation("cr-1", "t3.micro", "az-1")}}
	node2 := ec2.DescribeCapacityReservationsOutput{CapacityReservations: []*ec2.CapacityReservation{reservation("cr-2", "t3.small", "az-2")}}
	sub, err := nc.Subscribe("ec2.DescribeCapacityReservations", func(msg *nats.Msg) {
		for _, o := range []ec2.DescribeCapacityReservationsOutput{node1, node2} {
			data, _ := json.Marshal(o)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := DescribeCapacityReservations(&ec2.DescribeCapacityReservationsInput{}, nc, 2, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.CapacityReservations, 2)

	filtered, err := DescribeCapacityReservations(&ec2.DescribeCapacityReservationsInput{
		Filters: []*ec2.Filter{{Name: aws.String("instance-type"), Values: aws.StringSlice([]string{"t3.small"})}},
	}, nc, 2, testAccountID)
	require.NoError(t, err)
	require.Len(t, filtered.CapacityReservations, 1)
	assert.Equal(t, "cr-2", aws.StringValue(filtered.CapacityReservations[0].CapacityReservationId))
}

// Cancel

func TestCancelCapacityReservation_NilInput(t *testing.T) {
	_, err := CancelCapacityReservation(nil, nil, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCancelCapacityReservation_MissingID(t *testing.T) {
	_, err := CancelCapacityReservation(&ec2.CancelCapacityReservationInput{}, nil, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCancelCapacityReservation_Malformed(t *testing.T) {
	_, err := CancelCapacityReservation(&ec2.CancelCapacityReservationInput{
		CapacityReservationId: aws.String("bogus-id"),
	}, nil, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidCapacityReservationIdMalformed)
}

func TestCancelCapacityReservation_DryRun(t *testing.T) {
	_, err := CancelCapacityReservation(&ec2.CancelCapacityReservationInput{
		CapacityReservationId: aws.String("cr-0123456789abcdef0"),
		DryRun:                aws.Bool(true),
	}, nil, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorDryRunOperation)
}

func TestCancelCapacityReservation_NotFound(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe("ec2.CancelCapacityReservation", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.CancelCapacityReservationOutput{Return: aws.Bool(false)})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = CancelCapacityReservation(&ec2.CancelCapacityReservationInput{
		CapacityReservationId: aws.String("cr-0123456789abcdef0"),
	}, nc, 1, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidCapacityReservationIdNotFound)
}

func TestCancelCapacityReservation_Found(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe("ec2.CancelCapacityReservation", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.CancelCapacityReservationOutput{Return: aws.Bool(true)})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := CancelCapacityReservation(&ec2.CancelCapacityReservationInput{
		CapacityReservationId: aws.String("cr-0123456789abcdef0"),
	}, nc, 1, testAccountID)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(out.Return))
}
