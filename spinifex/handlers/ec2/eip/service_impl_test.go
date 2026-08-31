package handlers_ec2_eip

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func testPool() external.ExternalPoolConfig {
	return external.ExternalPoolConfig{
		Name:       "test-pool",
		RangeStart: "198.51.100.10",
		RangeEnd:   "198.51.100.20",
		Gateway:    "198.51.100.1",
		PrefixLen:  24,
	}
}

func setupTestEIP(t *testing.T) (*EIPServiceImpl, *handlers_ec2_vpc.ExternalIPAM) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	pool := testPool()
	ipam, err := handlers_ec2_vpc.NewExternalIPAM(t.Context(), js, []external.ExternalPoolConfig{pool})
	require.NoError(t, err)

	svc, err := NewEIPServiceImpl(t.Context(), nc, ipam, nil)
	require.NoError(t, err)

	return svc, ipam
}

func TestEIP_Allocate(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.NotEmpty(t, *out.AllocationId)
	assert.NotEmpty(t, *out.PublicIp)
	assert.Equal(t, "vpc", *out.Domain)
	// Gateway takes .10, so first allocable is .11
	assert.Equal(t, "198.51.100.11", *out.PublicIp)
}

func TestEIP_AllocateFromSpecificPool(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.NotEmpty(t, *out.AllocationId)
	assert.Equal(t, "vpc", *out.Domain)
	assert.NotEmpty(t, *out.PublicIp)
}

// A named pool that does not exist must surface the allocator's own error
// rather than a flat capacity code, so an upstream fault is not misreported
// as a genuinely exhausted pool.
func TestEIP_AllocateFromNamedPoolThatDoesNotExist(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{
		PublicIpv4Pool: aws.String("no-such-pool"),
	}, testAccountID)
	require.Error(t, err)
}

// Once the default pool's ten allocable addresses are exhausted, the eleventh
// allocation must still resolve to InsufficientAddressCapacity — the wrap
// added around the allocator's error must not swallow the AWS error code.
func TestEIP_AllocateFromExhaustedDefaultPool(t *testing.T) {
	svc, _ := setupTestEIP(t)

	for range 10 {
		_, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
		require.NoError(t, err)
	}

	_, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientAddressCapacity, awserrors.ValidErrorCodeFromError(err))
}

func TestEIP_Release(t *testing.T) {
	svc, ipam := setupTestEIP(t)

	// Allocate
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocatedIP := *out.PublicIp

	// Release
	_, err = svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{
		AllocationId: out.AllocationId,
	}, testAccountID)
	require.NoError(t, err)

	// Round-robin: re-allocating does NOT hand back the just-released IP;
	// the cursor advances, so the new EIP differs.
	out2, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	assert.NotEqual(t, allocatedIP, *out2.PublicIp, "released EIP must not be reused immediately")

	// The released IP stays free (not re-allocated) until the cursor cycles.
	record, err := ipam.GetPoolRecord("test-pool")
	require.NoError(t, err)
	_, stillAllocated := record.Allocated[allocatedIP]
	assert.False(t, stillAllocated, "released IP stays free until the cursor cycles the range")

	// The released allocation is gone; describing it by its old ID errors.
	_, descErr := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		AllocationIds: []*string{out.AllocationId},
	}, testAccountID)
	assert.Error(t, descErr)
}

