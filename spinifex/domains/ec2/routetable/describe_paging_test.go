package routetable

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeRouteTables_PagesThroughEveryTableOnce(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	want := map[string]bool{}
	for range 7 {
		want[createTestRtb(t, svc)] = true
	}

	got := map[string]bool{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
			MaxResults: aws.Int64(5), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.RouteTables), 5)
		for _, r := range out.RouteTables {
			assert.False(t, got[*r.RouteTableId], "route table %s returned twice", *r.RouteTableId)
			got[*r.RouteTableId] = true
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

func TestDescribeRouteTables_RejectsATokenItDidNotIssue(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{NextToken: aws.String("garbage")}, testAccountID)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}
