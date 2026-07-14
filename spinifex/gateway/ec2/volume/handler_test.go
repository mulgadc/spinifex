package gateway_ec2_volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

// Handler tests — call handlers directly to cover validation + NATS error paths

func TestCreateVolume_ValidationErrors(t *testing.T) {
	_, err := CreateVolume(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = CreateVolume(context.Background(), &ec2.CreateVolumeInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateVolume_NilNATS(t *testing.T) {
	_, err := CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String("us-east-1a"),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDeleteVolume_ValidationErrors(t *testing.T) {
	_, err := DeleteVolume(context.Background(), nil, nil, 1, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String("bad")}, nil, 1, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeIDMalformed)
}

func TestDeleteVolume_NilNATS(t *testing.T) {
	_, err := DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-1234567890abcdef0"),
	}, nil, 1, "acct-123")
	assert.Error(t, err)
}

func TestAttachVolume_ValidationErrors(t *testing.T) {
	_, err := AttachVolume(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = AttachVolume(context.Background(), &ec2.AttachVolumeInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = AttachVolume(context.Background(), &ec2.AttachVolumeInput{
		VolumeId: aws.String("vol-123"),
	}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAttachVolume_NilNATS(t *testing.T) {
	_, err := AttachVolume(context.Background(), &ec2.AttachVolumeInput{
		VolumeId:   aws.String("vol-1234567890abcdef0"),
		InstanceId: aws.String("i-1234567890abcdef0"),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDetachVolume_ValidationErrors(t *testing.T) {
	_, err := DetachVolume(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = DetachVolume(context.Background(), &ec2.DetachVolumeInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDetachVolume_NilNATS(t *testing.T) {
	_, err := DetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId:   aws.String("vol-1234567890abcdef0"),
		InstanceId: aws.String("i-1234567890abcdef0"),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestModifyVolume_ValidationErrors(t *testing.T) {
	_, err := ModifyVolume(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{VolumeId: aws.String("bad")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeIDMalformed)
}

func TestModifyVolume_NilNATS(t *testing.T) {
	_, err := ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-1234567890abcdef0"),
		Size:     aws.Int64(20),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDescribeVolumes_ValidationErrors(t *testing.T) {
	_, err := DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("bad-id")},
	}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeIDMalformed)
}

func TestDescribeVolumes_NilNATS(t *testing.T) {
	_, err := DescribeVolumes(context.Background(), nil, nil, "acct-123")
	assert.Error(t, err)

	_, err = DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDescribeVolumeStatus_NilNATS(t *testing.T) {
	_, err := DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{}, nil, "acct-123")
	assert.Error(t, err)
}
