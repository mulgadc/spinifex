//test:in-package — reuses the in-package VPC service fixtures (setupTestVPCService, createTestVPC)

package handlers_ec2_vpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeSubnets_PagesThroughEverySubnetOnce(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	want := map[string]bool{}
	for i := range 7 {
		want[createTestSubnet(t, svc, vpcID, fmt.Sprintf("10.0.%d.0/24", i))] = true
	}

	got := map[string]bool{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
			MaxResults: aws.Int64(5),
			NextToken:  token,
		}, testAccountID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.Subnets), 5)
		for _, s := range out.Subnets {
			assert.False(t, got[*s.SubnetId], "subnet %s returned twice", *s.SubnetId)
			got[*s.SubnetId] = true
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

func TestDescribeSecurityGroups_PagesAfterFiltering(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	for i := range 6 {
		createTestSG(t, svc, vpcID, fmt.Sprintf("paged-%d", i))
	}
	createTestSG(t, svc, createTestVPC(t, svc, "10.1.0.0/16"), "other-vpc")

	filter := []*ec2.Filter{{Name: aws.String("group-name"), Values: []*string{aws.String("paged-*")}}}
	first, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		Filters: filter, MaxResults: aws.Int64(5),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, first.SecurityGroups, 5)
	require.NotNil(t, first.NextToken)

	second, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		Filters: filter, MaxResults: aws.Int64(5), NextToken: first.NextToken,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, second.SecurityGroups, 1)
	assert.Nil(t, second.NextToken)
}

func TestDescribeVpcs_ValidatesPagingButReturnsEverything(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	for i := range 6 {
		createTestVPC(t, svc, fmt.Sprintf("10.%d.0.0/16", i))
	}

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{MaxResults: aws.Int64(5)}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.Vpcs), 6)
	assert.Nil(t, out.NextToken)
}

// TestNetworkDescribes_PagingErrors pins each operation to the code and message
// AWS returns, which differ in wording and casing between operations.
func TestNetworkDescribes_PagingErrors(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	ctx := context.Background()
	ids := []*string{aws.String("x-0123456789abcdef0")}

	cases := []struct {
		name    string
		call    func() error
		code    string
		message string
	}{
		{"DescribeVpcs too large", func() error {
			_, err := svc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{MaxResults: aws.Int64(2000)}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterValue, "Value ( 2000 ) for parameter MaxResults is invalid. Expecting a value smaller than or equal to 1000."},
		{"DescribeVpcs ids", func() error {
			_, err := svc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{MaxResults: aws.Int64(5), VpcIds: ids}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterCombination, "The parameter VpcIds cannot be used with the parameter MaxResults"},
		{"DescribeSubnets too small", func() error {
			_, err := svc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{MaxResults: aws.Int64(4)}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterValue, "Value ( 4 ) for parameter MaxResults is invalid. Expecting a value greater than or equal to 5."},
		{"DescribeSubnets bad token", func() error {
			_, err := svc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{NextToken: aws.String("garbage")}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterValue, "Value ( garbage ) for parameter NextToken is invalid. The token is invalid."},
		{"DescribeSecurityGroups too large", func() error {
			_, err := svc.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{MaxResults: aws.Int64(1001)}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterValue, "Value ( 1001 ) for parameter maxResults is invalid. Expecting a value smaller than 1000."},
		{"DescribeSecurityGroups ids", func() error {
			_, err := svc.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{MaxResults: aws.Int64(5), GroupIds: ids}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterCombination, "The parameter securityGroupIdSet cannot be used with the parameter maxResults"},
		{"DescribeSecurityGroupRules bad token", func() error {
			_, err := svc.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{NextToken: aws.String("garbage")}, testAccountID)
			return err
		}, awserrors.ErrorInvalidPaginationToken, "Next token 'garbage' is invalid"},
		{"DescribeSecurityGroupRules ids", func() error {
			_, err := svc.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{MaxResults: aws.Int64(5), SecurityGroupRuleIds: ids}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterCombination, "The parameter 'securityGroupRuleIds' may not be used in combination with 'maxResults'."},
		{"DescribeNetworkInterfaces ids", func() error {
			_, err := svc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{MaxResults: aws.Int64(5), NetworkInterfaceIds: ids}, testAccountID)
			return err
		}, awserrors.ErrorInvalidParameterCombination, "The parameter NetworkInterfaceIds cannot be used with the parameter MaxResults"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, message, ok := awserrors.ResolveErrorDetail(tc.call())
			require.True(t, ok)
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.message, message)
		})
	}
}
