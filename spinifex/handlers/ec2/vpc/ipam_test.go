package handlers_ec2_vpc

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestIPAM(t *testing.T) *IPAM {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	ipam, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	return ipam
}

func TestIPAM_AllocateFirst(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip, err := ipam.AllocateIP(t.Context(), "subnet-1", "10.0.1.0/24", PurposeENIPrimary, "eni-1")
	require.NoError(t, err)
	// First allocable IP is .4 (skip .0 network, .1 gateway, .2 DNS, .3 reserved)
	assert.Equal(t, "10.0.1.4", ip)
}

func TestIPAM_AllocateSequential(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP(t.Context(), "subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.4", ip1)

	ip2, err := ipam.AllocateIP(t.Context(), "subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.5", ip2)

	ip3, err := ipam.AllocateIP(t.Context(), "subnet-seq", "10.0.2.0/24", PurposeENIPrimary, "eni-seq")
	require.NoError(t, err)
	assert.Equal(t, "10.0.2.6", ip3)
}

func TestIPAM_Release(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP(t.Context(), "subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.4", ip1)

	ip2, err := ipam.AllocateIP(t.Context(), "subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.5", ip2)

	// Release first IP
	err = ipam.ReleaseIP(t.Context(), "subnet-rel", "10.0.3.4")
	require.NoError(t, err)

	// Next allocation should reuse the released IP
	ip3, err := ipam.AllocateIP(t.Context(), "subnet-rel", "10.0.3.0/24", PurposeENIPrimary, "eni-rel")
	require.NoError(t, err)
	assert.Equal(t, "10.0.3.4", ip3)
}

func TestIPAM_ReleaseNotAllocated(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.AllocateIP(t.Context(), "subnet-rna", "10.0.4.0/24", PurposeENIPrimary, "eni-rna")
	require.NoError(t, err)

	err = ipam.ReleaseIP(t.Context(), "subnet-rna", "10.0.4.99")
	assert.ErrorContains(t, err, "not allocated")
}

func TestIPAM_ReleaseNoRecord(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	err := ipam.ReleaseIP(t.Context(), "subnet-nonexistent", "10.0.0.4")
	assert.Error(t, err)
}

func TestIPAM_Exhaustion(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	// /28 = 16 IPs total. Reserved: .0, .1, .2, .3, .15 = 5. Available: 11
	cidr := "10.0.5.0/28"
	subnetId := "subnet-exhaust"

	var allocated []string
	for i := range 11 {
		ip, err := ipam.AllocateIP(t.Context(), subnetId, cidr, PurposeENIPrimary, "eni-test")
		require.NoError(t, err, "allocation %d should succeed", i)
		allocated = append(allocated, ip)
	}

	// Verify the IPs are .4 through .14
	assert.Equal(t, "10.0.5.4", allocated[0])
	assert.Equal(t, "10.0.5.14", allocated[len(allocated)-1])

	// Next allocation should fail — subnet exhausted
	_, err := ipam.AllocateIP(t.Context(), subnetId, cidr, PurposeENIPrimary, "eni-test")
	assert.ErrorContains(t, err, "exhausted")
}

func TestIPAM_ReservedIPs(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	cidr := "10.0.6.0/24"
	subnetId := "subnet-reserved"

	// First 4 allocations should be .4, .5, .6, .7 (skipping .0-.3)
	for i := 4; i <= 7; i++ {
		ip, err := ipam.AllocateIP(t.Context(), subnetId, cidr, PurposeENIPrimary, "eni-test")
		require.NoError(t, err)
		expected := "10.0.6." + itoa(i)
		assert.Equal(t, expected, ip)
	}
}

