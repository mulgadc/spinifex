package handlers_ec2_vpc

import (
	"context"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestExternalIPAM(t *testing.T, pools []external.ExternalPoolConfig) *ExternalIPAM {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	ipam, err := NewExternalIPAM(js, pools)
	require.NoError(t, err)
	return ipam
}

func testPool() external.ExternalPoolConfig {
	return external.ExternalPoolConfig{
		Name:       "wan",
		RangeStart: "192.168.1.150",
		RangeEnd:   "192.168.1.160",
		Gateway:    "192.168.1.1",
		PrefixLen:  24,
	}
}

func TestExternalIPAM_AllocateSequential(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	// .150 is reserved for gateway, so first allocable is .151
	ip1, poolName, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip1)
	assert.Equal(t, "wan", poolName)

	ip2, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.152", ip2)

	ip3, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.153", ip3)
}

func TestExternalIPAM_GatewayIPReserved(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	record, err := ipam.GetPoolRecord("wan")
	require.NoError(t, err)

	// Gateway IP (.150) should be reserved
	alloc, ok := record.Allocated["192.168.1.150"]
	assert.True(t, ok, "gateway IP should be in allocated map")
	assert.Equal(t, PurposeIGWLRP, alloc.Purpose)

	// Cannot release gateway IP
	err = ipam.ReleaseIP(context.Background(), "wan", "192.168.1.150", "")
	assert.ErrorContains(t, err, "cannot release gateway IP")
}

func TestExternalIPAM_ExplicitGatewayIP(t *testing.T) {
	pool := testPool()
	pool.GatewayIP = "192.168.1.155"
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	record, err := ipam.GetPoolRecord("wan")
	require.NoError(t, err)

	// Explicit gateway IP (.155) reserved, not .150
	_, ok := record.Allocated["192.168.1.155"]
	assert.True(t, ok)
	_, ok = record.Allocated["192.168.1.150"]
	assert.False(t, ok)

	// First allocable is .150 since .155 is the gateway
	ip1, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.150", ip1)
}

func TestExternalIPAM_Release(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	ip1, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip1)

	ip2, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.152", ip2)

	// Release first
	err = ipam.ReleaseIP(context.Background(), "wan", "192.168.1.151", "")
	require.NoError(t, err)

	// Round-robin: a released IP is NOT reused immediately. Allocation resumes
	// past the cursor, so the next IP is .153, not the freed .151.
	ip3, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.153", ip3)
}

func TestExternalIPAM_Exhaustion(t *testing.T) {
	pool := external.ExternalPoolConfig{
		Name:       "tiny",
		RangeStart: "10.0.0.1",
		RangeEnd:   "10.0.0.3",
		Gateway:    "10.0.0.254",
		PrefixLen:  24,
	}
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	// .1 reserved for gateway, .2 and .3 allocable
	ip1, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.2", ip1)

	ip2, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.3", ip2)

	// Pool exhausted
	_, _, err = ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-3", "i-3")
	assert.ErrorContains(t, err, "InsufficientAddressCapacity")
}

func TestExternalIPAM_CASConflict(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	// Concurrent allocations should all succeed (CAS retry handles conflicts)
	var wg sync.WaitGroup
	results := make([]string, 5)
	errs := make([]error, 5)

	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip, _, err := ipam.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-"+itoa(idx), "i-"+itoa(idx))
			results[idx] = ip
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// All should succeed
	for i, err := range errs {
		assert.NoError(t, err, "concurrent allocation %d should succeed", i)
	}

	// All IPs should be unique
	seen := make(map[string]bool)
	for _, ip := range results {
		assert.False(t, seen[ip], "duplicate IP: %s", ip)
		seen[ip] = true
	}
}

