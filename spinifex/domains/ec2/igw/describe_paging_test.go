package igw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeInternetGateways_PagesThroughEveryGatewayOnce(t *testing.T) {
	t.Parallel()
	svc, _ := setupTestIGWService(t)
	want := map[string]bool{}
	for range 7 {
		want[createTestIGW(t, svc)] = true
	}

	got := map[string]bool{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{
			MaxResults: aws.Int64(5), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.InternetGateways), 5)
		for _, g := range out.InternetGateways {
			assert.False(t, got[*g.InternetGatewayId], "gateway %s returned twice", *g.InternetGatewayId)
			got[*g.InternetGatewayId] = true
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
