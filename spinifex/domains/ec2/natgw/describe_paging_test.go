package natgw

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func putNatGatewayRecord(t *testing.T, svc *NatGatewayServiceImpl, id, state string) {
	t.Helper()
	data, err := json.Marshal(NatGatewayRecord{NatGatewayId: id, VpcId: "vpc-test1", SubnetId: "subnet-pub1", State: state})
	require.NoError(t, err)
	_, err = svc.natgwKV.Put(t.Context(), kvstore.AccountKey(testAccountID, id), data)
	require.NoError(t, err)
}

func TestDescribeNatGateways_PagesAfterFilteringAndSeesEachGatewayOnce(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	want := map[string]bool{}
	for i := range 7 {
		id := fmt.Sprintf("nat-page%d", i)
		putNatGatewayRecord(t, svc, id, "available")
		want[id] = true
	}
	putNatGatewayRecord(t, svc, "nat-failed", "failed")
	filter := []*ec2.Filter{{Name: aws.String("state"), Values: []*string{aws.String("available")}}}

	got := map[string]bool{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeNatGateways(t.Context(), &ec2.DescribeNatGatewaysInput{
			Filter: filter, MaxResults: aws.Int64(5), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.NatGateways), 5)
		for _, g := range out.NatGateways {
			assert.False(t, got[*g.NatGatewayId], "gateway %s returned twice", *g.NatGatewayId)
			got[*g.NatGatewayId] = true
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

func TestDescribeNatGateways_RejectsATokenItDidNotIssue(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.DescribeNatGateways(t.Context(), &ec2.DescribeNatGatewaysInput{NextToken: aws.String("garbage")}, testAccountID)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}
