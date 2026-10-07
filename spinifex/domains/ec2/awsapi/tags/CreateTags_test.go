package tags

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateTagsInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateTagsInput
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
			name:    "EmptyInput",
			input:   &ec2.CreateTagsInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MissingTags",
			input: &ec2.CreateTagsInput{
				Resources: []*string{aws.String("i-1234567890abcdef0")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "EmptyTagKey",
			input: &ec2.CreateTagsInput{
				Resources: []*string{aws.String("i-1234567890abcdef0")},
				Tags: []*ec2.Tag{
					{Key: aws.String(""), Value: aws.String("value")},
				},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidInput",
			input: &ec2.CreateTagsInput{
				Resources: []*string{aws.String("i-1234567890abcdef0")},
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("MyInstance")},
				},
			},
			wantErr: false,
		},
		{
			name: "ValidInput_MultipleResourcesAndTags",
			input: &ec2.CreateTagsInput{
				Resources: []*string{
					aws.String("i-1234567890abcdef0"),
					aws.String("vol-1234567890abcdef0"),
				},
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("MyResource")},
					{Key: aws.String("Environment"), Value: aws.String("Production")},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateTagsInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateDeleteTagsInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteTagsInput
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
			name:    "EmptyInput",
			input:   &ec2.DeleteTagsInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInput_WithResources",
			input: &ec2.DeleteTagsInput{
				Resources: []*string{aws.String("i-1234567890abcdef0")},
			},
			wantErr: false,
		},
		{
			name: "ValidInput_WithResourcesAndTags",
			input: &ec2.DeleteTagsInput{
				Resources: []*string{aws.String("i-1234567890abcdef0")},
				Tags: []*ec2.Tag{
					{Key: aws.String("Name")},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteTagsInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateCreateTagsInput_RejectsReservedAWSPrefix(t *testing.T) {
	for _, key := range []string{"aws:foo", "AWS:Foo"} {
		err := ValidateCreateTagsInput(&ec2.CreateTagsInput{
			Resources: []*string{aws.String("vpc-1234567890abcdef0")},
			Tags:      []*ec2.Tag{{Key: aws.String(key), Value: aws.String("v")}},
		})
		code, msg, ok := awserrors.ResolveErrorDetail(err)
		require.True(t, ok, "error %v carries no AWS code", err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
		assert.Equal(t, "Value ( "+key+" ) for parameter key is invalid. Tag keys starting with 'aws:' are reserved for internal use", msg)
	}

	assert.NoError(t, ValidateCreateTagsInput(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vpc-1234567890abcdef0")},
		Tags:      []*ec2.Tag{{Key: aws.String("team:aws:owner"), Value: aws.String("v")}},
	}))
}
