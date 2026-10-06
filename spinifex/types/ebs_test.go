package types

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVolumeType_AcceptsEmptyAndSupportedInAnyCase(t *testing.T) {
	assert.NoError(t, ValidateVolumeType(""))
	for _, vt := range SupportedVolumeTypes {
		assert.NoError(t, ValidateVolumeType(vt))
		assert.NoError(t, ValidateVolumeType(strings.ToUpper(vt)))
	}
}

// gp3 is the only type served; the other types AWS accepts are refused, not
// aliased to gp3.
func TestValidateVolumeType_RejectsOtherAWSTypes(t *testing.T) {
	for _, vt := range []string{"gp2", "io1", "io2", "st1", "sc1", "standard"} {
		code, _, ok := awserrors.ResolveErrorDetail(ValidateVolumeType(vt))
		require.True(t, ok, vt)
		assert.Equal(t, awserrors.ErrorUnknownVolumeType, code, vt)
	}
}

func TestValidateVolumeType_MessageNamesExactlyTheAcceptedSet(t *testing.T) {
	code, message, ok := awserrors.ResolveErrorDetail(ValidateVolumeType("foo"))
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorUnknownVolumeType, code)

	const prefix = "Unsupported volume type 'foo' for volume creation. Supported volume types: "
	require.True(t, strings.HasPrefix(message, prefix), message)
	named := strings.Split(strings.TrimSuffix(strings.TrimPrefix(message, prefix), "."), ", ")

	assert.ElementsMatch(t, SupportedVolumeTypes, named)
	for _, vt := range named {
		assert.NoError(t, ValidateVolumeType(vt), "the message names %q, so it must be accepted", vt)
	}
}
