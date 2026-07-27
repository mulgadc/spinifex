package handlers_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullVM returns a VM whose every vm.VM-sourced projection field is populated,
// so a projection either carries each through or deliberately drops it.
func fullVM(status vm.InstanceState) *vm.VM {
	return &vm.VM{
		ID:     "i-abc123",
		Status: status,
		Instance: &ec2.Instance{
			InstanceId:       aws.String("i-abc123"),
			PrivateIpAddress: aws.String("10.0.0.5"),
		},
		PublicIP:              "203.0.113.7",
		PlacementGroupName:    "pg-1",
		IamInstanceProfileArn: "arn:aws:iam::123456789012:instance-profile/role",
		CapacityReservationId: "cr-1",
		InstanceLifecycle:     "spot",
		SpotInstanceRequestId: "sir-1",
	}
}

// runningCfg is the projection the running describe path passes.
func runningCfg() InstanceProjection {
	return InstanceProjection{
		Region:                "us-east-1",
		DNSBaseDomain:         "example.com",
		DNSInternalDomain:     "compute.internal",
		AZ:                    "us-east-1a",
		IncludeRuntimeNetwork: true,
		FallbackStateName:     "pending",
	}
}

// stoppedCfg is the projection the stopped/terminated KV path passes.
func stoppedCfg(code int64, name string) InstanceProjection {
	return InstanceProjection{
		AZ:                "us-east-1a",
		FallbackStateCode: code,
		FallbackStateName: name,
	}
}

func TestProjectInstance_RunningProjectsAllFields(t *testing.T) {
	got, mapped := ProjectInstance(fullVM(vm.StateRunning), runningCfg())

	require.True(t, mapped)
	assert.Equal(t, int64(16), aws.Int64Value(got.State.Code))
	assert.Equal(t, "running", aws.StringValue(got.State.Name))

	// Runtime network is present for a running instance.
	assert.Equal(t, "203.0.113.7", aws.StringValue(got.PublicIpAddress))
	assert.NotEmpty(t, aws.StringValue(got.PublicDnsName))
	assert.NotEmpty(t, aws.StringValue(got.PrivateDnsName))

	require.NotNil(t, got.Placement)
	assert.Equal(t, "pg-1", aws.StringValue(got.Placement.GroupName))
	assert.Equal(t, "us-east-1a", aws.StringValue(got.Placement.AvailabilityZone))

	require.NotNil(t, got.IamInstanceProfile)
	assert.Equal(t, "arn:aws:iam::123456789012:instance-profile/role", aws.StringValue(got.IamInstanceProfile.Arn))
	// Id is left for the gateway to resolve post-aggregation.
	assert.Nil(t, got.IamInstanceProfile.Id)

	assert.Equal(t, "cr-1", aws.StringValue(got.CapacityReservationId))
	require.NotNil(t, got.CapacityReservationSpecification)

	assert.Equal(t, "spot", aws.StringValue(got.InstanceLifecycle))
	assert.Equal(t, "sir-1", aws.StringValue(got.SpotInstanceRequestId))
}

// TestProjectInstance_StoppedRetainsPlacementAndSpot is the direct bug fix:
// Placement and Spot lineage survive a stop, while the runtime network and the
// capacity reservation — all released by AWS on stop — do not.
func TestProjectInstance_StoppedRetainsPlacementAndSpot(t *testing.T) {
	got, mapped := ProjectInstance(fullVM(vm.StateStopped), stoppedCfg(80, "stopped"))

	require.True(t, mapped)
	assert.Equal(t, "stopped", aws.StringValue(got.State.Name))

	// Retained across the stop.
	require.NotNil(t, got.Placement)
	assert.Equal(t, "pg-1", aws.StringValue(got.Placement.GroupName))
	assert.Equal(t, "spot", aws.StringValue(got.InstanceLifecycle))
	assert.Equal(t, "sir-1", aws.StringValue(got.SpotInstanceRequestId))

	// Released on stop, so never projected onto a stopped instance.
	assert.Nil(t, got.PublicIpAddress)
	assert.Nil(t, got.PublicDnsName)
	assert.Nil(t, got.PrivateDnsName)
	assert.Nil(t, got.CapacityReservationId)
	assert.Nil(t, got.CapacityReservationSpecification)
}

