package gateway_ec2_instance

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

var defaults = ec2.RunInstancesInput{
	ImageId:      aws.String("ami-0abcdef1234567890"),
	InstanceType: aws.String("t2.micro"),
	MinCount:     aws.Int64(1),
	MaxCount:     aws.Int64(1),
	KeyName:      aws.String("my-key-pair"),
	SecurityGroupIds: []*string{
		aws.String("sg-0123456789abcdef0"),
	},
	SubnetId: aws.String("subnet-6e7f829e"),
}

func TestParseRunInstances(t *testing.T) {
	// Group multiple tests
	tests := []struct {
		name  string
		input *ec2.RunInstancesInput
		want  error
	}{

		{
			name: "InvalidMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(0),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(0),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(0),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidNoImageId",
			input: &ec2.RunInstancesInput{
				ImageId:          aws.String(""),
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "InvalidNoInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     aws.String(""),
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "InvalidNoInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          aws.String("invalid-name-here"),
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidAMIIDMalformed),
		},

		{
			name:  "NilInput",
			input: nil,
			want:  errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         nil,
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "MinCountGreaterThanMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(5),
				MaxCount:         aws.Int64(2),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "NilImageId",
			input: &ec2.RunInstancesInput{
				ImageId:          nil,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     nil,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         nil,
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "MissingKeyName",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          nil,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		// Successful test
		{
			name: "ValidTest",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Skip the valid test as it requires full daemon infrastructure
			// These tests are covered by the integration tests in service_nats_test.go
			if test.want == nil {
				t.Skip("Skipping valid test - requires full daemon infrastructure")
			}

			// For validation tests, we can pass nil conn since validation happens before NATS call
			response, err := RunInstances(test.input, nil, nil, "123456789012", nil)

			// Use assert to check if the error is as expected
			assert.Equal(t, test.want, err)

			if err != nil {
				assert.Len(t, response.Instances, 0)
			}
		})
	}

	// Additional test
}