func TestExternalIPAM_MultiPool(t *testing.T) {
	pools := []external.ExternalPoolConfig{
		{
			Name:       "us-east",
			RangeStart: "203.0.113.2",
			RangeEnd:   "203.0.113.10",
			Gateway:    "203.0.113.1",
			PrefixLen:  24,
			Region:     "us-east-1",
			AZ:         "us-east-1a",
		},
		{
			Name:       "eu-west",
			RangeStart: "198.51.100.2",
			RangeEnd:   "198.51.100.10",
			Gateway:    "198.51.100.1",
			PrefixLen:  24,
			Region:     "eu-west-1",
			AZ:         "eu-west-1a",
		},
	}
	ipam := setupTestExternalIPAM(t, pools)

	// Allocate from US pool
	ip1, poolName1, err := ipam.AllocateIP(context.Background(), "us-east-1", "us-east-1a", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.3", ip1) // .2 reserved for gateway
	assert.Equal(t, "us-east", poolName1)

	// Allocate from EU pool
	ip2, poolName2, err := ipam.AllocateIP(context.Background(), "eu-west-1", "eu-west-1a", PurposeENIPublic, "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.3", ip2)
	assert.Equal(t, "eu-west", poolName2)
}

func TestExternalIPAM_PoolFallback(t *testing.T) {
	pools := []external.ExternalPoolConfig{
		{
			Name:       "az-pool",
			RangeStart: "10.0.0.1",
			RangeEnd:   "10.0.0.2",
			Gateway:    "10.0.0.254",
			PrefixLen:  24,
			Region:     "us-east-1",
			AZ:         "us-east-1a",
		},
		{
			Name:       "region-pool",
			RangeStart: "10.0.1.1",
			RangeEnd:   "10.0.1.10",
			Gateway:    "10.0.1.254",
			PrefixLen:  24,
			Region:     "us-east-1",
		},
		{
			Name:       "global-pool",
			RangeStart: "10.0.2.1",
			RangeEnd:   "10.0.2.10",
			Gateway:    "10.0.2.254",
			PrefixLen:  24,
		},
	}
	ipam := setupTestExternalIPAM(t, pools)

	// AZ match: us-east-1a → az-pool
	ip1, pool1, err := ipam.AllocateIP(context.Background(), "us-east-1", "us-east-1a", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "az-pool", pool1)
	assert.Equal(t, "10.0.0.2", ip1) // .1 reserved for gw

	// Region match (different AZ): us-east-1b → region-pool
	ip2, pool2, err := ipam.AllocateIP(context.Background(), "us-east-1", "us-east-1b", PurposeENIPublic, "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "region-pool", pool2)
	assert.Equal(t, "10.0.1.2", ip2)

	// No match: eu-west-1 → global-pool
	ip3, pool3, err := ipam.AllocateIP(context.Background(), "eu-west-1", "eu-west-1a", PurposeENIPublic, "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "global-pool", pool3)
	assert.Equal(t, "10.0.2.2", ip3)
}

func TestExternalIPAM_SpecificPool(t *testing.T) {
	pools := []external.ExternalPoolConfig{
		{
			Name:       "pool-a",
			RangeStart: "10.0.0.1",
			RangeEnd:   "10.0.0.10",
			Gateway:    "10.0.0.254",
			PrefixLen:  24,
		},
		{
			Name:       "pool-b",
			RangeStart: "10.0.1.1",
			RangeEnd:   "10.0.1.10",
			Gateway:    "10.0.1.254",
			PrefixLen:  24,
		},
	}
	ipam := setupTestExternalIPAM(t, pools)

	// Allocate specifically from pool-b
	ip, err := ipam.AllocateFromPool(context.Background(), "pool-b", PurposeEIP, "eipalloc-test-b", "", "")
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.2", ip) // .1 reserved for gw
}

func TestExternalIPAM_RangeValidation(t *testing.T) {
	tests := []struct {
		name    string
		pool    external.ExternalPoolConfig
		wantErr string
	}{
		{
			name:    "empty name",
			pool:    external.ExternalPoolConfig{RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
			wantErr: "pool name is required",
		},
		{
			name:    "bad range_start",
			pool:    external.ExternalPoolConfig{Name: "bad", RangeStart: "not-an-ip", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
			wantErr: "invalid range_start",
		},
		{
			name:    "bad range_end",
			pool:    external.ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "not-an-ip", Gateway: "10.0.0.254"},
			wantErr: "invalid range_end",
		},
		{
			name:    "start > end",
			pool:    external.ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.10", RangeEnd: "10.0.0.1", Gateway: "10.0.0.254"},
			wantErr: "greater than range_end",
		},
		{
			name:    "missing gateway",
			pool:    external.ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10"},
			wantErr: "gateway is required",
		},
		{
			name:    "bad gateway",
			pool:    external.ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "nope"},
			wantErr: "invalid gateway IP",
		},
		{
			name:    "bad gateway_ip",
			pool:    external.ExternalPoolConfig{Name: "ok", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254", GatewayIP: "nope"},
			wantErr: "invalid gateway_ip",
		},
		{
			name: "valid",
			pool: external.ExternalPoolConfig{Name: "ok", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePoolConfig(tt.pool)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExternalIPAM_InitFromConfig(t *testing.T) {
	pool := testPool()
	// Create IPAM twice — second init should be idempotent
	_, _, js := testutil.StartTestJetStream(t)

	// First init
	ipam1, err := NewExternalIPAM(js, []external.ExternalPoolConfig{pool})
	require.NoError(t, err)

	// Allocate an IP
	ip, _, err := ipam1.AllocateIP(context.Background(), "", "", PurposeENIPublic, "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip)

	// Second init (simulating restart) — should not lose allocation
	ipam2, err := NewExternalIPAM(js, []external.ExternalPoolConfig{pool})
	require.NoError(t, err)

	record, err := ipam2.GetPoolRecord("wan")
	require.NoError(t, err)
	assert.Contains(t, record.Allocated, "192.168.1.151")
	assert.Contains(t, record.Allocated, "192.168.1.150") // gateway still reserved
}

func TestExternalIPAM_ReleaseNotAllocated(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	err := ipam.ReleaseIP(context.Background(), "wan", "192.168.1.200", "")
	assert.ErrorContains(t, err, "not allocated")
}

func TestExternalIPAM_NoPoolAvailable(t *testing.T) {
	pool := external.ExternalPoolConfig{
		Name:       "us-only",
		RangeStart: "10.0.0.1",
		RangeEnd:   "10.0.0.10",
		Gateway:    "10.0.0.254",
		PrefixLen:  24,
		Region:     "us-east-1",
		AZ:         "us-east-1a",
	}
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	// No pool matches eu-west
	_, _, err := ipam.AllocateIP(context.Background(), "eu-west-1", "eu-west-1a", PurposeENIPublic, "", "eni-1", "i-1")
	assert.ErrorContains(t, err, `region="eu-west-1" az="eu-west-1a"`)
	code, ok := awserrors.ResolveErrorCode(err)
	assert.True(t, ok)
	assert.Equal(t, awserrors.ErrorInsufficientAddressCapacity, code)
}

func TestExternalIPAM_FindPoolByName_NotFound(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []external.ExternalPoolConfig{pool})

	// AllocateFromPool with a non-existent pool name: the pool "nonexistent"
	// has no KV record, so getRecord fails.
	kv := ipam.kv
	ipam2 := NewExternalIPAMWithKV(kv, []external.ExternalPoolConfig{pool})
	_, err := ipam2.AllocateFromPool(context.Background(), "nonexistent", PurposeENIPublic, "", "eni-1", "i-1")
	assert.Error(t, err)
}

func TestValidatePoolConfig(t *testing.T) {
	base := func() external.ExternalPoolConfig {
		return external.ExternalPoolConfig{
			Name:       "wan",
			Gateway:    "192.168.1.1",
			RangeStart: "192.168.1.100",
			RangeEnd:   "192.168.1.110",
			PrefixLen:  24,
		}
	}
	cases := []struct {
		name    string
		mutate  func(*external.ExternalPoolConfig)
		wantErr string
	}{
		{name: "ok", mutate: func(p *external.ExternalPoolConfig) {}},
		{name: "no name", mutate: func(p *external.ExternalPoolConfig) { p.Name = "" }, wantErr: "pool name is required"},
		{name: "no gateway", mutate: func(p *external.ExternalPoolConfig) { p.Gateway = "" }, wantErr: "gateway is required"},
		{name: "bad gateway", mutate: func(p *external.ExternalPoolConfig) { p.Gateway = "x" }, wantErr: "invalid gateway IP"},
		{name: "bad gateway_ip", mutate: func(p *external.ExternalPoolConfig) { p.GatewayIP = "x" }, wantErr: "invalid gateway_ip"},
		{name: "bad range_start", mutate: func(p *external.ExternalPoolConfig) { p.RangeStart = "x" }, wantErr: "invalid range_start"},
		{name: "bad range_end", mutate: func(p *external.ExternalPoolConfig) { p.RangeEnd = "x" }, wantErr: "invalid range_end"},
		{name: "range reversed", mutate: func(p *external.ExternalPoolConfig) {
			p.RangeStart = "192.168.1.200"
			p.RangeEnd = "192.168.1.100"
		}, wantErr: "greater than range_end"},
		{name: "bad gw_lrp_start", mutate: func(p *external.ExternalPoolConfig) {
			p.GwLrpRangeStart = "x"
			p.GwLrpRangeEnd = "192.168.1.20"
		}, wantErr: "invalid gw_lrp_range_start"},
		{name: "bad gw_lrp_end", mutate: func(p *external.ExternalPoolConfig) {
			p.GwLrpRangeStart = "192.168.1.20"
			p.GwLrpRangeEnd = "x"
		}, wantErr: "invalid gw_lrp_range_end"},
		{name: "gw_lrp reversed", mutate: func(p *external.ExternalPoolConfig) {
			p.GwLrpRangeStart = "192.168.1.30"
			p.GwLrpRangeEnd = "192.168.1.20"
		}, wantErr: "greater than gw_lrp_range_end"},
		{name: "gw_lrp overlaps range", mutate: func(p *external.ExternalPoolConfig) {
			p.GwLrpRangeStart = "192.168.1.105"
			p.GwLrpRangeEnd = "192.168.1.108"
		}, wantErr: "overlaps range"},
		{name: "gw_lrp valid below range", mutate: func(p *external.ExternalPoolConfig) {
			p.GwLrpRangeStart = "192.168.1.20"
			p.GwLrpRangeEnd = "192.168.1.29"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			err := ValidatePoolConfig(p)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
