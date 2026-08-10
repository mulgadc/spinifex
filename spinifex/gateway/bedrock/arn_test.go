package gateway_bedrock

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ptTestRegion   = "us-east-1"
	ptTestAccount  = "000000000001"
	ptOtherAccount = "000000000002"
	ptTestID       = "9f8b3c1e4a5d"
)

func TestFormatProvisionedModelARN(t *testing.T) {
	arn := FormatProvisionedModelARN(ptTestRegion, ptTestAccount, ptTestID)
	assert.Equal(t, "arn:aws:bedrock:us-east-1:000000000001:provisioned-model/9f8b3c1e4a5d", arn)
}

// TestParseProvisionedModelARN_RoundTrip proves a formatted ARN parses back
// to the same region/account/id it was built from.
func TestParseProvisionedModelARN_RoundTrip(t *testing.T) {
	arn := FormatProvisionedModelARN(ptTestRegion, ptTestAccount, ptTestID)
	parsed, err := ParseProvisionedModelARN(arn, ptTestRegion, ptTestAccount)
	require.NoError(t, err)
	assert.Equal(t, ptTestRegion, parsed.Region)
	assert.Equal(t, ptTestAccount, parsed.AccountID)
	assert.Equal(t, ptTestID, parsed.ID)
}

// TestParseProvisionedModelARN_RejectsForeignAccount guards the exact
// invariant the plan calls out: a caller must never resolve another tenant's
// commitment by presenting its ARN with their own credentials.
func TestParseProvisionedModelARN_RejectsForeignAccount(t *testing.T) {
	arn := FormatProvisionedModelARN(ptTestRegion, ptOtherAccount, ptTestID)
	_, err := ParseProvisionedModelARN(arn, ptTestRegion, ptTestAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

func TestParseProvisionedModelARN_RejectsForeignRegion(t *testing.T) {
	arn := FormatProvisionedModelARN("us-west-2", ptTestAccount, ptTestID)
	_, err := ParseProvisionedModelARN(arn, ptTestRegion, ptTestAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

func TestParseProvisionedModelARN_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-an-arn",
		"arn:aws:rds:us-east-1:000000000001:provisioned-model/abc",
		"arn:aws:bedrock:us-east-1:000000000001:custom-model/abc",
		"arn:aws:bedrock:us-east-1:000000000001:provisioned-model/",
		"arn:aws:bedrock:us-east-1:000000000001:provisioned-model",
	}
	for _, arn := range cases {
		t.Run(arn, func(t *testing.T) {
			_, err := ParseProvisionedModelARN(arn, ptTestRegion, ptTestAccount)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// TestResolveProvisionedModelID_AcceptsRawIDOrARN covers the shape every PT
// op's ProvisionedModelId field allows: a bare id or a full ARN.
func TestResolveProvisionedModelID_AcceptsRawIDOrARN(t *testing.T) {
	id, err := resolveProvisionedModelID(ptTestID, ptTestRegion, ptTestAccount)
	require.NoError(t, err)
	assert.Equal(t, ptTestID, id)

	arn := FormatProvisionedModelARN(ptTestRegion, ptTestAccount, ptTestID)
	id, err = resolveProvisionedModelID(arn, ptTestRegion, ptTestAccount)
	require.NoError(t, err)
	assert.Equal(t, ptTestID, id)

	foreignARN := FormatProvisionedModelARN(ptTestRegion, ptOtherAccount, ptTestID)
	_, err = resolveProvisionedModelID(foreignARN, ptTestRegion, ptTestAccount)
	require.Error(t, err)
}
