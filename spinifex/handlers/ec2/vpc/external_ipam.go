package handlers_ec2_vpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
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

// ExternalPoolConfig is the admin-defined IP pool from spinifex.toml.
// Duplicated from network/external.ExternalPoolConfig pending consolidation.
type ExternalPoolConfig struct {
	Name       string
	Source     string // "static" (default) or "dhcp"
	BindBridge string // Linux bridge for DHCP DORA (source=dhcp only)
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

// ExternalIPAM is the AWS-facing entry point for external IP allocation,
// dispatching to StaticPoolAllocator or dhcp.DHCPPoolAllocator per pool name.
type ExternalIPAM struct {
	kv      nats.KeyValue
	pools   []ExternalPoolConfig
	static  *external.StaticPoolAllocator
	perPool map[string]external.Allocator // dhcp overrides; static pools fall through to `static`
}

// NewExternalIPAM creates a new ExternalIPAM. Static pools wire through
// external.StaticPoolAllocator; DHCP-sourced pools wait for EnableDHCP
// to install the per-pool dhcp.DHCPPoolAllocator.
func NewExternalIPAM(js nats.JetStreamContext, pools []ExternalPoolConfig) (*ExternalIPAM, error) {
	staticPools := filterStatic(pools)
	var (
		alloc *external.StaticPoolAllocator
		kv    nats.KeyValue
	)
	if len(staticPools) > 0 {
		var err error
		alloc, err = external.NewStaticPoolAllocator(js, toExternalPools(staticPools))
		if err != nil {
			return nil, err
		}
		kv = alloc.KV()
	}
	return &ExternalIPAM{kv: kv, pools: pools, static: alloc, perPool: map[string]external.Allocator{}}, nil
}

// NewExternalIPAMWithKV creates an ExternalIPAM with an existing KV bucket (for testing).
func NewExternalIPAMWithKV(kv nats.KeyValue, pools []ExternalPoolConfig) *ExternalIPAM {
	alloc := external.NewStaticPoolAllocatorWithKV(kv, toExternalPools(filterStatic(pools)))
	return &ExternalIPAM{kv: kv, pools: pools, static: alloc, perPool: map[string]external.Allocator{}}
}

// EnableDHCP installs a DHCPPoolAllocator for every pool with
// Source="dhcp". client is the daemon-side NATS wrapper that fans out
// to vpcd. Idempotent — repeated calls overwrite existing dhcp entries.
func (m *ExternalIPAM) EnableDHCP(client *dhcp.NATSClient) error {
	if client == nil {
		return errors.New("ExternalIPAM EnableDHCP: nil NATSClient")
	}
	for _, p := range m.pools {
		if p.Source != external.SourceDHCP {
			continue
		}
		m.perPool[p.Name] = dhcp.NewDHCPPoolAllocator(client, toExternalPools([]ExternalPoolConfig{p})[0])
	}
	return nil
}

// AllocateIP allocates the next available external IP from the best pool
// matching the given region/AZ. Returns the allocated IP and pool name.
func (m *ExternalIPAM) AllocateIP(region, az, purpose, allocID, eniID, instanceID string) (string, string, error) {
	pool := m.findPool(region, az)
	if pool == nil {
		return "", "", fmt.Errorf("InsufficientAddressCapacity: no external pool available for region=%q az=%q", region, az)
	}
	ip, err := m.AllocateFromPool(pool.Name, purpose, allocID, eniID, instanceID)
	if err != nil {
		return "", "", err
	}
	return ip, pool.Name, nil
}

// AllocateFromPool allocates an IP from a specific named pool.
func (m *ExternalIPAM) AllocateFromPool(poolName, purpose, allocID, eniID, instanceID string) (string, error) {
	alloc, err := m.allocatorFor(poolName)
	if err != nil {
		return "", err
	}
	addr, err := alloc.Allocate(context.Background(), external.AllocateRequest{
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
// ownerENIID, when non-empty, scopes the release to the ENI that currently owns
// the lease so a stale or duplicated teardown for a recycled IP is a no-op.
func (m *ExternalIPAM) ReleaseIP(poolName, ip, ownerENIID string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse release IP %q: %w", ip, err)
	}
	alloc, err := m.allocatorFor(poolName)
	if err != nil {
		return err
	}
	return alloc.Release(context.Background(), poolName, addr, ownerENIID)
}

// GetPoolRecord returns the current IPAM record for a pool. DHCP-sourced
// pools have no static record — the per-AZ lease bucket is authoritative.
func (m *ExternalIPAM) GetPoolRecord(poolName string) (*ExternalIPAMRecord, error) {
	if m.static == nil {
		return nil, fmt.Errorf("pool record unavailable: no static allocator")
	}
	return m.static.GetPoolRecord(poolName)
}

func (m *ExternalIPAM) allocatorFor(poolName string) (external.Allocator, error) {
	if a, ok := m.perPool[poolName]; ok {
		return a, nil
	}
	if m.static == nil {
		return nil, fmt.Errorf("no allocator for pool %q (static disabled, dhcp not enabled)", poolName)
	}
	return m.static, nil
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
			Source:          p.Source,
			BindBridge:      p.BindBridge,
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

// filterStatic returns only the static pools (Source unset or "static").
func filterStatic(pools []ExternalPoolConfig) []ExternalPoolConfig {
	out := make([]ExternalPoolConfig, 0, len(pools))
	for _, p := range pools {
		if p.Source == "" || p.Source == external.SourceStatic {
			out = append(out, p)
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
