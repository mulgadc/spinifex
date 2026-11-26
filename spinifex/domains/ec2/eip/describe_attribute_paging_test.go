package eip

import (
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The page is cut after the AllocationIds selection: an unrequested address
// that sorts inside the first page does not shorten it.
func TestEIP_DescribeAddressesAttribute_PageWalkFollowsSelection(t *testing.T) {
	t.Parallel()
	svc, _, _ := setupTestEIP(t)
	var ids []string
	for range 6 {
		id, _ := allocateEIP(t, svc)
		ids = append(ids, id)
	}
	slices.Sort(ids)
	want := slices.Delete(slices.Clone(ids), 1, 2)

	var seen []string
	var token *string
	for range 10 {
		out, err := svc.DescribeAddressesAttribute(t.Context(), &ec2.DescribeAddressesAttributeInput{
			AllocationIds: aws.StringSlice(append(want, want[0])), MaxResults: aws.Int64(2), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		for _, a := range out.Addresses {
			seen = append(seen, *a.AllocationId)
		}
		if out.NextToken == nil {
			break
		}
		require.Len(t, out.Addresses, 2, "a page before the last is full")
		token = out.NextToken
	}
	require.NotNil(t, token, "the walk took more than one page")
	assert.Equal(t, want, seen)

	_, err := svc.DescribeAddressesAttribute(t.Context(), &ec2.DescribeAddressesAttributeInput{NextToken: aws.String("garbage")}, testAccountID)
	testutil.RequireAWSError(t, err, awserrors.ErrorInvalidParameterValue,
		"Value ( garbage ) for parameter NextToken is invalid. The token is invalid.")
}
