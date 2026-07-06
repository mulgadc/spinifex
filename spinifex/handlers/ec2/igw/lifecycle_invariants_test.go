package handlers_ec2_igw

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_IGWDeleteNotFoundOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 (AWS-faithful delete, per-service): the EC2 DeleteInternetGateway
// API returns InvalidInternetGatewayID.NotFound for an absent IGW, not success.
// Idempotent convergence belongs to destroy orchestration, which tolerates
// NotFound via awserrors.IsNotFound; the public API stays AWS compatible.
func TestRLC1_IGWDeleteNotFoundOnAbsent(t *testing.T) {
	svc, _ := setupTestIGWService(t)

	_, err := svc.DeleteInternetGateway(context.Background(), &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String("igw-absent00000000"),
	}, testAccountID)

	require.Errorf(t, err, "DeleteInternetGateway on an absent IGW must return %s, not success (RLC rule #1 AWS-faithful delete): destroy orchestration tolerates NotFound, the API must not", awserrors.ErrorInvalidInternetGatewayIDNotFound)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidInternetGatewayIDNotFound, "DeleteInternetGateway on an absent IGW must return the canonical InvalidInternetGatewayID.NotFound (RLC rule #1)")
}
