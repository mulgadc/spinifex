package handlers_ec2_routetable

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_RouteTableDeleteNotFoundOnAbsent enforces the Common Resource
// Lifecycle Contract rule #1 (AWS-faithful delete, per-service): the EC2
// DeleteRouteTable API returns InvalidRouteTableID.NotFound for an absent route
// table, not success. Idempotent convergence belongs to destroy orchestration,
// which tolerates NotFound via awserrors.IsNotFound; the public API stays AWS compatible.
func TestRLC1_RouteTableDeleteNotFoundOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.DeleteRouteTable(context.Background(), &ec2.DeleteRouteTableInput{
		RouteTableId: aws.String("rtb-absent00000000"),
	}, testAccountID)

	require.Errorf(t, err, "DeleteRouteTable on an absent route table must return %s, not success (RLC rule #1 AWS-faithful delete): destroy orchestration tolerates NotFound, the API must not", awserrors.ErrorInvalidRouteTableIDNotFound)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidRouteTableIDNotFound, "DeleteRouteTable on an absent route table must return the canonical InvalidRouteTableID.NotFound (RLC rule #1)")
}
