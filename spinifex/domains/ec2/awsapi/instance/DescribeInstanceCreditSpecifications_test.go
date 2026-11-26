package instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeInstanceCreditSpecifications_NilInput(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(nil)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeInstanceCreditSpecifications_EmptyInstanceIds(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{})
	require.NoError(t, err)
	assert.Empty(t, out.InstanceCreditSpecifications)
}

func TestDescribeInstanceCreditSpecifications_SingleInstance(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: []*string{aws.String("i-abc123")},
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceCreditSpecifications, 1)
	assert.Equal(t, "i-abc123", *out.InstanceCreditSpecifications[0].InstanceId)
	assert.Equal(t, "standard", *out.InstanceCreditSpecifications[0].CpuCredits)
}

func TestDescribeInstanceCreditSpecifications_MultipleInstances(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: []*string{aws.String("i-aaa"), aws.String("i-bbb"), aws.String("i-ccc")},
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceCreditSpecifications, 3)
	for _, spec := range out.InstanceCreditSpecifications {
		assert.Equal(t, "standard", *spec.CpuCredits)
	}
}

func TestDescribeInstanceCreditSpecifications_SkipsNilAndEmpty(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: []*string{nil, aws.String(""), aws.String("i-valid"), nil, aws.String("i-also")},
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceCreditSpecifications, 2)
	assert.Equal(t, "i-also", *out.InstanceCreditSpecifications[0].InstanceId)
	assert.Equal(t, "i-valid", *out.InstanceCreditSpecifications[1].InstanceId)
}

func TestDescribeInstanceCreditSpecifications_NextTokenResumesListing(t *testing.T) {
	t.Parallel()
	out, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: []*string{aws.String("i-aaa"), aws.String("i-bbb"), aws.String("i-ccc")},
		NextToken:   aws.String(paging.EncodeToken("i-bbb")),
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceCreditSpecifications, 2)
	assert.Equal(t, "i-bbb", *out.InstanceCreditSpecifications[0].InstanceId)
	assert.Equal(t, "i-ccc", *out.InstanceCreditSpecifications[1].InstanceId)
	assert.Nil(t, out.NextToken)
}

func TestDescribeInstanceCreditSpecifications_RefusesForeignToken(t *testing.T) {
	t.Parallel()
	_, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		NextToken: aws.String("not-a-token"),
	})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), err)
}

func TestDescribeInstanceCreditSpecifications_RefusesMaxResultsWithInstanceIds(t *testing.T) {
	t.Parallel()
	_, err := DescribeInstanceCreditSpecifications(&ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: []*string{aws.String("i-aaa")}, MaxResults: aws.Int64(5),
	})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterCombination), err)
}
