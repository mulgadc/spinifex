package image

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertCodeAndMessage(t *testing.T, err error, code, message string) {
	t.Helper()
	gotCode, gotMessage, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	assert.Equal(t, code, gotCode)
	assert.Equal(t, message, gotMessage)
}

func TestValidateImageDeregistrationProtectionInput(t *testing.T) {
	malformed := "The image ID 'ami-zzz' is malformed"
	assertCodeAndMessage(t, ValidateEnableImageDeregistrationProtectionInput(&ec2.EnableImageDeregistrationProtectionInput{ImageId: aws.String("ami-zzz")}),
		awserrors.ErrorInvalidAMIIDMalformed, malformed)
	assertCodeAndMessage(t, ValidateDisableImageDeregistrationProtectionInput(&ec2.DisableImageDeregistrationProtectionInput{ImageId: aws.String("ami-zzz")}),
		awserrors.ErrorInvalidAMIIDMalformed, malformed)

	missing := "The request must contain the parameter ImageId"
	assertCodeAndMessage(t, ValidateEnableImageDeregistrationProtectionInput(&ec2.EnableImageDeregistrationProtectionInput{}), awserrors.ErrorMissingParameter, missing)
	assertCodeAndMessage(t, ValidateDisableImageDeregistrationProtectionInput(&ec2.DisableImageDeregistrationProtectionInput{}), awserrors.ErrorMissingParameter, missing)

	// The cooldown is not modelled, so it is refused rather than ignored.
	assertCodeAndMessage(t, ValidateEnableImageDeregistrationProtectionInput(&ec2.EnableImageDeregistrationProtectionInput{
		ImageId: aws.String("ami-0123456789abcdef0"), WithCooldown: aws.Bool(true),
	}), awserrors.ErrorInvalidParameterValue, "Value (true) for parameter WithCooldown is invalid. WithCooldown is not supported.")

	require.NoError(t, ValidateEnableImageDeregistrationProtectionInput(&ec2.EnableImageDeregistrationProtectionInput{
		ImageId: aws.String("ami-0123456789abcdef0"), WithCooldown: aws.Bool(false),
	}))
	require.NoError(t, ValidateDisableImageDeregistrationProtectionInput(&ec2.DisableImageDeregistrationProtectionInput{ImageId: aws.String("ami-0123456789abcdef0")}))
}
