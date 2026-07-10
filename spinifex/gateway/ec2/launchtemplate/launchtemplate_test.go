package gateway_ec2_launchtemplate

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

func validData() *ec2.RequestLaunchTemplateData {
	return &ec2.RequestLaunchTemplateData{
		ImageId:      aws.String("ami-123"),
		InstanceType: aws.String("t3.micro"),
	}
}

// requireTemplateIdentity

func TestRequireTemplateIdentity(t *testing.T) {
	assert.EqualError(t, requireTemplateIdentity(nil, nil), awserrors.ErrorMissingParameter)
	assert.EqualError(t, requireTemplateIdentity(aws.String(""), aws.String("")), awserrors.ErrorMissingParameter)
	assert.EqualError(t, requireTemplateIdentity(aws.String("lt-1"), aws.String("name")), awserrors.ErrorInvalidParameterValue)
	assert.NoError(t, requireTemplateIdentity(aws.String("lt-1"), nil))
	assert.NoError(t, requireTemplateIdentity(nil, aws.String("name")))
}

// CreateLaunchTemplate

func TestCreateLaunchTemplate_NilInput(t *testing.T) {
	_, err := CreateLaunchTemplate(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateLaunchTemplate_MissingData(t *testing.T) {
	_, err := CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateLaunchTemplate_MissingName(t *testing.T) {
	_, err := CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateData: validData(),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// CreateLaunchTemplateVersion

func TestCreateLaunchTemplateVersion_MissingData(t *testing.T) {
	_, err := CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String("lt-1"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateLaunchTemplateVersion_BothIdAndName(t *testing.T) {
	_, err := CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateData: validData(),
		LaunchTemplateId:   aws.String("lt-1"),
		LaunchTemplateName: aws.String("web"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

// DeleteLaunchTemplate

func TestDeleteLaunchTemplate_MissingIdentity(t *testing.T) {
	_, err := DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// DeleteLaunchTemplateVersions

func TestDeleteLaunchTemplateVersions_MissingVersions(t *testing.T) {
	_, err := DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String("lt-1"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteLaunchTemplateVersions_MissingIdentity(t *testing.T) {
	_, err := DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		Versions: aws.StringSlice([]string{"1"}),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// ModifyLaunchTemplate

func TestModifyLaunchTemplate_MissingIdentity(t *testing.T) {
	_, err := ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
		DefaultVersion: aws.String("2"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// DescribeLaunchTemplateVersions

func TestDescribeLaunchTemplateVersions_MissingIdentity(t *testing.T) {
	_, err := DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// Forward path: valid input with a nil NATS conn reaches the service client,
// which fails cluster-unavailable. Exercises the validate-then-forward wiring.

func TestForwardPath_NilNATS(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{"CreateLaunchTemplate", func() error {
			_, err := CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
				LaunchTemplateName: aws.String("web"), LaunchTemplateData: validData(),
			}, nil, testAccountID)
			return err
		}},
		{"CreateLaunchTemplateVersion", func() error {
			_, err := CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
				LaunchTemplateId: aws.String("lt-1"), LaunchTemplateData: validData(),
			}, nil, testAccountID)
			return err
		}},
		{"DeleteLaunchTemplate", func() error {
			_, err := DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{
				LaunchTemplateId: aws.String("lt-1"),
			}, nil, testAccountID)
			return err
		}},
		{"DeleteLaunchTemplateVersions", func() error {
			_, err := DeleteLaunchTemplateVersions(ctx, &ec2.DeleteLaunchTemplateVersionsInput{
				LaunchTemplateId: aws.String("lt-1"), Versions: aws.StringSlice([]string{"1"}),
			}, nil, testAccountID)
			return err
		}},
		{"ModifyLaunchTemplate", func() error {
			_, err := ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
				LaunchTemplateId: aws.String("lt-1"), DefaultVersion: aws.String("2"),
			}, nil, testAccountID)
			return err
		}},
		{"DescribeLaunchTemplates", func() error {
			_, err := DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{}, nil, testAccountID)
			return err
		}},
		{"DescribeLaunchTemplateVersions", func() error {
			_, err := DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
				LaunchTemplateId: aws.String("lt-1"),
			}, nil, testAccountID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, tt.call())
		})
	}
}
