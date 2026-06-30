package gateway_ec2_spotinstance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

// The Describe/Cancel gateway functions are thin pass-throughs to the NATS spot
// service, so without a connection the NATSRequest call surfaces an error rather
// than panicking.

func TestDescribeSpotInstanceRequests_NilNATS(t *testing.T) {
	_, err := DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{}, nil, testAccountID)
	assert.Error(t, err)
}

func TestDescribeSpotInstanceRequests_WithIDsNilNATS(t *testing.T) {
	_, err := DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-12345678")},
	}, nil, testAccountID)
	assert.Error(t, err)
}

func TestCancelSpotInstanceRequests_NilNATS(t *testing.T) {
	_, err := CancelSpotInstanceRequests(&ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-12345678")},
	}, nil, testAccountID)
	assert.Error(t, err)
}
