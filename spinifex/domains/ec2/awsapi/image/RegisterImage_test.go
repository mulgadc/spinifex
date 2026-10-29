package image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		// wantIn is the text the message must carry, so the caller can see which
		// parameter was refused.
		wantIn string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "parameter Name",
		},
		{
			name:    "MissingName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "parameter Name",
		},
		{
			name:    "EmptyName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = aws.String("") },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "parameter Name",
		},
		{
			name:    "ShortName",
			mutate:  func(i *ec2.RegisterImageInput) { i.Name = aws.String("ab") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMINameMalformed,
			wantIn:  "for parameter Name is invalid",
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
			wantIn:  "for parameter Name is invalid",
		},
		{
			name:    "MissingBlockDeviceMappings",
			mutate:  func(i *ec2.RegisterImageInput) { i.BlockDeviceMappings = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "parameter BlockDeviceMapping",
		},
		{
			name: "MissingSnapshotIdInRootBDM",
			mutate: func(i *ec2.RegisterImageInput) {
				i.BlockDeviceMappings[0].Ebs.SnapshotId = nil
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "parameter SnapshotId",
		},
		{
			name: "MalformedSnapshotId",
			mutate: func(i *ec2.RegisterImageInput) {
				i.BlockDeviceMappings[0].Ebs.SnapshotId = aws.String("not-a-snapshot")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidSnapshotIDMalformed,
			wantIn:  "Value (not-a-snapshot) for parameter SnapshotId is invalid",
		},
		{
			name: "RootDeviceNameWithNoMatchingBDM",
			mutate: func(i *ec2.RegisterImageInput) {
				i.RootDeviceName = aws.String("/dev/xvda")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
			wantIn:  "Value (/dev/xvda) for parameter RootDeviceName is invalid",
		},
		{
			name: "InvalidArchitecture",
			mutate: func(i *ec2.RegisterImageInput) {
				i.Architecture = aws.String("riscv")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (riscv) for parameter Architecture is invalid",
		},
		{
			name: "ParavirtualVirtualizationRejected",
			mutate: func(i *ec2.RegisterImageInput) {
				i.VirtualizationType = aws.String("paravirtual")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (paravirtual) for parameter VirtualizationType is invalid",
		},
		{
			name: "ImageLocationRejected",
			mutate: func(i *ec2.RegisterImageInput) {
				i.ImageLocation = aws.String("s3://bundles/my-ami.manifest.xml")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "for parameter ImageLocation is invalid. ImageLocation is not supported",
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
			wantIn:  "Value (legacy-bios) for parameter BootMode is invalid",
		},
		{
			name:    "KernelIdRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.KernelId = aws.String("aki-123") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (aki-123) for parameter KernelId is invalid. KernelId is not supported",
		},
		{
			name:    "RamdiskIdRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.RamdiskId = aws.String("ari-123") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (ari-123) for parameter RamdiskId is invalid. RamdiskId is not supported",
		},
		{
			name:    "TpmSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.TpmSupport = aws.String("v2.0") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (v2.0) for parameter TpmSupport is invalid. TpmSupport is not supported",
		},
		{
			name:    "ImdsSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.ImdsSupport = aws.String("v2.0") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (v2.0) for parameter ImdsSupport is invalid. ImdsSupport is not supported",
		},
		{
			name:    "EnaSupportAcceptedTrue",
			mutate:  func(i *ec2.RegisterImageInput) { i.EnaSupport = aws.Bool(true) },
			wantErr: false,
		},
		{
			name:    "EnaSupportAcceptedFalse",
			mutate:  func(i *ec2.RegisterImageInput) { i.EnaSupport = aws.Bool(false) },
			wantErr: false,
		},
		{
			name:    "SriovNetSupportRejected",
			mutate:  func(i *ec2.RegisterImageInput) { i.SriovNetSupport = aws.String("simple") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
			wantIn:  "Value (simple) for parameter SriovNetSupport is invalid. SriovNetSupport is not supported",
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
				require.Error(t, err)
				code, msg, ok := awserrors.ResolveErrorDetail(err)
				require.True(t, ok, "the error must carry a registered code")
				assert.Equal(t, tt.errMsg, code)
				assert.NotEmpty(t, msg, "the response must say what was refused")
				assert.Contains(t, msg, tt.wantIn)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRegisterImage_GatewayValidationFailureReturnsEarly(t *testing.T) {
	// nil natsConn is fine here: validation rejects the input before any
	// NATS round-trip is attempted, so we exercise the early-exit path.
	_, err := RegisterImage(context.Background(), &ec2.RegisterImageInput{}, nil, "000000000001")
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorMissingParameter, code)
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
