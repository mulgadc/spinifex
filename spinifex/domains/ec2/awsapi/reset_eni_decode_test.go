package awsapi

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AWS answers a present, empty SourceDestCheck and an absent one differently,
// so the decoder must keep them apart.
func TestResetNetworkInterfaceAttribute_DecodesEmptySourceDestCheck(t *testing.T) {
	decode := func(body string) *ec2.ResetNetworkInterfaceAttributeInput {
		args, err := query.ParseArgs(body)
		require.NoError(t, err)
		input, err := ec2Actions["ResetNetworkInterfaceAttribute"].parse(args)
		require.NoError(t, err)
		return input.(*ec2.ResetNetworkInterfaceAttributeInput)
	}

	present := decode("Action=ResetNetworkInterfaceAttribute&NetworkInterfaceId=eni-1&SourceDestCheck=")
	require.NotNil(t, present.SourceDestCheck)
	assert.Empty(t, *present.SourceDestCheck)

	assert.Nil(t, decode("Action=ResetNetworkInterfaceAttribute&NetworkInterfaceId=eni-1").SourceDestCheck)
}