// TestProjectInstance_EmptyIamArnClearsStaleProfile guards the auto-clear: an
// empty ARN on the VM record must wipe any profile left on the stored instance.
func TestProjectInstance_EmptyIamArnClearsStaleProfile(t *testing.T) {
	v := fullVM(vm.StateRunning)
	v.IamInstanceProfileArn = ""
	v.Instance.IamInstanceProfile = &ec2.IamInstanceProfile{Arn: aws.String("arn:aws:iam::123456789012:instance-profile/stale")}

	got, _ := ProjectInstance(v, runningCfg())

	assert.Nil(t, got.IamInstanceProfile)
}

// TestProjectInstance_UnmappedStatusUsesFallback covers a stored status with no
// EC2 equivalent: the caller's fallback labels it and stateMapped reports the gap.
func TestProjectInstance_UnmappedStatusUsesFallback(t *testing.T) {
	got, mapped := ProjectInstance(fullVM(vm.InstanceState("weird-status")), stoppedCfg(48, "terminated"))

	assert.False(t, mapped)
	assert.Equal(t, int64(48), aws.Int64Value(got.State.Code))
	assert.Equal(t, "terminated", aws.StringValue(got.State.Name))
}

// TestProjectInstance_DoesNotMutateSource confirms the projection writes only to
// its fresh copy, never back to the stored vm.VM.Instance shared under the lock.
func TestProjectInstance_DoesNotMutateSource(t *testing.T) {
	v := fullVM(vm.StateRunning)

	_, _ = ProjectInstance(v, runningCfg())

	assert.Nil(t, v.Instance.PublicIpAddress)
	assert.Nil(t, v.Instance.State)
	assert.Nil(t, v.Instance.Placement)
}

// TestProjectInstance_PathsAgreeOnRetainedFields is the regression guard that
// would have caught the original drift: for one vm.VM, the fields AWS keeps
// across a stop must be identical whether projected by the running path or the
// stopped path. The running-only fields must, conversely, differ.
func TestProjectInstance_PathsAgreeOnRetainedFields(t *testing.T) {
	v := fullVM(vm.StateStopped)

	running, _ := ProjectInstance(v, runningCfg())
	stopped, _ := ProjectInstance(v, stoppedCfg(80, "stopped"))

	// Retained-field set must match across both paths.
	assert.Equal(t, running.Placement, stopped.Placement)
	assert.Equal(t, running.InstanceLifecycle, stopped.InstanceLifecycle)
	assert.Equal(t, running.SpotInstanceRequestId, stopped.SpotInstanceRequestId)
	assert.Equal(t, running.IamInstanceProfile, stopped.IamInstanceProfile)

	// Runtime-only fields are intentionally path-specific.
	assert.NotEqual(t, running.PublicIpAddress, stopped.PublicIpAddress)
	assert.NotEqual(t, running.CapacityReservationId, stopped.CapacityReservationId)
}

func TestParseInstanceIDFilter(t *testing.T) {
	tests := []struct {
		name    string
		ids     []*string
		want    map[string]bool
		wantErr bool
	}{
		{
			name: "valid i- IDs",
			ids:  []*string{aws.String("i-aaa"), aws.String("i-bbb")},
			want: map[string]bool{"i-aaa": true, "i-bbb": true},
		},
		{
			name:    "malformed ID rejected",
			ids:     []*string{aws.String("not-an-id")},
			wantErr: true,
		},
		{
			name:    "empty string rejected as malformed",
			ids:     []*string{aws.String("")},
			wantErr: true,
		},
		{
			name: "nil entries skipped",
			ids:  []*string{nil, aws.String("i-aaa"), nil},
			want: map[string]bool{"i-aaa": true},
		},
		{
			name: "empty input yields empty filter",
			ids:  nil,
			want: map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInstanceIDFilter(tt.ids)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
