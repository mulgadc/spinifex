package handlers_ec2_igw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_IGWDeleteIdempotentOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 (idempotent delete): deleting an absent internet gateway is
// success, not NotFound, so tofu destroy retries converge.
func TestRLC1_IGWDeleteIdempotentOnAbsent(t *testing.T) {
	svc, _ := setupTestIGWService(t)

	out, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String("igw-absent00000000"),
	}, testAccountID)

	require.NoErrorf(t, err, "DeleteInternetGateway on an absent IGW must return success, not NotFound (RLC rule #1 idempotent delete): return an empty output on nats.ErrKeyNotFound")
	assert.NotNil(t, out, "DeleteInternetGateway must return a non-nil output on absent (RLC rule #1)")
}