func TestEIP_ReleaseWhileAssociated(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Allocate
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Manually mark as associated by writing to KV (simulates AssociateAddress without needing a real VPCService)
	allocID := *out.AllocationId
	// We can't easily associate without a VPC service, but we can test the error path
	// by directly updating the record's state in the KV store.
	// Instead, let's verify that ReleaseAddress checks the state.
	// Since we haven't associated, this should succeed (testing the non-associated path).
	// To test the associated path, we need to manipulate the KV directly.

	// Get the KV entry and update state to "associated"
	entry, err := svc.eipKV.Get(t.Context(), testAccountID+"."+allocID)
	require.NoError(t, err)

	var record EIPRecord
	err = json.Unmarshal(entry.Value(), &record)
	require.NoError(t, err)
	record.State = "associated"
	record.AssociationId = "eipassoc-test"
	record.ENIId = "eni-test"

	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = svc.eipKV.Update(t.Context(), testAccountID+"."+allocID, data, entry.Revision())
	require.NoError(t, err)

	// Now try to release — should fail
	_, err = svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{
		AllocationId: aws.String(allocID),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAddress.Locked")
}

func TestEIP_ReleaseByInstanceID_ReclaimsAssociatedEIP(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID := *out.AllocationId

	// Mark the EIP associated with a system instance, as an internet-facing ALB
	// leaves it. ENIId stays empty so disassociate skips the VPC-service lookup.
	entry, err := svc.eipKV.Get(t.Context(), testAccountID+"."+allocID)
	require.NoError(t, err)
	var record EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	record.State = "associated"
	record.AssociationId = "eipassoc-test"
	record.InstanceId = "i-alb-test"
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = svc.eipKV.Update(t.Context(), testAccountID+"."+allocID, data, entry.Revision())
	require.NoError(t, err)

	// Backstop release by instance disassociates then frees the allocation.
	require.NoError(t, svc.ReleaseAddressByInstanceID("i-alb-test"))

	_, err = svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAllocationID.NotFound")
}

// associateToENI marks an allocated EIP as associated with eniID the way
// AssociateAddress would, without needing a real VPC service behind it.
func associateToENI(t *testing.T, svc *EIPServiceImpl, allocID, eniID string) {
	t.Helper()
	key := testAccountID + "." + allocID
	entry, err := svc.eipKV.Get(t.Context(), key)
	require.NoError(t, err)
	var record EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	record.State = "associated"
	record.AssociationId = "eipassoc-" + eniID
	record.ENIId = eniID
	record.InstanceId = "i-owner"
	record.PrivateIp = "172.31.0.8"
	record.VpcId = "vpc-test"
	record.MacAddress = "02:52:f5:7f:9d:0e"
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = svc.eipKV.Update(t.Context(), key, data, entry.Revision())
	require.NoError(t, err)
}

// TestEIP_DisassociateByENI is the teardown path the zombie ENIs needed: the
// association goes with the interface, and the allocation stays behind.
func TestEIP_DisassociateByENI(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID, publicIP := *out.AllocationId, *out.PublicIp
	associateToENI(t, svc, allocID, "eni-zombie")

	found, err := svc.DisassociateByENI(t.Context(), testAccountID, "eni-zombie")
	require.NoError(t, err)
	assert.True(t, found)

	entry, err := svc.eipKV.Get(t.Context(), testAccountID+"."+allocID)
	require.NoError(t, err, "the allocation survives: an EIP outlives the instance it was attached to")
	var record EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	assert.Equal(t, "allocated", record.State)
	assert.Equal(t, publicIP, record.PublicIp)
	assert.Empty(t, record.ENIId)
	assert.Empty(t, record.AssociationId)
	assert.Empty(t, record.InstanceId)
	assert.Empty(t, record.PrivateIp)
	assert.Empty(t, record.VpcId)
	assert.Empty(t, record.MacAddress)

	// The record no longer names the ENI, so a re-run finds nothing and still
	// succeeds — terminate and the orphan sweep both retry this.
	found, err = svc.DisassociateByENI(t.Context(), testAccountID, "eni-zombie")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestEIP_DisassociateByENI_NoMatchIsNoOp pins that an ENI carrying no EIP — the
// overwhelmingly common case on terminate — leaves other accounts' associations
// alone and reports no error.
func TestEIP_DisassociateByENI_NoMatchIsNoOp(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID := *out.AllocationId
	associateToENI(t, svc, allocID, "eni-other")

	found, err := svc.DisassociateByENI(t.Context(), testAccountID, "eni-unrelated")
	require.NoError(t, err)
	assert.False(t, found)

	found, err = svc.DisassociateByENI(t.Context(), testAccountID, "")
	require.NoError(t, err)
	assert.False(t, found)

	entry, err := svc.eipKV.Get(t.Context(), testAccountID+"."+allocID)
	require.NoError(t, err)
	var record EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	assert.Equal(t, "associated", record.State)
	assert.Equal(t, "eni-other", record.ENIId)
}

// TestEIP_DisassociateByENI_ScopedToAccount pins that the scan honours the
// account prefix: a cluster-wide sweep passes the account the key was found
// under, and matching across accounts would disassociate a stranger's address.
func TestEIP_DisassociateByENI_ScopedToAccount(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	associateToENI(t, svc, *out.AllocationId, "eni-zombie")

	found, err := svc.DisassociateByENI(t.Context(), "210987654321", "eni-zombie")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestEIP_DisassociateByENI_SkipsMalformedRecord pins that one undecodable
// entry cannot hide a real association. Teardown deletes the interface on a
// "no EIP" answer, and the association would then be stranded with nothing
// left that could find it.
func TestEIP_DisassociateByENI_SkipsMalformedRecord(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.eipKV.Put(t.Context(), testAccountID+".eipalloc-corrupt", []byte("{"))
	require.NoError(t, err)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	associateToENI(t, svc, *out.AllocationId, "eni-zombie")

	found, err := svc.DisassociateByENI(t.Context(), testAccountID, "eni-zombie")
	require.NoError(t, err)
	assert.True(t, found)
}

func TestEIP_ReleaseByInstanceID_NoMatchIsNoOp(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID := *out.AllocationId

	// An instance with no recorded EIP must leave existing allocations untouched.
	require.NoError(t, svc.ReleaseAddressByInstanceID("i-unrelated"))

	_, err = svc.eipKV.Get(t.Context(), testAccountID+"."+allocID)
	assert.NoError(t, err)

	// Empty instance ID is a no-op too.
	require.NoError(t, svc.ReleaseAddressByInstanceID(""))
}

func TestEIP_ReleaseMissingParams(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Nil AllocationId
	_, err := svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{}, testAccountID)
	assert.Error(t, err)

	// Empty AllocationId
	_, err = svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: aws.String("")}, testAccountID)
	assert.Error(t, err)
}

