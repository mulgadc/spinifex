package handlers_ec2_vpc

import (
	"strconv"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestIPAM(t *testing.T) *IPAM {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	ipam, err := NewIPAM(js)
	require.NoError(t, err)
	return ipam
}

func TestIPAM_AllocateFirst(t *testing.T) {
	ipam := setupTestIPAM(t)

	ip, err := ipam.AllocateIP("subnet-1", "10.0.1.0/24", PurposeENIPrimary, "eni-1")
	require.NoError(t, err)
	// First allocable IP is .4 (skip .0 network, .1 gateway, .2 DNS, .3 reserved)
	assert.Equal(t, "10.0.1.4", ip)
}

func TestIPAM_AllocateSequential(t *testing.T) {
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP("subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.4", ip1)

	ip2, err := ipam.AllocateIP("subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.5", ip2)

	ip3, err := ipam.AllocateIP("subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.6", ip3)
}

func TestIPAM_Release(t *testing.T) {
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP("subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.4", ip1)

	ip2, err := ipam.AllocateIP("subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.5", ip2)

	// Release first IP
	err = ipam.ReleaseIP("subnet-rel", "10.0.3.4")
	require.NoError(t, err)

	// Next allocation should reuse the released IP
	ip3, err := ipam.AllocateIP("subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.4", ip3)
}

func TestIPAM_ReleaseNotAllocated(t *testing.T) {
	ipam := setupTestIPAM(t)

	_, err := ipam.AllocateIP("subnet-rna", "10.0.4.0/24", PurposeENIPrimary, "eni-rna")
	require.NoError(t, err)

	err = ipam.ReleaseIP("subnet-rna", "10.0.4.99")
	assert.ErrorContains(t, err, "not allocated")
}

func TestIPAM_ReleaseNoRecord(t *testing.T) {
	ipam := setupTestIPAM(t)

	err := ipam.ReleaseIP("subnet-nonexistent", "10.0.0.4")
	assert.Error(t, err)
}

func TestIPAM_Exhaustion(t *testing.T) {
	ipam := setupTestIPAM(t)

	// /28 = 16 IPs total. Reserved: .0, .1, .2, .3, .15 = 5. Available: 11
	cidr := "10.0.5.0/28"
	subnetId := "subnet-exhaust"

	var allocated []string
	for i := range 11 {
		ip, err := ipam.AllocateIP(subnetId, cidr, PurposeENIPrimary, "eni-test")
		require.NoError(t, err, "allocation %d should succeed", i)
		allocated = append(allocated, ip)
	}

	// Verify the IPs are .4 through .14
	assert.Equal(t, "10.0.5.4", allocated[0])
	assert.Equal(t, "10.0.5.14", allocated[len(allocated)-1])

	// Next allocation should fail — subnet exhausted
	_, err := ipam.AllocateIP(subnetId, cidr, PurposeENIPrimary, "eni-test")
	assert.ErrorContains(t, err, "exhausted")
}

func TestIPAM_ReservedIPs(t *testing.T) {
	ipam := setupTestIPAM(t)

	cidr := "10.0.6.0/24"
	subnetId := "subnet-reserved"

	// First 4 allocations should be .4, .5, .6, .7 (skipping .0-.3)
	for i := 4; i <= 7; i++ {
		ip, err := ipam.AllocateIP(subnetId, cidr, PurposeENIPrimary, "eni-test")
		require.NoError(t, err)
		expected := "10.0.6." + itoa(i)
		assert.Equal(t, expected, ip)
	}
}

func TestIPAM_AllocatedIPs(t *testing.T) {
	ipam := setupTestIPAM(t)

	// No allocations yet
	ips, err := ipam.AllocatedIPs("subnet-empty")
	require.NoError(t, err)
	assert.Nil(t, ips)

	// Allocate some IPs
	_, _ = ipam.AllocateIP("subnet-list", "10.0.7.0/24", PurposeENIPrimary, "eni-list")
	_, _ = ipam.AllocateIP("subnet-list", "10.0.7.0/24", PurposeENIPrimary, "eni-list")

	ips, err = ipam.AllocatedIPs("subnet-list")
	require.NoError(t, err)
	assert.Len(t, ips, 2)
	assert.Equal(t, "10.0.7.4", ips[0].IP)
	assert.Equal(t, PurposeENIPrimary, ips[0].Purpose)
	assert.Equal(t, "eni-list", ips[0].OwnerID)
	assert.Equal(t, "10.0.7.5", ips[1].IP)
}

func TestIPAM_MultipleSubnets(t *testing.T) {
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP("subnet-a", "10.0.10.0/24", PurposeENIPrimary, "eni-a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.10.4", ip1)

	ip2, err := ipam.AllocateIP("subnet-b", "10.0.20.0/24", PurposeENIPrimary, "eni-b")
	require.NoError(t, err)
	assert.Equal(t, "10.0.20.4", ip2)

	// Each subnet tracks independently
	ip3, err := ipam.AllocateIP("subnet-a", "10.0.10.0/24", PurposeENIPrimary, "eni-a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.10.5", ip3)
}

func TestIPAM_LargerSubnet(t *testing.T) {
	ipam := setupTestIPAM(t)

	// /20 subnet — first allocable is still .4
	ip, err := ipam.AllocateIP("subnet-big", "172.16.0.0/20", PurposeENIPrimary, "eni-big")
	require.NoError(t, err)
	assert.Equal(t, "172.16.0.4", ip)
}

func TestIPAM_NewIPAMWithKV(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: "test-ipam-kv",
	})
	require.NoError(t, err)

	ipam := NewIPAMWithKV(kv)
	require.NotNil(t, ipam)

	// Should work the same as regular IPAM
	ip, err := ipam.AllocateIP("subnet-kv", "10.0.1.0/24", PurposeENIPrimary, "eni-kv")
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.4", ip)
}

func TestIPAM_InvalidCIDR(t *testing.T) {
	ipam := setupTestIPAM(t)
	_, err := ipam.AllocateIP("subnet-bad", "not-a-cidr", PurposeENIPrimary, "eni-bad")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse CIDR")
}

func TestIPAM_IpToIntNil(t *testing.T) {
	assert.Equal(t, uint32(0), ipToInt(nil))
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
