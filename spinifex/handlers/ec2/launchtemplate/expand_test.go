package handlers_ec2_launchtemplate

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandRunInstances_NoTemplate(t *testing.T) {
	svc := setupTestService(t)
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-direct"),
		InstanceType: aws.String("t3.nano"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	require.NoError(t, ExpandRunInstances(context.Background(), svc, input, testAccountID))
	assert.Equal(t, "ami-direct", aws.StringValue(input.ImageId), "untouched when no template referenced")
	assert.Equal(t, "t3.nano", aws.StringValue(input.InstanceType))
}

func TestExpandRunInstances_ByIdDefaultVersion(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro") // v1: ami-123, t3.micro
	input := &ec2.RunInstancesInput{
		MinCount:       aws.Int64(2),
		MaxCount:       aws.Int64(3),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{LaunchTemplateId: lt.LaunchTemplateId},
	}
	require.NoError(t, ExpandRunInstances(context.Background(), svc, input, testAccountID))
	assert.Equal(t, "ami-123", aws.StringValue(input.ImageId), "template image applied")
	assert.Equal(t, "t3.micro", aws.StringValue(input.InstanceType), "template type applied")
	assert.Nil(t, input.LaunchTemplate, "template spec cleared after expansion")
	assert.Equal(t, int64(2), aws.Int64Value(input.MinCount), "counts come from the direct request")
	assert.Equal(t, int64(3), aws.Int64Value(input.MaxCount))
}

func TestExpandRunInstances_DirectParamsWin(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")
	input := &ec2.RunInstancesInput{
		InstanceType:   aws.String("t3.large"), // overrides template
		MinCount:       aws.Int64(1),
		MaxCount:       aws.Int64(1),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{LaunchTemplateName: aws.String("web")},
	}
	require.NoError(t, ExpandRunInstances(context.Background(), svc, input, testAccountID))
	assert.Equal(t, "t3.large", aws.StringValue(input.InstanceType), "direct param overrides template")
	assert.Equal(t, "ami-123", aws.StringValue(input.ImageId), "template image inherited")
}

func TestExpandRunInstances_SpecificVersion(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "t3.large", "") // v2: no SourceVersion, so image is unset

	input := &ec2.RunInstancesInput{
		MinCount: aws.Int64(1),
		MaxCount: aws.Int64(1),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(id),
			Version:          aws.String("2"),
		},
	}
	require.NoError(t, ExpandRunInstances(context.Background(), svc, input, testAccountID))
	assert.Equal(t, "t3.large", aws.StringValue(input.InstanceType))
	assert.Nil(t, input.ImageId, "v2 carried no image")
}

func TestExpandRunInstances_MissingVersion(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	input := &ec2.RunInstancesInput{
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateId: lt.LaunchTemplateId,
			Version:          aws.String("99"),
		},
	}
	err := ExpandRunInstances(context.Background(), svc, input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, err.Error())
}

func TestExpandRunInstances_IdNameConflict(t *testing.T) {
	svc := setupTestService(t)
	input := &ec2.RunInstancesInput{
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateId:   aws.String("lt-123"),
			LaunchTemplateName: aws.String("web"),
		},
	}
	err := ExpandRunInstances(context.Background(), svc, input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestExpandRunInstances_NeitherIdNorName(t *testing.T) {
	svc := setupTestService(t)
	input := &ec2.RunInstancesInput{LaunchTemplate: &ec2.LaunchTemplateSpecification{}}
	err := ExpandRunInstances(context.Background(), svc, input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestExpandRunInstances_UnknownTemplate(t *testing.T) {
	svc := setupTestService(t)
	input := &ec2.RunInstancesInput{
		LaunchTemplate: &ec2.LaunchTemplateSpecification{LaunchTemplateId: aws.String("lt-doesnotexist000")},
	}
	err := ExpandRunInstances(context.Background(), svc, input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, err.Error())
}
