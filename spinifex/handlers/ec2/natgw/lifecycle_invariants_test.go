package handlers_ec2_natgw

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

// TestRLC3_NatGatewayBlocksWhileRouted enforces the Common Resource Lifecycle
// Contract rule #3 (live-only dependency guards): a NAT gateway a live route
// table still forwards to must not delete (DependencyViolation), so tofu
// destroy ordering is driven; once nothing routes to it, delete succeeds.
func TestRLC3_NatGatewayBlocksWhileRouted(t *testing.T) {
	t.Run("blocked while a route forwards to it", func(t *testing.T) {
		svc, js := setupTestServiceJS(t)
		natgwID := createTestNatGateway(t, svc)

		testutil.SeedKV(t, js, kvBucketRouteTables, map[string][]byte{
			utils.AccountKey(testAccountID, "rtb-routed"): fmt.Appendf(nil,
				`{"route_table_id":"rtb-routed","vpc_id":"vpc-test1","routes":[{"destination_cidr_block":"0.0.0.0/0","nat_gateway_id":%q}]}`, natgwID),
		})

		_, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(natgwID)}, testAccountID)
		assert.ErrorContainsf(t, err, awserrors.ErrorDependencyViolation,
			"ADR-0004 §2: DeleteNatGateway must return DependencyViolation while a live route table forwards to it (rule #3)")
	})

	t.Run("allowed once nothing routes to it", func(t *testing.T) {
		svc := setupTestService(t)
		natgwID := createTestNatGateway(t, svc)

		_, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(natgwID)}, testAccountID)
		require.NoErrorf(t, err,
			"ADR-0004 §2: with no route forwarding to it, DeleteNatGateway must succeed")
	})
}

// TestRLC3_NatGatewayDeleteDisassociatesEIPKeepingAllocated enforces ADR-0004
// §2: deleting a NAT gateway releases its EIP association but keeps the
// allocation (AWS parity), never orphaning the EIP as permanently associated.
func TestRLC3_NatGatewayDeleteDisassociatesEIPKeepingAllocated(t *testing.T) {
	svc := setupTestService(t)
	natgwID := createTestNatGateway(t, svc)

	// Creating the NAT GW must have marked its EIP associated.
	assoc := readEIP(t, svc, "eipalloc-test1")
	require.Equal(t, "associated", assoc.State, "CreateNatGateway must mark the EIP associated so a second NAT GW cannot reuse it")
	require.NotEmpty(t, assoc.AssociationId)

	_, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(natgwID)}, testAccountID)
	require.NoError(t, err)

	eip := readEIP(t, svc, "eipalloc-test1")
	assert.Equal(t, "allocated", eip.State, "ADR-0004 §2: the EIP must stay allocated after NAT GW delete (AWS parity)")
	assert.Empty(t, eip.AssociationId, "ADR-0004 §2: the EIP must be disassociated, not left orphaned as associated")
}

func createTestNatGateway(t *testing.T, svc *NatGatewayServiceImpl) string {
	t.Helper()
	out, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	return *out.NatGateway.NatGatewayId
}

func readEIP(t *testing.T, svc *NatGatewayServiceImpl, allocID string) handlers_ec2_eip.EIPRecord {
	t.Helper()
	entry, err := svc.eipKV.Get(utils.AccountKey(testAccountID, allocID))
	require.NoError(t, err)
	var eip handlers_ec2_eip.EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &eip))
	return eip
}
