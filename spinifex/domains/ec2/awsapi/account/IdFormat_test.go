package account

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const idFormatAccount = "123456789012"

// The resource order and ap-southeast-2 deadlines AWS returned for
// DescribeIdFormat, as captured from the AWS CLI.
var awsIdFormatOrder = []string{
	"reservation", "instance", "volume", "snapshot",
	"network-interface-attachment", "network-interface", "elastic-ip-allocation", "elastic-ip-association",
	"vpc", "subnet", "route-table", "route-table-association", "network-acl", "network-acl-association",
	"dhcp-options", "internet-gateway", "vpc-cidr-block-association", "vpc-ipv6-cidr-block-association",
	"subnet-ipv6-cidr-block-association", "vpc-peering-connection", "security-group", "flow-log",
	"vpc-endpoint", "conversion-task", "export-task", "import-task", "image", "bundle",
	"customer-gateway", "vpn-connection", "vpn-gateway",
}

func assertFullList(t *testing.T, statuses []*ec2.IdFormat, hour string) {
	t.Helper()
	require.Len(t, statuses, 31)
	for i, s := range statuses {
		assert.Equal(t, awsIdFormatOrder[i], aws.StringValue(s.Resource))
		assert.True(t, aws.BoolValue(s.UseLongIds))
		day := "2018-08-09"
		if i < 4 {
			day = "2016-12-07"
		}
		assert.Equal(t, day+"T"+hour+":00:00Z", aws.TimeValue(s.Deadline).UTC().Format(time.RFC3339), aws.StringValue(s.Resource))
	}
}

func assertInvalidResourceType(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidResourceTypeUnknown, code)
	assert.Contains(t, err.Error(), "Invalid resource type")
}

func TestDescribeIdFormat_FullList(t *testing.T) {
	out, err := DescribeIdFormat(&ec2.DescribeIdFormatInput{})
	require.NoError(t, err)
	assertFullList(t, out.Statuses, "12")
}

func TestDescribeIdFormat_ResourceFilter(t *testing.T) {
	out, err := DescribeIdFormat(&ec2.DescribeIdFormatInput{Resource: aws.String("vpc")})
	require.NoError(t, err)
	require.Len(t, out.Statuses, 1)
	assert.Equal(t, "vpc", aws.StringValue(out.Statuses[0].Resource))

	_, err = DescribeIdFormat(&ec2.DescribeIdFormatInput{Resource: aws.String("bogus")})
	assertInvalidResourceType(t, err)
}

func TestDescribeIdentityIdFormat_PrincipalArn(t *testing.T) {
	for _, principal := range []string{
		"arn:aws:iam::" + idFormatAccount + ":root",
		"arn:aws:iam::" + idFormatAccount + ":user/nobody",
	} {
		out, err := DescribeIdentityIdFormat(&ec2.DescribeIdentityIdFormatInput{PrincipalArn: aws.String(principal)}, idFormatAccount)
		require.NoError(t, err, principal)
		assertFullList(t, out.Statuses, "12")
	}
	for _, principal := range []string{"not-an-arn", "arn:aws:iam::999999999999:root"} {
		_, err := DescribeIdentityIdFormat(&ec2.DescribeIdentityIdFormatInput{PrincipalArn: aws.String(principal)}, idFormatAccount)
		require.Error(t, err, principal)
		code, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, awserrors.ErrorInvalidTargetArnUnknown, code)
		assert.Contains(t, err.Error(), "Invalid TargetArn: "+principal)
	}
}

func TestDescribeIdentityIdFormat_ResourceFilter(t *testing.T) {
	in := &ec2.DescribeIdentityIdFormatInput{PrincipalArn: aws.String("arn:aws:iam::" + idFormatAccount + ":root"), Resource: aws.String("bogus")}
	_, err := DescribeIdentityIdFormat(in, idFormatAccount)
	assertInvalidResourceType(t, err)
}

func TestDescribeAggregateIdFormat(t *testing.T) {
	out, err := DescribeAggregateIdFormat(&ec2.DescribeAggregateIdFormatInput{})
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(out.UseLongIdsAggregated))
	assertFullList(t, out.Statuses, "02")
}

func TestDescribePrincipalIdFormat_RootPrincipal(t *testing.T) {
	out, err := DescribePrincipalIdFormat(&ec2.DescribePrincipalIdFormatInput{MaxResults: aws.Int64(4)}, idFormatAccount)
	require.NoError(t, err)
	require.Len(t, out.Principals, 1)
	assert.Equal(t, "arn:aws:iam::"+idFormatAccount+":root", aws.StringValue(out.Principals[0].Arn))
	assertFullList(t, out.Principals[0].Statuses, "12")
	assert.Nil(t, out.NextToken)
}

func TestDescribePrincipalIdFormat_ResourcesKeepFixedOrder(t *testing.T) {
	out, err := DescribePrincipalIdFormat(&ec2.DescribePrincipalIdFormatInput{
		Resources: aws.StringSlice([]string{"vpn-gateway", "instance"}),
	}, idFormatAccount)
	require.NoError(t, err)
	statuses := out.Principals[0].Statuses
	require.Len(t, statuses, 2)
	assert.Equal(t, "instance", aws.StringValue(statuses[0].Resource))
	assert.Equal(t, "vpn-gateway", aws.StringValue(statuses[1].Resource))

	_, err = DescribePrincipalIdFormat(&ec2.DescribePrincipalIdFormatInput{Resources: aws.StringSlice([]string{"instance", "bogus"})}, idFormatAccount)
	assertInvalidResourceType(t, err)
}

func TestDescribePrincipalIdFormat_ForeignTokenRefused(t *testing.T) {
	_, err := DescribePrincipalIdFormat(&ec2.DescribePrincipalIdFormatInput{NextToken: aws.String("1")}, idFormatAccount)
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Equal(t, "Value ( 1 ) for parameter NextToken is invalid. The token is invalid.", msg)
}
