package handlers_ec2_vpc

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestExternalIPAM(t *testing.T, pools []ExternalPoolConfig) *ExternalIPAM {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	ipam, err := NewExternalIPAM(nc, js, pools)
	require.NoError(t, err)
	return ipam
}

func testPool() ExternalPoolConfig {
	return ExternalPoolConfig{
		Name:       "wan",
		RangeStart: "192.168.1.150",
		RangeEnd:   "192.168.1.160",
		Gateway:    "192.168.1.1",
		PrefixLen:  24,
	}
}

func TestExternalIPAM_AllocateSequential(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	// .150 is reserved for gateway, so first allocable is .151
	ip1, poolName, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip1)
	assert.Equal(t, "wan", poolName)

	ip2, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.152", ip2)

	ip3, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.153", ip3)
}

func TestExternalIPAM_GatewayIPReserved(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	record, err := ipam.GetPoolRecord("wan")
	require.NoError(t, err)

	// Gateway IP (.150) should be reserved
	alloc, ok := record.Allocated["192.168.1.150"]
	assert.True(t, ok, "gateway IP should be in allocated map")
	assert.Equal(t, "gateway", alloc.Type)

	// Cannot release gateway IP
	err = ipam.ReleaseIP("wan", "192.168.1.150")
	assert.ErrorContains(t, err, "cannot release gateway IP")
}

func TestExternalIPAM_ExplicitGatewayIP(t *testing.T) {
	pool := testPool()
	pool.GatewayIP = "192.168.1.155"
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	record, err := ipam.GetPoolRecord("wan")
	require.NoError(t, err)

	// Explicit gateway IP (.155) reserved, not .150
	_, ok := record.Allocated["192.168.1.155"]
	assert.True(t, ok)
	_, ok = record.Allocated["192.168.1.150"]
	assert.False(t, ok)

	// First allocable is .150 since .155 is the gateway
	ip1, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.150", ip1)
}

func TestExternalIPAM_Release(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	ip1, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip1)

	ip2, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.152", ip2)

	// Release first
	err = ipam.ReleaseIP("wan", "192.168.1.151")
	require.NoError(t, err)

	// Next allocation reuses released IP
	ip3, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip3)
}

func TestExternalIPAM_Exhaustion(t *testing.T) {
	pool := ExternalPoolConfig{
		Name:       "tiny",
		RangeStart: "10.0.0.1",
		RangeEnd:   "10.0.0.3",
		Gateway:    "10.0.0.254",
		PrefixLen:  24,
	}
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	// .1 reserved for gateway, .2 and .3 allocable
	ip1, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.2", ip1)

	ip2, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.3", ip2)

	// Pool exhausted
	_, _, err = ipam.AllocateIP("", "", "auto_assign", "", "eni-3", "i-3")
	assert.ErrorContains(t, err, "InsufficientAddressCapacity")
}

