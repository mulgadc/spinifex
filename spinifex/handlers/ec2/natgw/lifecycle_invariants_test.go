package handlers_ec2_natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_NatGatewayDeleteIdempotentOnAbsent enforces the Common Resource
// Lifecycle Contract rule #1 (idempotent delete): deleting an absent NAT
// gateway is success, not NotFound, so tofu destroy retries converge.
func TestRLC1_NatGatewayDeleteIdempotentOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String("nat-absent00000000"),
	}, testAccountID)

	require.NoErrorf(t, err, "DeleteNatGateway on an absent NAT gateway must return success, not NotFound (RLC rule #1 idempotent delete): return an empty output on nats.ErrKeyNotFound")
	assert.NotNil(t, out, "DeleteNatGateway must return a non-nil output on absent (RLC rule #1)")
}
