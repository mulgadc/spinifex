package handlers_ec2_vpc

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/nats-io/nats.go"
)

// Backward-compatible aliases so callers (daemon, EIP, EC2 handlers, tests)
// keep importing record + allocation shapes from this package while the
// allocator implementation lives in network/external.
type (
	ExternalIPAllocation = external.ExternalIPAllocation
	ExternalIPAMRecord   = external.PoolRecord
)

const (
	KVBucketExternalIPAM        = external.KVBucketStaticPool
	KVBucketExternalIPAMVersion = external.KVBucketStaticPoolVersion
)

// ExternalPoolConfig is the admin-defined pool from spinifex.toml.
// Duplicated against network/external.ExternalPoolConfig — the duplicate
// stays until ExternalIPAM moves into network/external (deferred L5
// cleanup tracked separately).
type ExternalPoolConfig struct {
	Name       string
	RangeStart string
	RangeEnd   string
	Gateway    string
	GatewayIP  string
	PrefixLen  int
	Region     string
	AZ         string
	// GwLrpRangeStart/End reserves a sub-range of the LAN for OVN gateway
	// LRP IPs in centralized NAT mode. IPAM must skip these addresses or
	// the per-VM EIP allocator and vpcd will fight over them.
	GwLrpRangeStart string
	GwLrpRangeEnd   string
}

// ExternalIPAM is the AWS-facing entry point for external IP allocation.
// State + CAS logic now lives in external.StaticPoolAllocator; this type
// is a thin facade that preserves the existing call surface (AllocateIP /
// AllocateFromPool / ReleaseIP / GetPoolRecord) for handlers and tests.
type ExternalIPAM struct {
	kv        nats.KeyValue
	pools     []ExternalPoolConfig
	allocator external.Allocator
}

// NewExternalIPAM creates a new ExternalIPAM backed by a static pool
// allocator on NATS JetStream KV.
func NewExternalIPAM(js nats.JetStreamContext, pools []ExternalPoolConfig) (*ExternalIPAM, error) {
	alloc, err := external.NewStaticPoolAllocator(js, toExternalPools(pools))
	if err != nil {
		return nil, err
	}
	return &ExternalIPAM{kv: alloc.KV(), pools: pools, allocator: alloc}, nil
}

// NewExternalIPAMWithKV creates an ExternalIPAM with an existing KV bucket (for testing).
func NewExternalIPAMWithKV(kv nats.KeyValue, pools []ExternalPoolConfig) *ExternalIPAM {
	alloc := external.NewStaticPoolAllocatorWithKV(kv, toExternalPools(pools))
	return &ExternalIPAM{kv: kv, pools: pools, allocator: alloc}
}

// AllocateIP allocates the next available external IP from the best pool
// matching the given region/AZ. Returns the allocated IP and pool name.
func (m *ExternalIPAM) AllocateIP(region, az, purpose, allocID, eniID, instanceID string) (string, string, error) {
	pool := m.findPool(region, az)
	if pool == nil {
		return "", "", fmt.Errorf("InsufficientAddressCapacity: no external pool available for region=%q az=%q", region, az)
	}
	ip, err := m.allocateFromPool(pool.Name, purpose, allocID, eniID, instanceID)
	if err != nil {
		return "", "", err
	}
	return ip, pool.Name, nil
}

// AllocateFromPool allocates an IP from a specific named pool.
func (m *ExternalIPAM) AllocateFromPool(poolName, purpose, allocID, eniID, instanceID string) (string, error) {
	return m.allocateFromPool(poolName, purpose, allocID, eniID, instanceID)
}

func (m *ExternalIPAM) allocateFromPool(poolName, purpose, allocID, eniID, instanceID string) (string, error) {
	addr, err := m.allocator.Allocate(context.Background(), external.AllocateRequest{
		PoolName:     poolName,
		Purpose:      purpose,
		AllocationID: allocID,
		ENIID:        eniID,
		InstanceID:   instanceID,
	})
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// ReleaseIP releases a previously allocated external IP back to its pool.
func (m *ExternalIPAM) ReleaseIP(poolName, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse release IP %q: %w", ip, err)
	}
	return m.allocator.Release(context.Background(), poolName, addr)
}

