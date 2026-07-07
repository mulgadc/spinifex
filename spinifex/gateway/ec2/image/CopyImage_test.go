package gateway_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testCopyImageRegion = "ap-southeast-2"

func validCopyImageInput() *ec2.CopyImageInput {
	return &ec2.CopyImageInput{
		Name:          aws.String("copied-ami"),
		SourceImageId: aws.String("ami-1234567890abcdef0"),
		SourceRegion:  aws.String(testCopyImageRegion),
	}
}

func TestValidateCopyImageInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ec2.CopyImageInput)
		input   *ec2.CopyImageInput
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
			mutate:  func(i *ec2.CopyImageInput) { i.Name = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptyName",
			mutate:  func(i *ec2.CopyImageInput) { i.Name = aws.String("") },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "ShortName",
			mutate:  func(i *ec2.CopyImageInput) { i.Name = aws.String("ab") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMINameMalformed,
		},
		{
			name: "LongName",
			mutate: func(i *ec2.CopyImageInput) {
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
			name:    "MissingSourceImageId",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceImageId = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptySourceImageId",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceImageId = aws.String("") },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "MalformedSourceImageId",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceImageId = aws.String("not-an-ami") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name:    "MissingSourceRegion",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceRegion = nil },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptySourceRegion",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceRegion = aws.String("") },
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "CrossRegionRejected",
			mutate:  func(i *ec2.CopyImageInput) { i.SourceRegion = aws.String("us-east-1") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EncryptedRejected",
			mutate:  func(i *ec2.CopyImageInput) { i.Encrypted = aws.Bool(true) },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "KmsKeyIdRejected",
			mutate:  func(i *ec2.CopyImageInput) { i.KmsKeyId = aws.String("alias/some-key") },
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "DestinationOutpostArnRejected",
			mutate: func(i *ec2.CopyImageInput) {
				i.DestinationOutpostArn = aws.String("arn:aws:outposts:us-east-1:123:outpost/op-abc")
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "ValidMinimal",
			wantErr: false,
		},
		{
			name: "ValidWithDescriptionAndClientToken",
			mutate: func(i *ec2.CopyImageInput) {
				i.Description = aws.String("renamed clone")
				i.ClientToken = aws.String("idempotency-token-ignored")
			},
			wantErr: false,
		},
		{
			name: "ValidWithCopyImageTagsTrue",
			mutate: func(i *ec2.CopyImageInput) {
				i.CopyImageTags = aws.Bool(true)
			},
			wantErr: false,
		},
		{
			name: "ValidWithCopyImageTagsFalse",
			mutate: func(i *ec2.CopyImageInput) {
				i.CopyImageTags = aws.Bool(false)
			},
			wantErr: false,
		},
		{
			name: "ValidWithTagSpecifications",
			mutate: func(i *ec2.CopyImageInput) {
				i.TagSpecifications = []*ec2.TagSpecification{
					{
						ResourceType: aws.String("image"),
						Tags: []*ec2.Tag{
							{Key: aws.String("Env"), Value: aws.String("prod")},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "EncryptedFalseAllowed",
			mutate: func(i *ec2.CopyImageInput) {
				i.Encrypted = aws.Bool(false)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input *ec2.CopyImageInput
			if tt.input == nil && tt.name != "NilInput" {
				input = validCopyImageInput()
			} else {
				input = tt.input
			}
			if tt.mutate != nil {
				tt.mutate(input)
			}

			err := ValidateCopyImageInput(input, testCopyImageRegion)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCopyImage_GatewayValidationFailureReturnsEarly(t *testing.T) {
	// nil natsConn is fine: validation rejects the input before any NATS
	// round-trip is attempted, so we exercise the early-exit path.
	_, err := CopyImage(context.Background(), &ec2.CopyImageInput{}, nil, testCopyImageRegion, "000000000001")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}
