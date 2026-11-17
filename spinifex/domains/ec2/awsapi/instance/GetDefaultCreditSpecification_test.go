package instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDefaultCreditSpecification_Families(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"t2":  "standard",
		"t3":  "unlimited",
		"t3a": "unlimited",
		"t4g": "unlimited",
		"t8i": "unlimited",
	}
	for family, credits := range want {
		out, err := GetDefaultCreditSpecification(&ec2.GetDefaultCreditSpecificationInput{InstanceFamily: aws.String(family)})
		require.NoError(t, err, family)
		require.NotNil(t, out.InstanceFamilyCreditSpecification, family)
		assert.Equal(t, family, aws.StringValue(out.InstanceFamilyCreditSpecification.InstanceFamily))
		assert.Equal(t, credits, aws.StringValue(out.InstanceFamilyCreditSpecification.CpuCredits), family)
	}
}

func TestGetDefaultCreditSpecification_UnknownFamily(t *testing.T) {
	t.Parallel()
	_, err := GetDefaultCreditSpecification(&ec2.GetDefaultCreditSpecificationInput{InstanceFamily: aws.String("bogus")})
	require.Error(t, err)
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidInstanceFamily, code)
	assert.Equal(t, "The bogus instance family does not support Unlimited. Only burstable performance instance families support Unlimited. Change the instance family and try again.", msg)
}

func TestGetDefaultCreditSpecification_MissingFamily(t *testing.T) {
	t.Parallel()
	_, err := GetDefaultCreditSpecification(&ec2.GetDefaultCreditSpecificationInput{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}
