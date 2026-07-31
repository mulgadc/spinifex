package handlers_ec2_eip

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocationExists(t *testing.T) {
	svc, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	exists, err := svc.AllocationExists(t.Context(), *out.AllocationId)
	require.NoError(t, err)
	assert.True(t, exists)
}

// The lease store keys on the client-id alone, so the lookup has to work without
// the owning account.
func TestAllocationExistsIgnoresAccount(t *testing.T) {
	svc, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, "210987654321")
	require.NoError(t, err)

	exists, err := svc.AllocationExists(t.Context(), *out.AllocationId)
	require.NoError(t, err)
	assert.True(t, exists)
}

// A released allocation is what the reaper acts on, so this answer costs an
// address if it is wrong.
func TestAllocationExistsFalseAfterRelease(t *testing.T) {
	svc, _ := setupTestEIP(t)
	out, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	_, err = svc.ReleaseAddress(t.Context(), &ec2.ReleaseAddressInput{AllocationId: out.AllocationId}, testAccountID)
	require.NoError(t, err)

	exists, err := svc.AllocationExists(t.Context(), *out.AllocationId)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestAllocationExistsUnknownID(t *testing.T) {
	svc, _ := setupTestEIP(t)

	exists, err := svc.AllocationExists(t.Context(), "eipalloc-missing")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestAllocationExistsRejectsEmptyID(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.AllocationExists(t.Context(), "")
	require.Error(t, err)
}
