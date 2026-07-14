package gateway_ec2_placementgroup

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

// CreatePlacementGroup tests

func TestCreatePlacementGroup_NilInput(t *testing.T) {
	_, err := CreatePlacementGroup(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreatePlacementGroup_NilGroupName(t *testing.T) {
	_, err := CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		Strategy: aws.String("cluster"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreatePlacementGroup_EmptyGroupName(t *testing.T) {
	_, err := CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(""),
		Strategy:  aws.String("cluster"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreatePlacementGroup_NilStrategy_PassesValidation(t *testing.T) {
	err := ValidateCreatePlacementGroupInput(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String("my-group"),
	})
	assert.NoError(t, err)
}

func TestCreatePlacementGroup_EmptyStrategy_PassesValidation(t *testing.T) {
	err := ValidateCreatePlacementGroupInput(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String("my-group"),
		Strategy:  aws.String(""),
	})
	assert.NoError(t, err)
}

func TestCreatePlacementGroup_NilNATS(t *testing.T) {
	_, err := CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("my-group"),
		Strategy:  aws.String("cluster"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DeletePlacementGroup tests

func TestDeletePlacementGroup_NilInput(t *testing.T) {
	_, err := DeletePlacementGroup(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeletePlacementGroup_NilGroupName(t *testing.T) {
	_, err := DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeletePlacementGroup_EmptyGroupName(t *testing.T) {
	_, err := DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String(""),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeletePlacementGroup_NilNATS(t *testing.T) {
	_, err := DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("my-group"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DescribePlacementGroups tests

func TestDescribePlacementGroups_NilInput(t *testing.T) {
	_, err := DescribePlacementGroups(context.Background(), nil, nil, testAccountID)
	assert.Error(t, err)
}

func TestDescribePlacementGroups_NilNATS(t *testing.T) {
	_, err := DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{}, nil, testAccountID)
	assert.Error(t, err)
}
