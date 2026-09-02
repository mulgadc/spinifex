package handlers_ec2_eip

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unassociated EIP has no dnat_and_snat, so the record move needs no vpcd.
func TestRebindPublicIP_MovesUnassociatedRecord(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID, oldIP := *out.AllocationId, *out.PublicIp

	require.NoError(t, svc.RebindPublicIP(t.Context(), allocID, oldIP, "198.51.100.99"))

	got, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, got.Addresses, 1)
	assert.Equal(t, "198.51.100.99", *got.Addresses[0].PublicIp,
		"DescribeAddresses must not keep reporting an address vpcd released")
}

// Duplicate delivery must not fail: the revision check would reject the second
// write even though the record already holds the target address.
func TestRebindPublicIP_IsIdempotent(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID, oldIP := *out.AllocationId, *out.PublicIp

	require.NoError(t, svc.RebindPublicIP(t.Context(), allocID, oldIP, "198.51.100.99"))
	require.NoError(t, svc.RebindPublicIP(t.Context(), allocID, oldIP, "198.51.100.99"))
}

// Moving a record that names neither address would overwrite an allocation this
// lease does not own.
func TestRebindPublicIP_RejectsMismatchedOldIP(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	err = svc.RebindPublicIP(t.Context(), *out.AllocationId, "203.0.113.7", "198.51.100.99")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the superseded 203.0.113.7")
}

// A lease that outlived its allocation must report, not silently pass.
func TestRebindPublicIP_UnknownAllocationErrors(t *testing.T) {
	svc, _, _ := setupTestEIP(t)

	err := svc.RebindPublicIP(t.Context(), "eipalloc-missing", "198.51.100.11", "198.51.100.99")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no EIP record for allocation eipalloc-missing")
}

func TestRebindPublicIP_RejectsEmptyArgs(t *testing.T) {
	svc, _, _ := setupTestEIP(t)

	assert.Error(t, svc.RebindPublicIP(t.Context(), "", "198.51.100.11", "198.51.100.99"))
	assert.Error(t, svc.RebindPublicIP(t.Context(), "eipalloc-1", "198.51.100.11", ""))
}
