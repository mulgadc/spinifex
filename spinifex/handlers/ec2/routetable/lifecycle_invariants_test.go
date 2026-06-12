package handlers_ec2_routetable

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_RouteTableDeleteIdempotentOnAbsent enforces the Common Resource
// Lifecycle Contract rule #1 (idempotent delete): deleting an absent route
// table is success, not NotFound, so tofu destroy retries converge.
func TestRLC1_RouteTableDeleteIdempotentOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String("rtb-absent00000000"),
	}, testAccountID)

	require.NoErrorf(t, err, "DeleteRouteTable on an absent route table must return success, not NotFound (RLC rule #1 idempotent delete): pre-check by key and return an empty output on nats.ErrKeyNotFound")
	assert.NotNil(t, out, "DeleteRouteTable must return a non-nil output on absent (RLC rule #1)")
}
