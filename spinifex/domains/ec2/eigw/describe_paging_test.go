package eigw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeEgressOnlyInternetGateways_PagesThroughEveryGatewayOnce(t *testing.T) {
	t.Parallel()
	svc := setupTestEIGWService(t)
	want := map[string]bool{}
	for range 7 {
		want[createTestEIGW(t, svc)] = true
	}

	got := map[string]bool{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeEgressOnlyInternetGateways(t.Context(), &ec2.DescribeEgressOnlyInternetGatewaysInput{
			MaxResults: aws.Int64(5), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.EgressOnlyInternetGateways), 5)
		for _, g := range out.EgressOnlyInternetGateways {
			id := *g.EgressOnlyInternetGatewayId
			assert.False(t, got[id], "gateway %s returned twice", id)
			got[id] = true
		}
		pages++
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 2, pages)
}

func TestDescribeEgressOnlyInternetGateways_RejectsATokenItDidNotIssue(t *testing.T) {
	t.Parallel()
	svc := setupTestEIGWService(t)
	_, err := svc.DescribeEgressOnlyInternetGateways(t.Context(), &ec2.DescribeEgressOnlyInternetGatewaysInput{
		NextToken: aws.String("garbage"),
	}, testAccountID)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}