func TestExternalIPAM_CASConflict(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	// Concurrent allocations should all succeed (CAS retry handles conflicts)
	var wg sync.WaitGroup
	results := make([]string, 5)
	errs := make([]error, 5)

	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip, _, err := ipam.AllocateIP("", "", "auto_assign", "", "eni-"+itoa(idx), "i-"+itoa(idx))
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
	pools := []ExternalPoolConfig{
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
	ip1, poolName1, err := ipam.AllocateIP("us-east-1", "us-east-1a", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.3", ip1) // .2 reserved for gateway
	assert.Equal(t, "us-east", poolName1)

	// Allocate from EU pool
	ip2, poolName2, err := ipam.AllocateIP("eu-west-1", "eu-west-1a", "auto_assign", "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.3", ip2)
	assert.Equal(t, "eu-west", poolName2)
}

func TestExternalIPAM_PoolFallback(t *testing.T) {
	pools := []ExternalPoolConfig{
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
	ip1, pool1, err := ipam.AllocateIP("us-east-1", "us-east-1a", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "az-pool", pool1)
	assert.Equal(t, "10.0.0.2", ip1) // .1 reserved for gw

	// Region match (different AZ): us-east-1b → region-pool
	ip2, pool2, err := ipam.AllocateIP("us-east-1", "us-east-1b", "auto_assign", "", "eni-2", "i-2")
	require.NoError(t, err)
	assert.Equal(t, "region-pool", pool2)
	assert.Equal(t, "10.0.1.2", ip2)

	// No match: eu-west-1 → global-pool
	ip3, pool3, err := ipam.AllocateIP("eu-west-1", "eu-west-1a", "auto_assign", "", "eni-3", "i-3")
	require.NoError(t, err)
	assert.Equal(t, "global-pool", pool3)
	assert.Equal(t, "10.0.2.2", ip3)
}

func TestExternalIPAM_SpecificPool(t *testing.T) {
	pools := []ExternalPoolConfig{
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
	ip, err := ipam.AllocateFromPool("pool-b", "elastic_ip", "eipalloc-test-b", "", "")
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.2", ip) // .1 reserved for gw
}

func TestExternalIPAM_RangeValidation(t *testing.T) {
	tests := []struct {
		name    string
		pool    ExternalPoolConfig
		wantErr string
	}{
		{
			name:    "empty name",
			pool:    ExternalPoolConfig{RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
			wantErr: "pool name is required",
		},
		{
			name:    "bad range_start",
			pool:    ExternalPoolConfig{Name: "bad", RangeStart: "not-an-ip", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
			wantErr: "invalid range_start",
		},
		{
			name:    "bad range_end",
			pool:    ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "not-an-ip", Gateway: "10.0.0.254"},
			wantErr: "invalid range_end",
		},
		{
			name:    "start > end",
			pool:    ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.10", RangeEnd: "10.0.0.1", Gateway: "10.0.0.254"},
			wantErr: "greater than range_end",
		},
		{
			name:    "missing gateway",
			pool:    ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10"},
			wantErr: "gateway is required",
		},
		{
			name:    "bad gateway",
			pool:    ExternalPoolConfig{Name: "bad", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "nope"},
			wantErr: "invalid gateway IP",
		},
		{
			name:    "bad gateway_ip",
			pool:    ExternalPoolConfig{Name: "ok", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254", GatewayIP: "nope"},
			wantErr: "invalid gateway_ip",
		},
		{
			name: "valid",
			pool: ExternalPoolConfig{Name: "ok", RangeStart: "10.0.0.1", RangeEnd: "10.0.0.10", Gateway: "10.0.0.254"},
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
	_, nc, js := testutil.StartTestJetStream(t)

	// First init
	ipam1, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)

	// Allocate an IP
	ip, _, err := ipam1.AllocateIP("", "", "auto_assign", "", "eni-1", "i-1")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip)

	// Second init (simulating restart) — should not lose allocation
	ipam2, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)

	record, err := ipam2.GetPoolRecord("wan")
	require.NoError(t, err)
	assert.Contains(t, record.Allocated, "192.168.1.151")
	assert.Contains(t, record.Allocated, "192.168.1.150") // gateway still reserved
}

func TestExternalIPAM_ReleaseNotAllocated(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	err := ipam.ReleaseIP("wan", "192.168.1.200")
	assert.ErrorContains(t, err, "not allocated")
}

func TestExternalIPAM_NoPoolAvailable(t *testing.T) {
	pool := ExternalPoolConfig{
		Name:       "us-only",
		RangeStart: "10.0.0.1",
		RangeEnd:   "10.0.0.10",
		Gateway:    "10.0.0.254",
		PrefixLen:  24,
		Region:     "us-east-1",
		AZ:         "us-east-1a",
	}
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	// No pool matches eu-west
	_, _, err := ipam.AllocateIP("eu-west-1", "eu-west-1a", "auto_assign", "", "eni-1", "i-1")
	assert.ErrorContains(t, err, "InsufficientAddressCapacity")
}

func TestExternalIPAM_FindPoolByName_NotFound(t *testing.T) {
	pool := testPool()
	ipam := setupTestExternalIPAM(t, []ExternalPoolConfig{pool})

	// findPoolByName is private, but exercised via AllocateFromPool with unknown pool.
	// The function returns nil when no pool matches, which means static allocation path.
	// We verify by using NewExternalIPAMWithKV to directly check findPoolByName returns nil.
	kv := ipam.kv
	ipam2 := NewExternalIPAMWithKV(nil, kv, []ExternalPoolConfig{pool})
	// AllocateFromPool with a non-existent pool name: findPoolByName returns nil,
	// so pool.IsDHCP() is skipped and static allocation is used.
	// The pool "nonexistent" has no KV record, so getRecord fails.
	_, err := ipam2.AllocateFromPool("nonexistent", "auto_assign", "", "eni-1", "i-1")
	assert.Error(t, err)
}

func TestExternalPoolConfig_IsDHCP(t *testing.T) {
	p := ExternalPoolConfig{Source: "dhcp"}
	assert.True(t, p.IsDHCP())

	p2 := ExternalPoolConfig{Source: "static"}
	assert.False(t, p2.IsDHCP())

	p3 := ExternalPoolConfig{}
	assert.False(t, p3.IsDHCP())
}

func TestValidatePoolConfig_DHCPPool(t *testing.T) {
	// DHCP pools don't need range_start/range_end
	pool := ExternalPoolConfig{
		Name:    "dhcp-pool",
		Source:  "dhcp",
		Gateway: "192.168.1.1",
	}
	err := ValidatePoolConfig(pool)
	assert.NoError(t, err)
}

// dhcpStub captures acquire/release requests and replies with a
// deterministic lease so the handler-side IPAM code can be exercised
// without running vpcd.
type dhcpStub struct {
	mu             sync.Mutex
	acquired       []dhcp.AcquireRequestMsg
	released       []dhcp.ReleaseRequestMsg
	nextIP         string
	acquireErr     string
	acquireCalls   atomic.Int32
	releaseCalls   atomic.Int32
	serverID       string
	leaseSeconds   int64
	hwAddrOverride string
	releaseErrOnce atomic.Bool
}

func newDHCPStub(t *testing.T, nc *nats.Conn) *dhcpStub {
	t.Helper()
	s := &dhcpStub{
		nextIP:       "192.168.1.151",
		serverID:     "192.168.1.1",
		leaseSeconds: 3600,
	}
	acquire, err := nc.Subscribe(dhcp.TopicAcquire, func(msg *nats.Msg) {
		s.acquireCalls.Add(1)
		var req dhcp.AcquireRequestMsg
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respondTestJSON(t, msg, dhcp.AcquireReplyMsg{Error: "malformed request"})
			return
		}
		s.mu.Lock()
		s.acquired = append(s.acquired, req)
		ip := s.nextIP
		errMsg := s.acquireErr
		serverID := s.serverID
		expires := time.Now().Add(time.Duration(s.leaseSeconds) * time.Second).Unix()
		hw := s.hwAddrOverride
		s.mu.Unlock()

		if errMsg != "" {
			respondTestJSON(t, msg, dhcp.AcquireReplyMsg{Error: errMsg})
			return
		}
		if hw == "" {
			hw = "02:00:00:aa:bb:cc"
		}
		respondTestJSON(t, msg, dhcp.AcquireReplyMsg{
			IP:          ip,
			SubnetMask:  "255.255.255.0",
			Routers:     []string{"192.168.1.1"},
			DNS:         []string{"192.168.1.1"},
			ServerID:    serverID,
			HWAddr:      hw,
			ExpiresUnix: expires,
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = acquire.Unsubscribe() })

	release, err := nc.Subscribe(dhcp.TopicRelease, func(msg *nats.Msg) {
		s.releaseCalls.Add(1)
		var req dhcp.ReleaseRequestMsg
		_ = json.Unmarshal(msg.Data, &req)
		s.mu.Lock()
		s.released = append(s.released, req)
		s.mu.Unlock()
		reply := dhcp.ReleaseReplyMsg{}
		if s.releaseErrOnce.CompareAndSwap(true, false) {
			reply.Error = "first release fails"
		}
		respondTestJSON(t, msg, reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = release.Unsubscribe() })
	return s
}

func respondTestJSON(t *testing.T, msg *nats.Msg, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(data))
}

func TestExternalIPAM_DHCPAllocateAndRelease(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	stub := newDHCPStub(t, nc)
	stub.nextIP = "192.168.1.200"

	pool := ExternalPoolConfig{
		Name:           "wan-dhcp",
		Source:         "dhcp",
		Gateway:        "192.168.1.1",
		GatewayIP:      "192.168.1.1", // pre-set so initPool skips gateway DORA
		PrefixLen:      24,
		DhcpBindBridge: "br-wan",
	}
	ipam, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)

	ip, err := ipam.AllocateFromPool("wan-dhcp", "elastic_ip", "eipalloc-123", "eni-abc", "i-xyz")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.200", ip)
	require.EqualValues(t, 1, stub.acquireCalls.Load())

	stub.mu.Lock()
	got := stub.acquired[0]
	stub.mu.Unlock()
	assert.Equal(t, "eipalloc-123", got.ClientID, "clientID precedence: allocID first")
	assert.Equal(t, "eni-abc", got.Hostname, "hostname defaults to eniID")
	assert.Equal(t, "i-xyz", got.VendorClass, "vendor class i-<instanceID>")
	assert.Equal(t, "wan-dhcp", got.PoolName)
	assert.Equal(t, "br-wan", got.Bridge)

	record, err := ipam.GetPoolRecord("wan-dhcp")
	require.NoError(t, err)
	alloc, ok := record.Allocated["192.168.1.200"]
	require.True(t, ok)
	assert.Equal(t, "192.168.1.1", alloc.DHCPServerID)
	assert.NotZero(t, alloc.LeaseExpiresUnix)
	assert.Equal(t, "02:00:00:aa:bb:cc", alloc.HWAddr)

	require.NoError(t, ipam.ReleaseIP("wan-dhcp", "192.168.1.200"))
	require.EqualValues(t, 1, stub.releaseCalls.Load())
	stub.mu.Lock()
	assert.Equal(t, "eipalloc-123", stub.released[0].ClientID)
	stub.mu.Unlock()
}

func TestExternalIPAM_DHCPAcquireErrorPropagates(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	stub := newDHCPStub(t, nc)
	stub.acquireErr = "server unreachable"

	pool := ExternalPoolConfig{
		Name: "wan-dhcp-err", Source: "dhcp", Gateway: "192.168.1.1",
		GatewayIP: "192.168.1.1", PrefixLen: 24, DhcpBindBridge: "br-wan",
	}
	ipam, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)

	_, err = ipam.AllocateFromPool("wan-dhcp-err", "elastic_ip", "eipalloc-err", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server unreachable")
}

func TestExternalIPAM_DHCPGatewayFromLease(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	stub := newDHCPStub(t, nc)
	stub.nextIP = "192.168.3.247"

	pool := ExternalPoolConfig{
		Name: "wan-dhcp-gw", Source: "dhcp", Gateway: "192.168.3.1",
		PrefixLen: 24, DhcpBindBridge: "br-wan",
		// GatewayIP intentionally empty — initPool must pull it from DHCP.
	}
	_, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.acquireCalls.Load())

	stub.mu.Lock()
	got := stub.acquired[0]
	stub.mu.Unlock()
	assert.Equal(t, "gateway-wan-dhcp-gw", got.ClientID)
	assert.Equal(t, "mulga-spinifex-gw", got.VendorClass)
}

func TestObtainDHCPLease_RequestTimeout(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	// Stub never responds — verifies that a hung vpcd surfaces as a
	// wrapped NATS timeout error rather than hanging the caller.
	sub, err := nc.Subscribe(dhcp.TopicAcquire, func(*nats.Msg) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	prev := dhcpNATSTimeout
	dhcpNATSTimeout = 150 * time.Millisecond
	t.Cleanup(func() { dhcpNATSTimeout = prev })

	_, err = ObtainDHCPLease(nc, "br-wan", "eni-timeout", "eni-timeout", "mulga-spinifex", "wan")
	assert.ErrorContains(t, err, "dhcp acquire NATS request")
}

func TestReleaseDHCPLease_RequestTimeout(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe(dhcp.TopicRelease, func(*nats.Msg) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	prev := dhcpNATSTimeout
	dhcpNATSTimeout = 150 * time.Millisecond
	t.Cleanup(func() { dhcpNATSTimeout = prev })

	err = ReleaseDHCPLease(nc, "eni-timeout")
	assert.ErrorContains(t, err, "dhcp release NATS request")
}

func TestObtainDHCPLease_MalformedReply(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe(dhcp.TopicAcquire, func(msg *nats.Msg) {
		_ = msg.Respond([]byte("not json"))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = ObtainDHCPLease(nc, "br-wan", "eni-malformed", "eni-malformed", "mulga-spinifex", "wan")
	assert.ErrorContains(t, err, "unmarshal dhcp acquire reply")
}

func TestReleaseDHCPLease_MalformedReply(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe(dhcp.TopicRelease, func(msg *nats.Msg) {
		_ = msg.Respond([]byte("not json"))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = ReleaseDHCPLease(nc, "eni-malformed")
	assert.ErrorContains(t, err, "unmarshal dhcp release reply")
}

func TestReleaseDHCPLease_ReplyError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe(dhcp.TopicRelease, func(msg *nats.Msg) {
		data, _ := json.Marshal(dhcp.ReleaseReplyMsg{Error: "vpcd rejected release"})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = ReleaseDHCPLease(nc, "eni-rejected")
	assert.ErrorContains(t, err, "vpcd rejected release")
}

func TestObtainDHCPLease_Guards(t *testing.T) {
	_, err := ObtainDHCPLease(nil, "br-wan", "eni-1", "eni-1", "mulga-spinifex", "wan")
	assert.ErrorContains(t, err, "NATS connection is required")

	_, nc := testutil.StartTestNATS(t)
	_, err = ObtainDHCPLease(nc, "", "eni-1", "eni-1", "mulga-spinifex", "wan")
	assert.ErrorContains(t, err, "bridge name is required")

	_, err = ObtainDHCPLease(nc, "br-wan", "", "eni-1", "mulga-spinifex", "wan")
	assert.ErrorContains(t, err, "client ID is required")
}

func TestReleaseDHCPLease_NoopWhenNilOrEmpty(t *testing.T) {
	assert.NoError(t, ReleaseDHCPLease(nil, "eni-1"))
	_, nc := testutil.StartTestNATS(t)
	assert.NoError(t, ReleaseDHCPLease(nc, ""))
}

func TestExternalIPAM_DHCPGatewayErrorPropagates(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	stub := newDHCPStub(t, nc)
	stub.acquireErr = "no response from upstream"

	pool := ExternalPoolConfig{
		Name: "wan-gw-err", Source: "dhcp", Gateway: "192.168.1.1",
		PrefixLen: 24, DhcpBindBridge: "br-wan",
	}
	_, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no response from upstream")
}

func TestExternalIPAM_DHCPReleaseErrorIsNonFatal(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	stub := newDHCPStub(t, nc)
	stub.nextIP = "192.168.1.155"
	stub.releaseErrOnce.Store(true)

	pool := ExternalPoolConfig{
		Name: "wan-rel-err", Source: "dhcp", Gateway: "192.168.1.1",
		GatewayIP: "192.168.1.1", PrefixLen: 24, DhcpBindBridge: "br-wan",
	}
	ipam, err := NewExternalIPAM(nc, js, []ExternalPoolConfig{pool})
	require.NoError(t, err)

	ip, err := ipam.AllocateFromPool("wan-rel-err", "elastic_ip", "eipalloc-rel", "", "")
	require.NoError(t, err)

	// Release reports vpcd failure via slog.Warn but ReleaseIP itself
	// still succeeds — the allocation is removed from KV.
	require.NoError(t, ipam.ReleaseIP("wan-rel-err", ip))
	rec, err := ipam.GetPoolRecord("wan-rel-err")
	require.NoError(t, err)
	_, stillThere := rec.Allocated[ip]
	assert.False(t, stillThere)
}

func TestDHCPIdentityOptions(t *testing.T) {
	tests := []struct {
		name       string
		eniID      string
		instanceID string
		pool       string
		wantHost   string
		wantVendor string
	}{
		{"eni+instance", "eni-1", "i-abcd", "wan", "eni-1", "i-abcd"},
		{"instance only", "", "i-xyz", "wan", "spinifex-i-xyz", "i-xyz"},
		{"neither", "", "", "wan", "spinifex-wan", "mulga-spinifex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, vendor := dhcpIdentityOptions(tc.eniID, tc.instanceID, tc.pool)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantVendor, vendor)
		})
	}
}