func TestIPAM_AllocatedIPs(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	// No allocations yet
	ips, err := ipam.AllocatedIPs(t.Context(), "subnet-empty")
	require.NoError(t, err)
	assert.Nil(t, ips)

	// Allocate some IPs
	_, _ = ipam.AllocateIP(t.Context(), "subnet-list", "10.0.7.0/24", PurposeENIPrimary, "eni-list")
	_, _ = ipam.AllocateIP(t.Context(), "subnet-list", "10.0.7.0/24", PurposeENIPrimary, "eni-list")

	ips, err = ipam.AllocatedIPs(t.Context(), "subnet-list")
	require.NoError(t, err)
	assert.Len(t, ips, 2)
	assert.Equal(t, "10.0.7.4", ips[0].IP)
	assert.Equal(t, PurposeENIPrimary, ips[0].Purpose)
	assert.Equal(t, "eni-list", ips[0].OwnerID)
	assert.Equal(t, "10.0.7.5", ips[1].IP)
}

func TestIPAM_MultipleSubnets(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip1, err := ipam.AllocateIP(t.Context(), "subnet-a", "10.0.10.0/24", PurposeENIPrimary, "eni-a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.10.4", ip1)

	ip2, err := ipam.AllocateIP(t.Context(), "subnet-b", "10.0.20.0/24", PurposeENIPrimary, "eni-b")
	require.NoError(t, err)
	assert.Equal(t, "10.0.20.4", ip2)

	// Each subnet tracks independently
	ip3, err := ipam.AllocateIP(t.Context(), "subnet-a", "10.0.10.0/24", PurposeENIPrimary, "eni-a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.10.5", ip3)
}

func TestIPAM_LargerSubnet(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	// /20 subnet — first allocable is still .4
	ip, err := ipam.AllocateIP(t.Context(), "subnet-big", "172.16.0.0/20", PurposeENIPrimary, "eni-big")
	require.NoError(t, err)
	assert.Equal(t, "172.16.0.4", ip)
}

func TestIPAM_NewIPAMWithKV(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: "test-ipam-kv",
	})
	require.NoError(t, err)

	ipam := NewIPAMWithKV(kv)
	require.NotNil(t, ipam)

	// Should work the same as regular IPAM
	ip, err := ipam.AllocateIP(t.Context(), "subnet-kv", "10.0.1.0/24", PurposeENIPrimary, "eni-kv")
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.4", ip)
}

func TestIPAM_InvalidCIDR(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)
	_, err := ipam.AllocateIP(t.Context(), "subnet-bad", "not-a-cidr", PurposeENIPrimary, "eni-bad")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse CIDR")
}

func TestIPAM_ClaimIP_Success(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip, err := ipam.ClaimIP(t.Context(), "subnet-claim", "10.0.30.0/24", PurposeENIPrimary, "eni-claim", "10.0.30.50")
	require.NoError(t, err)
	assert.Equal(t, "10.0.30.50", ip)

	ips, err := ipam.AllocatedIPs(t.Context(), "subnet-claim")
	require.NoError(t, err)
	require.Len(t, ips, 1)
	assert.Equal(t, "10.0.30.50", ips[0].IP)
	assert.Equal(t, PurposeENIPrimary, ips[0].Purpose)
	assert.Equal(t, "eni-claim", ips[0].OwnerID)
}

func TestIPAM_ClaimIP_ThenSequentialAllocateSkipsIt(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.ClaimIP(t.Context(), "subnet-claim-seq", "10.0.31.0/24", PurposeENIPrimary, "eni-claim", "10.0.31.4")
	require.NoError(t, err)

	ip, err := ipam.AllocateIP(t.Context(), "subnet-claim-seq", "10.0.31.0/24", PurposeENIPrimary, "eni-next")
	require.NoError(t, err)
	assert.Equal(t, "10.0.31.5", ip)
}

func TestIPAM_ClaimIP_InvalidAddress(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.ClaimIP(t.Context(), "subnet-claim-bad", "10.0.32.0/24", PurposeENIPrimary, "eni-claim", "not-an-ip")
	assert.ErrorIs(t, err, ErrIPOutOfRange)
}

func TestIPAM_ClaimIP_OutsideSubnet(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.ClaimIP(t.Context(), "subnet-claim-oor", "10.0.33.0/24", PurposeENIPrimary, "eni-claim", "172.31.0.50")
	assert.ErrorIs(t, err, ErrIPOutOfRange)
}

