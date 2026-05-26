package gateway_ec2_image

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func validRegisterImageInput() *ec2.RegisterImageInput {
	return &ec2.RegisterImageInput{
		Name:           aws.String("my-image"),
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					SnapshotId: aws.String("snap-1234567890abcdef0"),
					VolumeSize: aws.Int64(20),
				},
			},
		},
	}
}

func TestValidateRegisterImageInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ec2.RegisterImageInput)
		input   *ec2.RegisterImageInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "MissingName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptyName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = aws.String("") },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "ShortName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = aws.String("ab") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMINameMalformed,
		},
		{
			name: "LongName",
			mutate: func(i *ec2.RegisterImageInput) {
				name := make([]byte, 129)
				for j := range name {
					name[j] = 'a'
				}
				i.Name = aws.String(string(name))
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMINameMalformed,
		},
		{
			name:    "MissingBlockDeviceMappings",
			mutate:  func(i *ec2.RegisterImageInput) { i.BlockDeviceMappings = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MissingSnapshotIdInRootBDM",
			mutate: func(i *ec2.RegisterImageInput) {
				i.BlockDeviceMappings[0].Ebs.SnapshotId = nil
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MalformedSnapshotId",
			mutate: func(i *ec2.RegisterImageInput) {
				i.BlockDeviceMappings[0].Ebs.SnapshotId = aws.String("not-a-snapshot")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidSnapshotIDMalformed,
		},
		{
			name: "RootDeviceNameWithNoMatchingBDM",
			mutate: func(i *ec2.RegisterImageInput) {
				i.RootDeviceName = aws.String("/dev/xvda")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "InvalidArchitecture",
			mutate: func(i *ec2.RegisterImageInput) {
				i.Architecture = aws.String("riscv")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ParavirtualVirtualizationRejected",
			mutate: func(i *ec2.RegisterImageInput) {
				i.VirtualizationType = aws.String("paravirtual")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ImageLocationRejected",
			mutate: func(i *ec2.RegisterImageInput) {
				i.ImageLocation = aws.String("s3://bundles/my-ami.manifest.xml")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "BootModeAcceptedBios",
			mutate:  func(i *ec2.RegisterImageInput) { i.BootMode = aws.String("bios") },
			wantErr: false,
		},
		{
			name:    "BootModeAcceptedUEFI",
			mutate:  func(i *ec2.RegisterImageInput) { i.BootMode = aws.String("uefi") },
			wantErr: false,
		},
		{
			name:    "BootModeAcceptedUEFIPreferred",
			mutate:  func(i *ec2.RegisterImageInput) { i.BootMode = aws.String("uefi-preferred") },
			wantErr: false,
		},
		{
			name:    "BootModeRejectedInvalidValue",
			mutate:  func(i *ec2.RegisterImageInput) { i.BootMode = aws.String("legacy-bios") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "KernelIdRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.KernelId = aws.String("aki-123") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "RamdiskIdRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.RamdiskId = aws.String("ari-123") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "TpmSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.TpmSupport = aws.String("v2.0") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "ImdsSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.ImdsSupport = aws.String("v2.0") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EnaSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.EnaSupport = aws.Bool(true) },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "SriovNetSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.SriovNetSupport = aws.String("simple") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidWithExplicitArch",
			mutate: func(i *ec2.RegisterImageInput) {
				i.Architecture = aws.String("arm64")
				i.VirtualizationType = aws.String("hvm")
			},
			wantErr: false,
		},
		{
			name: "ValidNoRootDeviceName",
			mutate: func(i *ec2.RegisterImageInput) {
				i.RootDeviceName = nil
			},
			wantErr: false,
		},
		{
			name:    "ValidMinimal",
			wantErr: false,
		},
		{
			name: "ValidWithTagSpecifications",
			mutate: func(i *ec2.RegisterImageInput) {
				i.TagSpecifications = []*ec2.TagSpecification{
					{
						ResourceType: aws.String("image"),
						Tags: []*ec2.Tag{
							{Key: aws.String("Env"), Value: aws.String("test")},
						},
					},
				}
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input *ec2.RegisterImageInput
			if tt.input == nil && tt.name != "NilInput" {
				input = validRegisterImageInput()
			} else {
				input = tt.input
			}
			if tt.mutate != nil {
				tt.mutate(input)
			}

			err := ValidateRegisterImageInput(input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRegisterImage_GatewayValidationFailureReturnsEarly(t *testing.T) {
	// nil natsConn is fine here: validation rejects the input before any
	// NATS round-trip is attempted, so we exercise the early-exit path.
	_, err := RegisterImage(&ec2.RegisterImageInput{}, nil, "000000000001")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestSelectRootBlockDeviceMapping_NoRootDeviceName(t *testing.T) {
	mappings := []*ec2.BlockDeviceMapping{
		nil,
		{Ebs: nil},
		{Ebs: &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-first")}},
		{Ebs: &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-second")}},
	}
	bdm := selectRootBlockDeviceMapping(mappings, nil)
	assert.NotNil(t, bdm)
	assert.Equal(t, "snap-first", *bdm.Ebs.SnapshotId)

	// Empty (non-nil) RootDeviceName behaves the same as nil.
	bdm = selectRootBlockDeviceMapping(mappings, aws.String(""))
	assert.NotNil(t, bdm)
	assert.Equal(t, "snap-first", *bdm.Ebs.SnapshotId)

	// Nothing usable.
	bdm = selectRootBlockDeviceMapping([]*ec2.BlockDeviceMapping{nil, {}}, nil)
	assert.Nil(t, bdm)
}
