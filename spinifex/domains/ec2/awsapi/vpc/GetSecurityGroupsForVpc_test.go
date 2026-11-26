package vpc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A bad MaxResults is refused with AWS's text before the request crosses NATS.
func TestGetSecurityGroupsForVpc_MaxResultsOutOfRange(t *testing.T) {
	for n, msg := range map[int64]string{
		4:    "Value ( 4 ) for parameter maxResults is invalid. Expecting a value greater than 5.",
		1001: "Value ( 1001 ) for parameter maxResults is invalid. Expecting a value smaller than 1000.",
	} {
		_, err := GetSecurityGroupsForVpc(context.Background(),
			&ec2.GetSecurityGroupsForVpcInput{VpcId: aws.String("vpc-1"), MaxResults: aws.Int64(n)}, nil, testAccountID)
		testutil.RequireAWSError(t, err, awserrors.ErrorInvalidParameterValue, msg)
	}
	// In range, the call passes the gateway and fails at the nil connection.
	for _, n := range []int64{5, 1000} {
		_, err := GetSecurityGroupsForVpc(context.Background(),
			&ec2.GetSecurityGroupsForVpcInput{VpcId: aws.String("vpc-1"), MaxResults: aws.Int64(n)}, nil, testAccountID)
		require.Error(t, err)
		code, _, _ := awserrors.ResolveErrorDetail(err)
		assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, code, "MaxResults %d", n)
	}
}
