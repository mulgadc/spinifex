package gateway_ec2_volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestValidateDeleteVolumeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteVolumeInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EmptyInput_NoVolumeId",
			input:   &ec2.DeleteVolumeInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
		{
			name: "ValidVolumeId",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String("vol-0123456789abcdef0"),
			},
			wantErr: false,
		},
		{
			name: "ValidVolumeId_Short",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String("vol-abc123"),
			},
			wantErr: false,
		},
		{
			name: "InvalidVolumeId_NoPrefix",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String("invalid-id"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
		{
			name: "InvalidVolumeId_WrongPrefix",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String("ami-0123456789abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
		{
			name: "InvalidVolumeId_BarePrefix",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String("vol-"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
		{
			name: "InvalidVolumeId_EmptyString",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
		{
			name: "InvalidVolumeId_Nil",
			input: &ec2.DeleteVolumeInput{
				VolumeId: nil,
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidVolumeIDMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteVolumeInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func instanceWithVolume(instanceID, state, volumeID string) *ec2.Instance {
	return &ec2.Instance{
		InstanceId: aws.String(instanceID),
		State:      &ec2.InstanceState{Name: aws.String(state)},
		BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
			{DeviceName: aws.String("/dev/vda"), Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String(volumeID)}},
		},
	}
}

func reservations(instances ...*ec2.Instance) []*ec2.Reservation {
	return []*ec2.Reservation{{Instances: instances}}
}

func TestVolumeHeldByInstance(t *testing.T) {
	const vol = "vol-target"

	tests := []struct {
		name         string
		reservations []*ec2.Reservation
		want         string
	}{
		{
			name:         "RunningInstanceHoldsVolume",
			reservations: reservations(instanceWithVolume("i-running", ec2.InstanceStateNameRunning, vol)),
			want:         "i-running",
		},
		{
			name:         "StoppedInstanceHoldsVolume",
			reservations: reservations(instanceWithVolume("i-stopped", ec2.InstanceStateNameStopped, vol)),
			want:         "i-stopped",
		},
		{
			name:         "TerminatedInstanceIgnored",
			reservations: reservations(instanceWithVolume("i-dead", ec2.InstanceStateNameTerminated, vol)),
			want:         "",
		},
		{
			name:         "NoInstanceReferencesVolume",
			reservations: reservations(instanceWithVolume("i-other", ec2.InstanceStateNameRunning, "vol-different")),
			want:         "",
		},
		{
			name: "RunningWinsOverTerminatedDuplicate",
			reservations: reservations(
				instanceWithVolume("i-dead", ec2.InstanceStateNameTerminated, vol),
				instanceWithVolume("i-live", ec2.InstanceStateNameRunning, vol),
			),
			want: "i-live",
		},
		{
			name:         "EmptyReservations",
			reservations: nil,
			want:         "",
		},
		{
			name: "NilSafety",
			reservations: []*ec2.Reservation{
				nil,
				{Instances: []*ec2.Instance{nil, {InstanceId: aws.String("i-nobdm")}}},
				{Instances: []*ec2.Instance{{
					InstanceId:          aws.String("i-nilebs"),
					State:               &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameRunning)},
					BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{nil, {Ebs: nil}},
				}}},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, volumeHeldByInstance(tt.reservations, vol))
		})
	}
}