// GetPoolRecord returns the current IPAM record for a pool.
func (m *ExternalIPAM) GetPoolRecord(poolName string) (*ExternalIPAMRecord, error) {
	sp, ok := m.allocator.(*external.StaticPoolAllocator)
	if !ok {
		return nil, fmt.Errorf("pool record unavailable: allocator is not static")
	}
	return sp.GetPoolRecord(poolName)
}

// findPool returns the best pool for the given region/AZ using the same
// fallback order as topology.go: AZ-scoped → region-scoped → unscoped.
func (m *ExternalIPAM) findPool(region, az string) *ExternalPoolConfig {
	for i := range m.pools {
		p := &m.pools[i]
		if p.AZ != "" && p.AZ == az && p.Region == region {
			return p
		}
	}
	for i := range m.pools {
		p := &m.pools[i]
		if p.AZ == "" && p.Region != "" && p.Region == region {
			return p
		}
	}
	for i := range m.pools {
		p := &m.pools[i]
		if p.Region == "" && p.AZ == "" {
			return p
		}
	}
	return nil
}

// toExternalPools converts the handlers-side ExternalPoolConfig into the
// network/external mirror. The duplicate type goes away in the deferred
// L5 cleanup.
func toExternalPools(pools []ExternalPoolConfig) []external.ExternalPoolConfig {
	out := make([]external.ExternalPoolConfig, len(pools))
	for i, p := range pools {
		out[i] = external.ExternalPoolConfig{
			Name:            p.Name,
			RangeStart:      p.RangeStart,
			RangeEnd:        p.RangeEnd,
			Gateway:         p.Gateway,
			GatewayIP:       p.GatewayIP,
			PrefixLen:       p.PrefixLen,
			DNSServers:      nil,
			Region:          p.Region,
			AZ:              p.AZ,
			GwLrpRangeStart: p.GwLrpRangeStart,
			GwLrpRangeEnd:   p.GwLrpRangeEnd,
		}
	}
	return out
}

// ValidatePoolConfig checks that a pool config is valid.
func ValidatePoolConfig(pool ExternalPoolConfig) error {
	if pool.Name == "" {
		return fmt.Errorf("pool name is required")
	}
	if pool.Gateway == "" {
		return fmt.Errorf("gateway is required for pool %q", pool.Name)
	}
	if net.ParseIP(pool.Gateway) == nil {
		return fmt.Errorf("invalid gateway IP: %q", pool.Gateway)
	}
	if pool.GatewayIP != "" && net.ParseIP(pool.GatewayIP) == nil {
		return fmt.Errorf("invalid gateway_ip: %q", pool.GatewayIP)
	}
	startIP := net.ParseIP(pool.RangeStart)
	if startIP == nil {
		return fmt.Errorf("invalid range_start: %q", pool.RangeStart)
	}
	endIP := net.ParseIP(pool.RangeEnd)
	if endIP == nil {
		return fmt.Errorf("invalid range_end: %q", pool.RangeEnd)
	}
	if compareIPs(startIP, endIP) > 0 {
		return fmt.Errorf("range_start %s is greater than range_end %s", pool.RangeStart, pool.RangeEnd)
	}
	if pool.GwLrpRangeStart != "" || pool.GwLrpRangeEnd != "" {
		gwS := net.ParseIP(pool.GwLrpRangeStart)
		gwE := net.ParseIP(pool.GwLrpRangeEnd)
		if gwS == nil {
			return fmt.Errorf("invalid gw_lrp_range_start: %q", pool.GwLrpRangeStart)
		}
		if gwE == nil {
			return fmt.Errorf("invalid gw_lrp_range_end: %q", pool.GwLrpRangeEnd)
		}
		if compareIPs(gwS, gwE) > 0 {
			return fmt.Errorf("gw_lrp_range_start %s is greater than gw_lrp_range_end %s",
				pool.GwLrpRangeStart, pool.GwLrpRangeEnd)
		}
		// Overlap test: !(gwE < rangeS || gwS > rangeE)
		if compareIPs(gwE, startIP) >= 0 && compareIPs(gwS, endIP) <= 0 {
			return fmt.Errorf("gw_lrp_range %s-%s overlaps range %s-%s",
				pool.GwLrpRangeStart, pool.GwLrpRangeEnd, pool.RangeStart, pool.RangeEnd)
		}
	}
	return nil
}

func compareIPs(a, b net.IP) int {
	ai := ipToInt(a.To4())
	bi := ipToInt(b.To4())
	return ai.Cmp(bi)
}
