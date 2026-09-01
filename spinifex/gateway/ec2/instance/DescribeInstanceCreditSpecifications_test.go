package gateway_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
	assert.Equal(t, "i-valid", *out.InstanceCreditSpecifications[0].InstanceId)
	assert.Equal(t, "i-also", *out.InstanceCreditSpecifications[1].InstanceId)
}