func TestIPAM_ClaimIP_ReservedHead(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	for _, ip := range []string{"10.0.34.0", "10.0.34.1", "10.0.34.2", "10.0.34.3"} {
		_, err := ipam.ClaimIP(t.Context(), "subnet-claim-head", "10.0.34.0/24", PurposeENIPrimary, "eni-claim", ip)
		assert.ErrorIsf(t, err, ErrIPOutOfRange, "reserved address %s should be rejected", ip)
	}
}

func TestIPAM_ClaimIP_Broadcast(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.ClaimIP(t.Context(), "subnet-claim-bcast", "10.0.35.0/24", PurposeENIPrimary, "eni-claim", "10.0.35.255")
	assert.ErrorIs(t, err, ErrIPOutOfRange)
}

func TestIPAM_ClaimIP_AlreadyInUse(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	_, err := ipam.ClaimIP(t.Context(), "subnet-claim-inuse", "10.0.36.0/24", PurposeENIPrimary, "eni-first", "10.0.36.10")
	require.NoError(t, err)

	_, err = ipam.ClaimIP(t.Context(), "subnet-claim-inuse", "10.0.36.0/24", PurposeENIPrimary, "eni-second", "10.0.36.10")
	assert.ErrorIs(t, err, ErrIPInUse)
}

func TestIPAM_ClaimIP_AlreadyAllocatedByAllocateIP(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	ip, err := ipam.AllocateIP(t.Context(), "subnet-claim-vs-alloc", "10.0.37.0/24", PurposeENIPrimary, "eni-first")
	require.NoError(t, err)
	assert.Equal(t, "10.0.37.4", ip)

	_, err = ipam.ClaimIP(t.Context(), "subnet-claim-vs-alloc", "10.0.37.0/24", PurposeENIPrimary, "eni-second", "10.0.37.4")
	assert.ErrorIs(t, err, ErrIPInUse)
}

// TestIPAM_ClaimIP_ConcurrentRace proves the CAS loop, not a mock, rejects a
// racing claim: many goroutines race for the same address against one live
// JetStream KV bucket, and exactly one must win.
func TestIPAM_ClaimIP_ConcurrentRace(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	const subnetId = "subnet-claim-race"
	const cidr = "10.0.38.0/24"
	const contenders = 10

	var wg sync.WaitGroup
	results := make([]error, contenders)
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ipam.ClaimIP(t.Context(), subnetId, cidr, PurposeENIPrimary, "eni-race", "10.0.38.50")
			results[i] = err
		}(i)
	}
	wg.Wait()

	successes, inUseFailures := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrIPInUse):
			inUseFailures++
		default:
			t.Fatalf("unexpected error from concurrent claim: %v", err)
		}
	}

	assert.Equal(t, 1, successes, "exactly one concurrent claim should win the address")
	assert.Equal(t, contenders-1, inUseFailures, "every losing racer should see the address already in use")

	ips, err := ipam.AllocatedIPs(t.Context(), subnetId)
	require.NoError(t, err)
	require.Len(t, ips, 1, "the address must be recorded exactly once despite the race")
	assert.Equal(t, "10.0.38.50", ips[0].IP)
}

func TestCompareIPs_NilSortsFirst(t *testing.T) {
	t.Parallel()
	assert.Negative(t, compareIPs(nil, net.ParseIP("10.0.0.1")))
	assert.Zero(t, compareIPs(net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.1")))
	assert.Positive(t, compareIPs(net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.1")))
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// Concurrent allocations against one subnet must not hand the same address to
// two callers, which is what the CAS loop exists to prevent.
func TestIPAM_AllocateConcurrentNoDuplicates(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)

	const allocations = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ips  = make(map[string]bool, allocations)
		errs []error
	)
	for i := range allocations {
		wg.Go(func() {
			ip, err := ipam.AllocateIP(t.Context(), "subnet-1", "10.0.1.0/24",
				PurposeENIPrimary, "eni-"+strconv.Itoa(i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ips[ip] = true
		})
	}
	wg.Wait()

	require.Empty(t, errs)
	assert.Len(t, ips, allocations)

	entries, err := ipam.AllocatedIPs(t.Context(), "subnet-1")
	require.NoError(t, err)
	assert.Len(t, entries, allocations)
}