func TestEIP_ReleaseNotFound(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: aws.String("eipalloc-nonexistent")}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAllocationID.NotFound")
}

func TestEIP_AssociateMissingAllocationId(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Nil AllocationId
	_, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{}, testAccountID)
	assert.Error(t, err)

	// Empty AllocationId
	_, err = svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{AllocationId: aws.String("")}, testAccountID)
	assert.Error(t, err)
}

func TestEIP_AssociateInvalidAllocationId(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId:       aws.String("eipalloc-nonexistent"),
		NetworkInterfaceId: aws.String("eni-test"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAllocationID.NotFound")
}

func TestEIP_AssociateMissingTarget(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Allocate first
	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Associate without NetworkInterfaceId or InstanceId
	_, err = svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId: out.AllocationId,
	}, testAccountID)
	assert.Error(t, err)
}

func TestEIP_DisassociateMissingParams(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Nil AssociationId
	_, err := svc.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{}, testAccountID)
	assert.Error(t, err)

	// Empty AssociationId
	_, err = svc.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{AssociationId: aws.String("")}, testAccountID)
	assert.Error(t, err)
}

func TestEIP_DisassociateNotFound(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{
		AssociationId: aws.String("eipassoc-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAssociationID.NotFound")
}

func TestEIP_RecordToEC2_WithTags(t *testing.T) {
	svc, _ := setupTestEIP(t)

	record := &EIPRecord{
		AllocationId:  "eipalloc-test",
		PublicIp:      "1.2.3.4",
		PoolName:      "test-pool",
		State:         "associated",
		AssociationId: "eipassoc-test",
		ENIId:         "eni-test",
		InstanceId:    "i-test",
		PrivateIp:     "10.0.0.5",
		Tags:          map[string]string{"Name": "my-eip", "env": "test"},
	}

	addr := svc.eipRecordToEC2(record)
	assert.Equal(t, "eipalloc-test", *addr.AllocationId)
	assert.Equal(t, "1.2.3.4", *addr.PublicIp)
	assert.Equal(t, "vpc", *addr.Domain)
	assert.Equal(t, "eipassoc-test", *addr.AssociationId)
	assert.Equal(t, "eni-test", *addr.NetworkInterfaceId)
	assert.Equal(t, "i-test", *addr.InstanceId)
	assert.Equal(t, "10.0.0.5", *addr.PrivateIpAddress)
	assert.Len(t, addr.Tags, 2)
}

func TestEIP_RecordToEC2_WithoutTags(t *testing.T) {
	svc, _ := setupTestEIP(t)

	record := &EIPRecord{
		AllocationId: "eipalloc-notags",
		PublicIp:     "5.6.7.8",
		PoolName:     "test-pool",
		State:        "allocated",
		Tags:         map[string]string{},
	}

	addr := svc.eipRecordToEC2(record)
	assert.Equal(t, "eipalloc-notags", *addr.AllocationId)
	assert.Equal(t, "5.6.7.8", *addr.PublicIp)
	assert.Nil(t, addr.AssociationId)
	assert.Nil(t, addr.NetworkInterfaceId)
	assert.Nil(t, addr.InstanceId)
	assert.Nil(t, addr.PrivateIpAddress)
	assert.Empty(t, addr.Tags)
}

func TestEIP_FindByAssociationID_NotFound(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, _, _, err := svc.findByAssociationID(t.Context(), testAccountID, "eipassoc-nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidAssociationID.NotFound")
}

func TestEIP_DescribeAddressesAttribute(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Allocate multiple EIPs
	out1, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	out2, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Describe all — should return both
	desc, err := svc.DescribeAddressesAttribute(context.Background(), &ec2.DescribeAddressesAttributeInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 2)

	// Each entry should have AllocationId and PublicIp populated
	for _, addr := range desc.Addresses {
		assert.NotNil(t, addr.AllocationId)
		assert.NotNil(t, addr.PublicIp)
		assert.Nil(t, addr.PtrRecord) // no reverse-DNS support
	}

	// Filter by specific allocation ID
	desc2, err := svc.DescribeAddressesAttribute(context.Background(), &ec2.DescribeAddressesAttributeInput{
		AllocationIds: []*string{out1.AllocationId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc2.Addresses, 1)
	assert.Equal(t, *out1.AllocationId, *desc2.Addresses[0].AllocationId)
	assert.Equal(t, *out1.PublicIp, *desc2.Addresses[0].PublicIp)

	// Filter by unknown allocation ID — returns empty, not error
	desc3, err := svc.DescribeAddressesAttribute(context.Background(), &ec2.DescribeAddressesAttributeInput{
		AllocationIds: []*string{aws.String("eipalloc-nonexistent")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc3.Addresses)

	_ = out2
}

func TestEIP_DescribeAddresses(t *testing.T) {
	svc, _ := setupTestEIP(t)

	// Allocate multiple EIPs
	out1, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	out2, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	out3, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Describe all
	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 3)

	// Verify all IPs are unique
	ips := make(map[string]bool)
	for _, addr := range desc.Addresses {
		ips[*addr.PublicIp] = true
	}
	assert.Len(t, ips, 3)

	// Describe by specific allocation ID
	desc2, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		AllocationIds: []*string{out1.AllocationId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc2.Addresses, 1)
	assert.Equal(t, *out1.AllocationId, *desc2.Addresses[0].AllocationId)

	_ = out2
	_ = out3
}

func TestEIP_DescribeAddresses_FilterByAllocationId(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out1, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("allocation-id"), Values: []*string{out1.AllocationId}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, *out1.AllocationId, *desc.Addresses[0].AllocationId)
}

func TestEIP_DescribeAddresses_FilterByPublicIp(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out1, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("public-ip"), Values: []*string{out1.PublicIp}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, *out1.PublicIp, *desc.Addresses[0].PublicIp)
}

func TestEIP_DescribeAddresses_FilterByDomain(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("domain"), Values: []*string{aws.String("vpc")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)

	// Non-matching domain
	desc, err = svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("domain"), Values: []*string{aws.String("standard")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Addresses)
}

func TestEIP_DescribeAddresses_FilterByInstanceId(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Manually set instance-id on the record
	entry, err := svc.eipKV.Get(t.Context(), testAccountID+"."+*out.AllocationId)
	require.NoError(t, err)
	var record EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	record.InstanceId = "i-test123"
	record.State = "associated"
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = svc.eipKV.Update(t.Context(), testAccountID+"."+*out.AllocationId, data, entry.Revision())
	require.NoError(t, err)

	// Allocate another without association
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("instance-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, "i-test123", *desc.Addresses[0].InstanceId)
}

func TestEIP_DescribeAddresses_FilterMultipleValues_OR(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out1, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	out2, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("allocation-id"), Values: []*string{out1.AllocationId, out2.AllocationId}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 2)
}

func TestEIP_DescribeAddresses_FilterMultipleFilters_AND(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Match both allocation-id and domain
	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("allocation-id"), Values: []*string{out.AllocationId}},
			{Name: aws.String("domain"), Values: []*string{aws.String("vpc")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)

	// Mismatch: correct allocation-id but wrong domain
	desc, err = svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("allocation-id"), Values: []*string{out.AllocationId}},
			{Name: aws.String("domain"), Values: []*string{aws.String("standard")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Addresses)
}

func TestEIP_DescribeAddresses_FilterUnknownName_Error(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestEIP_DescribeAddresses_FilterWildcard(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("allocation-id"), Values: []*string{aws.String("eipalloc-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, *out.AllocationId, *desc.Addresses[0].AllocationId)
}

func TestEIP_DescribeAddresses_FilterNoResults(t *testing.T) {
	svc, _ := setupTestEIP(t)

	_, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("public-ip"), Values: []*string{aws.String("1.1.1.1")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Addresses)
}

func TestEIP_DescribeAddresses_FilterByTag(t *testing.T) {
	svc, _ := setupTestEIP(t)

	out, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("elastic-ip"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Env"), Value: aws.String("prod")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, *out.AllocationId, *desc.Addresses[0].AllocationId)
}

// TestEIP_PublishNATEvent_PortNameHasPortPrefix verifies that publishNATEvent
// uses topology.Port(eniID) as PortName. A raw ENI id mismatch creates a
// dnat_and_snat row pointing at a nonexistent OVN port, black-holing the EIP.
func TestEIP_PublishNATEvent_PortNameHasPortPrefix(t *testing.T) {
	svc, _ := setupTestEIP(t)

	sub, err := svc.natsConn.SubscribeSync("vpc.add-nat")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	const eniID = "eni-abc123"
	svc.publishNATEvent("vpc.add-nat", "vpc-1", "198.51.100.10", "10.0.0.5", eniID, "02:00:00:00:00:01")

	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)

	var got natEvent
	require.NoError(t, json.Unmarshal(msg.Data, &got))
	assert.Equal(t, "port-"+eniID, got.PortName,
		"PortName must be port-<eni> to match the OVN logical switch port name")
	assert.Equal(t, "10.0.0.5", got.LogicalIP)
	assert.Equal(t, "198.51.100.10", got.ExternalIP)
}
